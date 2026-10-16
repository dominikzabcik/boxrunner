package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/nflog"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

// fakeHost plays nft and iptables: it keeps what nft was told to build and
// lists it back as nft would, and records every command.
type fakeHost struct {
	mu       sync.Mutex
	calls    []string
	scripts  []string
	elements []string
	// noNft makes nft fail, as a box without it does.
	noNft bool
	// flushed makes nft forget the table, as `nft flush ruleset` does.
	flushed bool
	// failApply makes nft refuse a ruleset.
	failApply bool
	chains    map[string]bool
	sets      map[string]bool
	counters  map[string]bool
	jumps     []string
}

var (
	addObject = regexp.MustCompile(`(?m)^add (chain|set|counter) inet boxrunner (\S+)`)
	addJump   = regexp.MustCompile(`(?m)^add rule inet boxrunner (forward|input) (iifname "\S+" jump \S+)$`)
)

func (f *fakeHost) run(stdin, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if name != "nft" {
		if strings.Contains(cmd, " -S ") {
			return "", errors.New("exit status 1")
		}
		return "", nil
	}
	switch {
	case f.noNft:
		return "", errors.New("exec: nft: executable file not found in $PATH")
	case cmd == "nft --version":
		return "nftables v1.0.9", nil
	case cmd == "nft list table inet boxrunner":
		if f.flushed || f.chains == nil {
			return "Error: No such file or directory", errors.New("exit status 1")
		}
		var b strings.Builder
		b.WriteString("table inet boxrunner {\n")
		for name := range f.counters {
			fmt.Fprintf(&b, "\tcounter %s {\n\t\tpackets 0 bytes 0\n\t}\n", name)
		}
		for name := range f.sets {
			fmt.Fprintf(&b, "\tset %s {\n\t}\n", name)
		}
		for name := range f.chains {
			fmt.Fprintf(&b, "\tchain %s {\n\t}\n", name)
		}
		for _, j := range f.jumps {
			fmt.Fprintf(&b, "\t%s\n", j)
		}
		b.WriteString("}\n")
		return b.String(), nil
	case cmd == "nft delete table inet boxrunner":
		f.chains, f.sets, f.counters, f.jumps = nil, nil, nil, nil
	case cmd == "nft list counters table inet boxrunner":
		return "table inet boxrunner {\n}\n", nil
	case cmd == "nft -f -":
		if strings.HasPrefix(stdin, "add element") || strings.HasPrefix(stdin, "delete element") {
			f.elements = append(f.elements, strings.TrimSpace(stdin))
			return "", nil
		}
		if f.failApply {
			return "Error: could not process rule", errors.New("exit status 1")
		}
		f.scripts = append(f.scripts, stdin)
		f.flushed = false
		f.chains, f.sets, f.counters, f.jumps = map[string]bool{"forward": true, "input": true}, map[string]bool{}, map[string]bool{}, nil
		for _, m := range addObject.FindAllStringSubmatch(stdin, -1) {
			map[string]map[string]bool{"chain": f.chains, "set": f.sets, "counter": f.counters}[m[1]][m[2]] = true
		}
		for _, m := range addJump.FindAllStringSubmatch(stdin, -1) {
			f.jumps = append(f.jumps, m[2])
		}
	}
	return "", nil
}

func (f *fakeHost) applied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.scripts)
}

func (f *fakeHost) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// answers is a resolver that gives every allowed name one address.
type answers struct{ addr string }

func (a answers) Exchange(_ context.Context, query []byte, _ bool) ([]byte, error) {
	resp := append([]byte{}, query...)
	resp[2], resp[3] = 0x81, 0x80
	binary.BigEndian.PutUint16(resp[6:], 1)
	resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
	return append(resp, netip.MustParseAddr(a.addr).AsSlice()...), nil
}

// packets is the egress rules' log, which the test fills.
type packets struct {
	ch     chan []nflog.Packet
	closed chan struct{}
	once   sync.Once
}

func newPackets() *packets {
	return &packets{ch: make(chan []nflog.Packet, 4), closed: make(chan struct{})}
}

func (p *packets) Read() ([]nflog.Packet, error) {
	select {
	case b := <-p.ch:
		return b, nil
	case <-p.closed:
		return nil, errors.New("closed")
	}
}

func (p *packets) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func dnsQuery(name string) []byte {
	q := []byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(name, ".") {
		q = append(append(q, byte(len(label))), label...)
	}
	return append(q, 0, 0, 1, 0, 1)
}

// askDNS sends a query to the forwarder and returns the response code.
func askDNS(t *testing.T, addr, name string) int {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(dnsQuery(name)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || n < 4 {
		t.Fatalf("no answer for %s: %v", name, err)
	}
	return int(buf[3] & 0xf)
}

// egressHarness starts the agent with one class that has an allowlist.
type egressHarness struct {
	*harness
	host *fakeHost
	// dns is where the class's forwarder listens.
	dns     chan string
	packets *packets
}

func startEgress(t *testing.T, host *fakeHost, containers map[string]*container, change ...func(*config.Class, *harness)) *egressHarness {
	t.Helper()
	policy, err := egress.Parse([]string{"github", "api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	e := &egressHarness{host: host, dns: make(chan string, 1), packets: newPackets()}
	change = append([]func(*config.Class, *harness){func(c *config.Class, h *harness) {
		c.Egress = &policy
		h.run = host.run
		h.docker.subnet, h.docker.gateway = "127.0.0.0/8", "127.0.0.1"
		h.upstream = answers{"203.0.113.5"}
		h.openPackets = func(uint16) (PacketSource, error) { return e.packets, nil }
		h.egressListen = func(egress.Net) (net.PacketConn, net.Listener, error) {
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				return nil, nil, err
			}
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, nil, err
			}
			e.dns <- pc.LocalAddr().String()
			return pc, l, nil
		}
	}}, change...)
	e.harness = startAgent(t, containers, nil, change...)
	return e
}

// snapshot is what status shows, once the agent answers.
func (e *egressHarness) snapshot(t *testing.T) status.Snapshot {
	t.Helper()
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var s status.Snapshot
		if s, err = status.Fetch(context.Background(), e.socket); err == nil {
			return s
		}
	}
	t.Fatal(err)
	return status.Snapshot{}
}

// A unix socket path stays under 104 bytes on macOS, so these tests have short names.

// TestEgressServed: the class runs on its own network and answers its own lookups.
func TestEgressServed(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	dns := <-e.dns
	net := egress.NetOf("boxrunner")

	waitFor(t, "the class's network", func() bool {
		e.docker.mu.Lock()
		defer e.docker.mu.Unlock()
		return e.docker.nets[net.Name] != nil
	})
	if got := e.docker.nets[net.Name]; got.Bridge != net.Bridge || got.Labels["boxrunner.egress"] != "boxrunner" {
		t.Errorf("network = %+v, want bridge %s labelled with the class", got, net.Bridge)
	}
	scripts := host.applied()
	if len(scripts) == 0 || !strings.Contains(scripts[0], fmt.Sprintf(`add rule inet boxrunner forward iifname "%s" jump egress_%s`, net.Bridge, net.ID())) {
		t.Fatalf("the table's script =\n%s", strings.Join(scripts, "\n--\n"))
	}
	if !slices.ContainsFunc(host.commands(), func(c string) bool { return strings.HasPrefix(c, "iptables-restore ") }) {
		t.Errorf("the host's rules for the bridge were not applied: %q", host.commands())
	}

	if got := askDNS(t, dns, "api.example.com"); got != 0 {
		t.Errorf("an allowed name answered with code %d", got)
	}
	if got := askDNS(t, dns, "data.evil.example"); got != 5 {
		t.Errorf("a name that is not allowed answered with code %d, want REFUSED", got)
	}
	host.mu.Lock()
	opened := slices.Clone(host.elements)
	host.mu.Unlock()
	if len(opened) != 1 || !strings.Contains(opened[0], "203.0.113.5 . 443 timeout 300s") {
		t.Errorf("opened %q, want the address of api.example.com on 80 and 443", opened)
	}

	e.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	var name string
	waitFor(t, "the job's container", func() bool {
		for n, c := range e.docker.snapshot() {
			name = n
			return c.State == "running"
		}
		return false
	})
	hostConfig := e.docker.snapshot()[name].Body["HostConfig"].(map[string]any)
	if hostConfig["NetworkMode"] != net.Name || fmt.Sprint(hostConfig["Dns"]) != "[127.0.0.1]" {
		t.Errorf("the job runs on %v with DNS %v, want %s and the class's gateway", hostConfig["NetworkMode"], hostConfig["Dns"], net.Name)
	}

	e.packets.ch <- []nflog.Packet{{Prefix: net.ID(), Src: netip.MustParseAddr("172.30.0.7"), Dst: netip.MustParseAddr("203.0.113.5"), Proto: 6, DstPort: 22}}
	var snap status.Snapshot
	waitFor(t, "the refusals in status", func() bool {
		snap = e.snapshot(t)
		return len(snap.Egress) == 1 && len(snap.Egress[0].Recent) == 2
	})
	got := snap.Egress[0]
	if got.Class != "boxrunner" || got.Problem != "" || got.Lookups != 1 || len(got.Entries) != 2 {
		t.Errorf("status of the class = %+v", got)
	}
	var lookup, packet egress.Block
	for _, b := range got.Recent {
		if b.Kind == egress.Lookup {
			lookup = b
		} else {
			packet = b
		}
	}
	if lookup.Dest != "data.evil.example" || packet.Dest != "203.0.113.5:22" || packet.Name != "api.example.com" || packet.Runner != name || packet.Repo != "octo/site" {
		t.Errorf("recent = %+v and %+v; want the lookup, and the packet with the name its address was opened for and the job that sent it", lookup, packet)
	}
}

// TestEgressNoNft: a box without nft does not serve the class.
func TestEgressNoNft(t *testing.T) {
	host := &fakeHost{noNft: true}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	var snap status.Snapshot
	waitFor(t, "the class's problem in status", func() bool {
		snap = e.snapshot(t)
		return len(snap.Egress) == 1 && snap.Egress[0].Problem != ""
	})
	if want := "nft is not installed; run sudo boxrunner install"; snap.Egress[0].Problem != want {
		t.Errorf("problem = %q, want %q", snap.Egress[0].Problem, want)
	}
	e.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	time.Sleep(300 * time.Millisecond)
	if n := len(e.docker.snapshot()); n != 0 {
		t.Errorf("%d containers started for a class whose allowlist is not in force", n)
	}
	for _, call := range e.github.workerCalls() {
		if strings.Contains(call, "scalesets") {
			t.Errorf("the agent asked for a scale set of a class it does not serve: %s", call)
		}
	}
}

// TestEgressFlushed: the rules come back when someone flushes them.
func TestEgressFlushed(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) == 1 })
	host.mu.Lock()
	host.flushed = true
	host.mu.Unlock()
	waitFor(t, "the ruleset again", func() bool { return len(host.applied()) >= 2 })
	if got := e.snapshot(t).Egress[0].Problem; got != "" {
		t.Errorf("the class is not served after the rules came back: %q", got)
	}
}

// TestEgressGone: no runner starts while the rules cannot be put back.
func TestEgressGone(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) == 1 })
	host.mu.Lock()
	host.flushed, host.failApply = true, true
	host.mu.Unlock()
	e.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	time.Sleep(500 * time.Millisecond)
	if n := len(e.docker.snapshot()); n != 0 {
		t.Errorf("%d containers started with the egress rules gone", n)
	}
	for _, call := range e.github.workerCalls() {
		if strings.Contains(call, "/v1/runners") {
			t.Errorf("a runner config was minted with the egress rules gone: %s", call)
		}
	}
}

// TestEgressLeft: the network of a class that left stays closed while its jobs run.
func TestEgressLeft(t *testing.T) {
	host := &fakeHost{}
	gone := egress.NetOf("gone")
	idle := egress.NetOf("idle")
	e := startEgress(t, host, map[string]*container{
		"box-1-gone-ab12cd": {Name: "box-1-gone-ab12cd", Labels: map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": "box-1-gone-ab12cd", "boxrunner.class": "gone", "boxrunner.repo": "octo/site"},
			State: "running", Created: time.Now().Unix(), Body: map[string]any{"HostConfig": map[string]any{"NetworkMode": gone.Name}}},
	}, func(c *config.Class, h *harness) {
		h.docker.nets = map[string]*fakeNet{
			gone.Name: {Bridge: gone.Bridge, Labels: map[string]string{"boxrunner.egress": "gone"}, Subnet: "172.40.0.0/16", Gateway: "172.40.0.1"},
			idle.Name: {Bridge: idle.Bridge, Labels: map[string]string{"boxrunner.egress": "idle"}, Subnet: "172.41.0.0/16", Gateway: "172.41.0.1"},
		}
	})
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) >= 1 })
	script := host.applied()[0]
	if !strings.Contains(script, "egress_"+gone.ID()+" ct state established") || strings.Contains(script, "@"+gone.ID()) {
		t.Errorf("the class that left is not closed:\n%s", script)
	}
	if strings.Contains(script, "egress_"+idle.ID()) {
		t.Errorf("the network of a class that left, with no job, is still in the table:\n%s", script)
	}
	e.docker.mu.Lock()
	defer e.docker.mu.Unlock()
	if e.docker.nets[gone.Name] == nil || e.docker.nets[idle.Name] != nil {
		t.Errorf("networks = %v; want the one with a job kept and the other removed", e.docker.nets)
	}
}

func TestSpecEgressClass(t *testing.T) {
	policy, _ := egress.Parse([]string{"github"})
	class := config.Class{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Image: "boxrunner/runner:local", Docker: true, Isolation: config.Sysbox, Egress: &policy}
	r := schedulerRunner()
	n := egress.NetOf("boxrunner")
	n.Gateway = netip.MustParseAddr("172.19.0.1")
	got := Spec("box-1", class, r, "jit", "http://172.18.0.1:5000", "", &n)
	if got.Network != "boxrunner-boxrunner" || fmt.Sprint(got.DNS) != "[172.19.0.1]" {
		t.Errorf("network %q, DNS %v", got.Network, got.DNS)
	}
	if want := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit", "BOXRUNNER_DOCKER=1", "BOXRUNNER_MIRROR=http://172.18.0.1:5000", "BOXRUNNER_DNS=172.19.0.1"}; !slices.Equal(got.Env, want) {
		t.Errorf("env = %q, want %q", got.Env, want)
	}
	class.Docker, class.Isolation = false, config.Runc
	if got := Spec("box-1", class, r, "jit", "", "", &n); slices.ContainsFunc(got.Env, func(e string) bool { return strings.HasPrefix(e, "BOXRUNNER_DNS") }) {
		t.Errorf("a class without Docker gets the daemon's DNS: %q", got.Env)
	}
}

func schedulerRunner() scheduler.Runner {
	return scheduler.Runner{Name: "box-1-boxrunner-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner"}, ScaleSetID: 7, ID: 41}
}

// TestEgressRemoved: a box whose config has no egress class any more loses the table.
func TestEgressRemoved(t *testing.T) {
	host := &fakeHost{chains: map[string]bool{"forward": true, "input": true}, sets: map[string]bool{}, counters: map[string]bool{}}
	h := startAgent(t, map[string]*container{}, nil, func(c *config.Class, h *harness) { h.run = host.run })
	defer h.stop(t)
	waitFor(t, "the table to go", func() bool { return slices.Contains(host.commands(), "nft delete table inet boxrunner") })
	if got := host.applied(); len(got) != 0 {
		t.Errorf("the agent built a table for no class: %q", got)
	}
}
