package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/nflog"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

// fakeHost plays nft and iptables: it keeps what each was told to build and
// lists it back as they would, and records every command.
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
	// packets is what every counter of the table reads.
	packets  uint64
	chains   map[string]bool
	sets     map[string]bool
	counters map[string]bool
	jumps    []string
	// ipt are the iptables and ip6tables chains, by command, with their rules.
	ipt map[string]map[string][]string
}

// builtinChains are a host's iptables chains with Docker running.
func builtinChains() map[string]map[string][]string {
	return map[string]map[string][]string{
		"iptables":  {"INPUT": nil, "FORWARD": nil, "DOCKER-USER": nil},
		"ip6tables": {"INPUT": nil, "FORWARD": nil},
	}
}

// flushRuleset does what `nft flush ruleset` does on a host whose iptables
// is iptables-nft: the table and every iptables rule go, Docker's included.
func (f *fakeHost) flushRuleset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chains, f.sets, f.counters, f.jumps = nil, nil, nil, nil
	f.ipt = map[string]map[string][]string{"iptables": {"INPUT": nil, "FORWARD": nil}, "ip6tables": {"INPUT": nil, "FORWARD": nil}}
}

// iptables runs an iptables, ip6tables or *-restore command on ipt.
func (f *fakeHost) iptables(stdin, name string, args ...string) (string, error) {
	if f.ipt == nil {
		f.ipt = builtinChains()
	}
	cmd := strings.TrimSuffix(name, "-restore")
	chains := f.ipt[cmd]
	if chains == nil {
		return "", fmt.Errorf("%s: command not found", name)
	}
	if strings.HasSuffix(name, "-restore") {
		for _, line := range strings.Split(stdin, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				chains[strings.Fields(line[1:])[0]] = nil
			case strings.HasPrefix(line, "-A "):
				c := strings.Fields(line)[1]
				chains[c] = append(chains[c], line)
			}
		}
		return "", nil
	}
	args = args[1:] // -w
	op, chain := args[0], args[1]
	rules, exists := chains[chain]
	if !exists {
		return "iptables: No chain/target/match by that name.", errors.New("exit status 1")
	}
	rule := func(from int) string { return strings.Join(append([]string{"-A", chain}, args[from:]...), " ") }
	switch op {
	case "-S":
		return strings.Join(append([]string{"-N " + chain}, rules...), "\n") + "\n", nil
	case "-C":
		if !slices.Contains(rules, rule(2)) {
			return "", errors.New("exit status 1")
		}
	case "-I":
		chains[chain] = append([]string{rule(3)}, rules...)
	case "-D":
		i := slices.Index(rules, rule(2))
		if n, err := strconv.Atoi(args[2]); err == nil && len(args) == 3 {
			i = n - 1
		}
		if i < 0 || i >= len(rules) {
			return "", errors.New("exit status 1")
		}
		chains[chain] = slices.Delete(rules, i, i+1)
	case "-F":
		chains[chain] = nil
	case "-X":
		delete(chains, chain)
	}
	return "", nil
}

// iptablesRules lists every rule of a chain of iptables.
func (f *fakeHost) iptablesRules(chain string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ipt["iptables"][chain])
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
		return f.iptables(stdin, name, args...)
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
		var b strings.Builder
		b.WriteString("table inet boxrunner {\n")
		for name := range f.counters {
			fmt.Fprintf(&b, "\tcounter %s {\n\t\tpackets %d bytes 0\n\t}\n", name, f.packets)
		}
		b.WriteString("}\n")
		return b.String(), nil
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
	// udp is the forwarder's latest UDP socket.
	mu  sync.Mutex
	udp net.PacketConn
}

func startEgress(t *testing.T, host *fakeHost, containers map[string]*container, change ...func(*config.Class, *harness)) *egressHarness {
	t.Helper()
	policy, err := egress.Parse([]string{"github", "api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	e := &egressHarness{host: host, dns: make(chan string, 4), packets: newPackets()}
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
			e.mu.Lock()
			e.udp = pc
			e.mu.Unlock()
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
	e.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	e.waitLog(t, `msg="class has an egress allowlist that is not in force; not serving it" class=boxrunner`)
	if want := "nft is not installed; run sudo boxrunner install"; e.snapshot(t).Egress[0].Problem != want {
		t.Errorf("problem = %q, want %q", e.snapshot(t).Egress[0].Problem, want)
	}
	if n := len(e.github.queue); n != 1 {
		t.Errorf("%d messages left in the queue, want the job's: nobody may listen for the class", n)
	}
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

// TestEgressGone: a runner whose start finds the rules gone, and cannot put
// them back, is refused before a runner config is minted, and the class is
// no longer served.
func TestEgressGone(t *testing.T) {
	host := &fakeHost{}
	// Only the start checks the rules here.
	e := startEgress(t, host, map[string]*container{}, func(_ *config.Class, h *harness) { h.guardEvery = time.Hour })
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) == 1 })
	host.mu.Lock()
	host.flushed, host.failApply = true, true
	host.mu.Unlock()
	e.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	e.waitLog(t, `msg="the egress rules are not in place; not starting a runner"`, "class=boxrunner")
	if n := len(e.docker.snapshot()); n != 0 {
		t.Errorf("%d containers started with the egress rules gone", n)
	}
	for _, call := range e.github.workerCalls() {
		if strings.Contains(call, "/v1/runners") {
			t.Errorf("a runner config was minted with the egress rules gone: %s", call)
		}
	}
	if got, want := e.snapshot(t).Egress[0].Problem, "the egress rules are gone and could not be put back: nft: exit status 1: Error: could not process rule"; got != want {
		t.Errorf("problem = %q, want %q", got, want)
	}
}

// TestEgressBroken: the class is not served while the guard cannot put the
// rules back, and is again once it can.
func TestEgressBroken(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) == 1 })
	host.mu.Lock()
	host.flushed, host.failApply = true, true
	host.mu.Unlock()
	want := "the egress rules are gone and could not be put back: nft: exit status 1: Error: could not process rule"
	waitFor(t, "the class's problem", func() bool { return e.snapshot(t).Egress[0].Problem == want })
	e.waitLog(t, `msg="class has an egress allowlist that is not in force; not serving it" class=boxrunner`)
	host.mu.Lock()
	host.failApply = false
	host.mu.Unlock()
	waitFor(t, "the class served again", func() bool { return e.snapshot(t).Egress[0].Problem == "" })
}

// egressJob is a running job container of a class on the class's network.
func egressJob(name string, n egress.Net) *container {
	return &container{Name: name, State: "running", Created: time.Now().Unix(),
		Labels: map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": name, "boxrunner.class": n.Class, "boxrunner.repo": "octo/site"},
		Body:   map[string]any{"HostConfig": map[string]any{"NetworkMode": n.Name}}}
}

// startJob gives the agent a job and returns its running container's name.
func (e *egressHarness) startJob(t *testing.T) string {
	t.Helper()
	e.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	var name string
	waitFor(t, "the job's container", func() bool {
		for n, c := range e.docker.snapshot() {
			name = n
			return c.State == "running"
		}
		return false
	})
	return name
}

// TestEgressFlushRemovesJobs: after `nft flush ruleset`, which takes the
// table and the host's backstop, the agent puts both back and removes the
// jobs that ran without them, since they may hold a connection the rules refuse.
func TestEgressFlushRemovesJobs(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	name := e.startJob(t)
	host.flushRuleset()
	e.waitLog(t, `msg="removed a job of an egress class" runner=`+name, `why="the egress rules were gone while it ran"`)
	if _, ok := e.docker.snapshot()[name]; ok {
		t.Errorf("the job %s still runs after the rules were flushed", name)
	}
	waitFor(t, "the backstop back", func() bool {
		return slices.Contains(host.iptablesRules("FORWARD"), "-A FORWARD -i boxrunner-+ -m comment --comment boxrunner -j BOXRUNNER-EGRESS-FWD") &&
			slices.Contains(host.iptablesRules("BOXRUNNER-EGRESS-FWD"), "-A BOXRUNNER-EGRESS-FWD -m mark ! --mark 0x10000000/0x10000000 -j DROP")
	})
	if !egress.Present(context.Background(), func(_ context.Context, stdin, name string, args ...string) (string, error) {
		return host.run(stdin, name, args...)
	}) {
		t.Error("the table is not back")
	}
}

// TestJobRulesFlushed: on a box without egress classes, the guard puts the
// job network's rules back after `nft flush ruleset` took them.
func TestJobRulesFlushed(t *testing.T) {
	host := &fakeHost{}
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) { h.run = host.run })
	defer h.stop(t)
	drop := "-A BOXRUNNER-FORWARD -d 10.0.0.0/8 -j DROP"
	waitFor(t, "the job network's rules", func() bool { return slices.Contains(host.iptablesRules("BOXRUNNER-FORWARD"), drop) })
	host.flushRuleset()
	h.waitLog(t, `level=WARN msg="the job network's firewall rules were incomplete; put them back"`)
	if !slices.Contains(host.iptablesRules("BOXRUNNER-FORWARD"), drop) {
		t.Errorf("BOXRUNNER-FORWARD = %q, want the private ranges dropped", host.iptablesRules("BOXRUNNER-FORWARD"))
	}
	if got, want := host.iptablesRules("FORWARD"), []string{"-A FORWARD -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD"}; !slices.Equal(got, want) {
		t.Errorf("FORWARD = %q, want %q", got, want)
	}
	if got, want := host.iptablesRules("INPUT"), []string{"-A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT"}; !slices.Equal(got, want) {
		t.Errorf("INPUT = %q, want %q", got, want)
	}
}

// TestEgressFlushJobRules: on a box with egress classes, the guard puts the
// job network's rules back with the table after `nft flush ruleset`, and the
// egress chain's jump stays ahead of theirs in INPUT.
func TestEgressFlushJobRules(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) == 1 })
	host.flushRuleset()
	e.waitLog(t, `level=WARN msg="the job network's firewall rules were incomplete; put them back"`)
	if got := host.iptablesRules("BOXRUNNER-FORWARD"); !slices.Contains(got, "-A BOXRUNNER-FORWARD -d 192.168.0.0/16 -j DROP") {
		t.Errorf("BOXRUNNER-FORWARD = %q, want the private ranges dropped", got)
	}
	want := []string{"-A INPUT -m comment --comment boxrunner -j BOXRUNNER-EGRESS", "-A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT"}
	waitFor(t, "the egress chain's jump first in INPUT", func() bool { return slices.Equal(host.iptablesRules("INPUT"), want) })
}

// TestEgressBackstop: the guard puts back the host's backstop when only it is
// gone. The table judged every packet meanwhile, so the job keeps running.
func TestEgressBackstop(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	name := e.startJob(t)
	jump := "-A DOCKER-USER -i boxrunner-+ -m comment --comment boxrunner -j BOXRUNNER-EGRESS-FWD"
	waitFor(t, "the backstop", func() bool { return slices.Contains(host.iptablesRules("DOCKER-USER"), jump) })
	host.mu.Lock()
	host.ipt["iptables"]["DOCKER-USER"] = slices.DeleteFunc(host.ipt["iptables"]["DOCKER-USER"], func(r string) bool { return r == jump })
	host.mu.Unlock()
	e.waitLog(t, `msg="the host's rules for the egress bridges were incomplete; put them back"`)
	if !slices.Contains(host.iptablesRules("DOCKER-USER"), jump) {
		t.Errorf("DOCKER-USER = %q, want the backstop's jump", host.iptablesRules("DOCKER-USER"))
	}
	if c, ok := e.docker.snapshot()[name]; !ok || c.State != "running" {
		t.Errorf("the job %s was stopped though the table stayed", name)
	}
}

// TestEgressStopRemovesJobs: a stopping agent stops the jobs of egress
// classes, which would otherwise run with nobody to keep their rules.
func TestEgressStopRemovesJobs(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	name := e.startJob(t)
	// On stop the agent removes a runner that has not taken a job without
	// stopping it first, so this one must have taken its job.
	e.github.queue <- message(2, 1, fmt.Sprintf(`{"messageType":"JobStarted","runnerRequestId":501,"runnerId":41,"runnerName":%q}`, name))
	waitFor(t, "the runner to take its job", func() bool {
		for _, r := range e.snapshot(t).Runners {
			if r.Name == name {
				return r.Phase == "running"
			}
		}
		return false
	})
	e.stop(t)
	if _, ok := e.docker.snapshot()[name]; ok {
		t.Errorf("the job %s still runs after the agent stopped", name)
	}
	e.docker.mu.Lock()
	defer e.docker.mu.Unlock()
	if !slices.Contains(e.docker.stopped, name) {
		t.Errorf("stopped %q, want %s stopped before it was removed, so its runner can cancel the job", e.docker.stopped, name)
	}
}

// TestEgressLeftover: a job of an egress class that ran while the agent was
// down is removed when the agent starts.
func TestEgressLeftover(t *testing.T) {
	host := &fakeHost{}
	n := egress.NetOf("boxrunner")
	e := startEgress(t, host, map[string]*container{"box-1-boxrunner-ab12cd": egressJob("box-1-boxrunner-ab12cd", n)}, func(c *config.Class, h *harness) {
		h.docker.nets = map[string]*fakeNet{n.Name: {Bridge: n.Bridge, Labels: map[string]string{"boxrunner.egress": "boxrunner"}, Subnet: "127.0.0.0/8", Gateway: "127.0.0.1"}}
	})
	defer e.stop(t)
	e.waitLog(t, `msg="removed a job of an egress class" runner=box-1-boxrunner-ab12cd`, `why="it ran while the agent was not running"`)
	if _, ok := e.docker.snapshot()["box-1-boxrunner-ab12cd"]; ok {
		t.Error("the job that ran while the agent was down still runs")
	}
}

// TestEgressNetRemoved: a class whose network someone removed is not served
// until the agent made it again on the same subnet and its forwarder listens again.
func TestEgressNetRemoved(t *testing.T) {
	host := &fakeHost{}
	n := egress.NetOf("boxrunner")
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	<-e.dns
	e.docker.mu.Lock()
	e.docker.failNetwork = true
	delete(e.docker.nets, n.Name)
	e.docker.mu.Unlock()
	want := "its Docker network boxrunner-boxrunner was removed and could not be made again on 127.0.0.0/8: docker: 403 Pool overlaps with other one on this address space; restart boxrunner"
	waitFor(t, "the class's problem", func() bool { return e.snapshot(t).Egress[0].Problem == want })
	e.docker.mu.Lock()
	e.docker.failNetwork = false
	e.docker.mu.Unlock()
	e.waitLog(t, `msg="the class's Docker network was removed; made it again" class=boxrunner network=boxrunner-boxrunner subnet=127.0.0.0/8`)
	<-e.dns
	waitFor(t, "the class served again", func() bool { return e.snapshot(t).Egress[0].Problem == "" })
	e.docker.mu.Lock()
	defer e.docker.mu.Unlock()
	if got := e.docker.nets[n.Name]; got == nil || got.Subnet != "127.0.0.0/8" || got.Gateway != "127.0.0.1" || got.Bridge != n.Bridge {
		t.Errorf("network = %+v, want it back on 127.0.0.0/8 with bridge %s", got, n.Bridge)
	}
}

// TestEgressForwarderStops: a forwarder that stops makes the class
// unserved until it listens again.
func TestEgressForwarderStops(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	<-e.dns
	e.mu.Lock()
	e.udp.Close()
	e.mu.Unlock()
	e.waitLog(t, `msg="not serving the class until its DNS forwarder listens again" class=boxrunner`)
	dns := <-e.dns
	waitFor(t, "the class served again", func() bool { return e.snapshot(t).Egress[0].Problem == "" })
	if got := askDNS(t, dns, "api.example.com"); got != 0 {
		t.Errorf("the forwarder answered an allowed name with code %d after it came back", got)
	}
}

// TestEgressLogReopens: the reader of the rules' log opens it again when reading fails.
func TestEgressLogReopens(t *testing.T) {
	host := &fakeHost{}
	broken := newPackets()
	broken.Close()
	var opens int
	var mu sync.Mutex
	e := startEgress(t, host, map[string]*container{}, func(_ *config.Class, h *harness) {
		good := h.openPackets
		h.openPackets = func(group uint16) (PacketSource, error) {
			mu.Lock()
			defer mu.Unlock()
			if opens++; opens == 1 {
				return broken, nil
			}
			return good(group)
		}
	})
	defer e.stop(t)
	n := egress.NetOf("boxrunner")
	e.waitLog(t, `msg="cannot read the egress rules' log; refused packets are counted, and described again once it opens" err=closed`)
	e.packets.ch <- []nflog.Packet{{Prefix: n.ID(), Src: netip.MustParseAddr("172.30.0.7"), Dst: netip.MustParseAddr("198.51.100.9"), Proto: 6, DstPort: 443}}
	waitFor(t, "the packet in status", func() bool {
		recent := e.snapshot(t).Egress[0].Recent
		return len(recent) == 1 && recent[0].Dest == "198.51.100.9:443"
	})
}

// TestEgressFirstStart: the first start denies the class's own new network,
// as a restart does.
func TestEgressFirstStart(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{}, func(_ *config.Class, h *harness) { h.docker.subnet, h.docker.gateway = "", "" })
	defer e.stop(t)
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) >= 1 })
	deny := regexp.MustCompile(`ip daddr \{ ([^}]*) \} goto blocked_`).FindStringSubmatch(host.applied()[0])
	if deny == nil || !slices.Contains(strings.Split(deny[1], ", "), "172.30.0.0/16") {
		t.Errorf("the class's network 172.30.0.0/16 is not denied on the first start: %q", deny)
	}
}

// TestEgressNewNet: a network that another tool makes after the agent
// started is denied to the class within the guard's interval, and one that
// goes is no longer. The chains are built again in one transaction that keeps
// the sets, so the job and what it has open stay.
func TestEgressNewNet(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	name := e.startJob(t)
	deny := func(script string) string {
		return regexp.MustCompile(`ip daddr \{ ([^}]*) \} goto blocked_`).FindStringSubmatch(script)[1]
	}
	first := host.applied()[0]
	e.docker.mu.Lock()
	e.docker.nets["octo-db"] = &fakeNet{Bridge: "br-octo", Subnet: "10.99.0.0/24", Gateway: "10.99.0.1"}
	e.docker.mu.Unlock()
	e.waitLog(t, `msg="the box's Docker networks changed; denied them to the egress classes" added=[10.99.0.0/24] removed=[]`)
	scripts := host.applied()
	if len(scripts) != 2 {
		t.Fatalf("%d scripts applied, want the first and one for the new network", len(scripts))
	}
	if got, want := deny(scripts[1]), "0.0.0.0/8, 10.99.0.0/24, 100.100.100.200/32, 127.0.0.0/8, 168.63.129.16/32, 169.254.0.0/16, 172.18.0.0/16, 192.0.0.192/32, 224.0.0.0/4, 240.0.0.0/4"; got != want {
		t.Errorf("denied %s, want %s", got, want)
	}
	if strings.Contains(scripts[1], "delete") || strings.Count(scripts[1], "add set") != strings.Count(first, "add set") {
		t.Errorf("the script for the new network replaces the sets:\n%s", scripts[1])
	}
	e.docker.mu.Lock()
	delete(e.docker.nets, "octo-db")
	e.docker.mu.Unlock()
	e.waitLog(t, `msg="the box's Docker networks changed; denied them to the egress classes" added=[] removed=[10.99.0.0/24]`)
	if got := deny(host.applied()[2]); got != deny(first) {
		t.Errorf("denied %s after the network went, want %s as at the start", got, deny(first))
	}
	if c, ok := e.docker.snapshot()[name]; !ok || c.State != "running" {
		t.Errorf("the job %s was stopped though the table stayed", name)
	}
	if got := e.snapshot(t).Egress[0].Problem; got != "" {
		t.Errorf("the class is not served: %q", got)
	}
}

// TestEgressLeft: the network of a class that left stays closed while a job
// the agent could not remove runs on it.
func TestEgressLeft(t *testing.T) {
	host := &fakeHost{}
	gone := egress.NetOf("gone")
	idle := egress.NetOf("idle")
	e := startEgress(t, host, map[string]*container{
		"box-1-gone-ab12cd": egressJob("box-1-gone-ab12cd", gone),
	}, func(c *config.Class, h *harness) {
		// The agent removes such a job when it starts; this one does not go.
		h.docker.failRemove = map[string]bool{"box-1-gone-ab12cd": true}
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

// TestStartRefusesWithoutTheClassNetwork: a runner of an egress class whose
// allowlist is not in force is not started, not even on the job network.
func TestStartRefusesWithoutTheClassNetwork(t *testing.T) {
	policy, _ := egress.Parse([]string{"github"})
	for _, set := range []map[string]*egressClass{
		{},
		{"agent": {net: egress.NetOf("agent"), dns: "its DNS forwarder stopped: use of closed network connection"}},
	} {
		var calls []string
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, r.URL.Path)
			mu.Unlock()
			http.Error(w, "unexpected", 500)
		}))
		u, _ := url.Parse(srv.URL)
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		a := &Agent{
			Options: Options{
				Config: config.Config{Box: "box-1", Classes: []config.Class{{Name: "agent", Image: "boxrunner/runner:local", Egress: &policy}}},
				Worker: worker.New(worker.Options{URL: u, Token: "box-token", Version: "0.1.0", Log: log}),
				Log:    log,
			},
			events: make(chan scheduler.Event, 1),
			resync: make(chan struct{}, 1),
			egress: &egressState{enforcer: &egress.Enforcer{}, ledger: &egress.Ledger{}, classes: set},
		}
		r := scheduler.Runner{Name: "box-1-agent-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "agent"}, ScaleSetID: 7}
		// The Worker's client retries a failed call until the start's context ends.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		a.start(ctx, r)
		cancel()
		srv.Close()
		if got := <-a.events; got != (scheduler.StartFailed{Runner: r.Name}) {
			t.Errorf("event = %#v, want the start to fail", got)
		}
		if len(calls) != 0 {
			t.Errorf("with classes %v the agent called the Worker: %q", set, calls)
		}
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

// TestEgressSetupFails: a class whose setup fails keeps its network closed,
// so a job on it is never left with the whole internet.
func TestEgressSetupFails(t *testing.T) {
	host := &fakeHost{}
	n := egress.NetOf("boxrunner")
	e := startEgress(t, host, map[string]*container{
		"box-1-boxrunner-ab12cd": egressJob("box-1-boxrunner-ab12cd", n),
	}, func(c *config.Class, h *harness) {
		h.docker.nets = map[string]*fakeNet{n.Name: {Bridge: n.Bridge, Labels: map[string]string{"boxrunner.egress": "boxrunner"}, Subnet: "127.0.0.0/8", Gateway: "127.0.0.1"}}
		h.docker.failRemove = map[string]bool{"box-1-boxrunner-ab12cd": true}
		// The agent inspects the network to list it, then to set it up.
		h.docker.failInspect = map[string]int{n.Name: 2}
	})
	defer e.stop(t)
	var snap status.Snapshot
	waitFor(t, "the class's problem in status", func() bool {
		snap = e.snapshot(t)
		return len(snap.Egress) == 1 && snap.Egress[0].Problem != ""
	})
	waitFor(t, "the first ruleset", func() bool { return len(host.applied()) >= 1 })
	script := host.applied()[0]
	if !strings.Contains(script, "egress_"+n.ID()+" ct state established") || strings.Contains(script, "@"+n.ID()+"_dyn") {
		t.Errorf("the class whose setup failed is not closed:\n%s", script)
	}
	if !slices.ContainsFunc(host.commands(), func(c string) bool { return strings.HasPrefix(c, "iptables-restore ") }) {
		t.Errorf("the host's rules were not applied for the closed bridge: %q", host.commands())
	}
}

// TestEgressCounts: the packets the table dropped only grow, though its counters start again.
func TestEgressCounts(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{})
	defer e.stop(t)
	<-e.dns // the forwarder starts once the agent has read where the counters begin
	set := func(n uint64) {
		host.mu.Lock()
		host.packets = n
		host.mu.Unlock()
	}
	packets := func() uint64 { return e.snapshot(t).Egress[0].Packets }
	for _, step := range []struct{ read, want uint64 }{{10, 10}, {3, 13}, {5, 15}} {
		set(step.read)
		waitFor(t, fmt.Sprintf("%d packets", step.want), func() bool { return packets() == step.want })
	}
}
