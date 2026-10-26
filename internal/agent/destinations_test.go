package agent

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/nflog"
	"github.com/dominikzabcik/boxrunner/internal/seen"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

// seenHarness starts the agent on the job network, with a log of new
// connections that the test fills.
type seenHarness struct {
	*harness
	host *fakeHost
	log  *packets
	// opens counts how often the agent opened the log of new connections.
	mu    sync.Mutex
	opens int
}

func startSeen(t *testing.T, host *fakeHost, change ...func(*config.Class, *harness)) *seenHarness {
	t.Helper()
	s := &seenHarness{host: host, log: newPackets()}
	change = append([]func(*config.Class, *harness){func(_ *config.Class, h *harness) {
		h.run = host.run
		h.openPackets = func(group uint16) (PacketSource, error) {
			if group != layout.SeenNFLogGroup {
				return newPackets(), nil
			}
			s.mu.Lock()
			s.opens++
			s.mu.Unlock()
			return s.log, nil
		}
	}}, change...)
	s.harness = startAgent(t, map[string]*container{}, nil, change...)
	return s
}

func (s *seenHarness) opened() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

func (s *seenHarness) snapshot(t *testing.T) status.Snapshot {
	t.Helper()
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var snap status.Snapshot
		if snap, err = status.Fetch(context.Background(), s.socket); err == nil {
			return snap
		}
	}
	t.Fatal(err)
	return status.Snapshot{}
}

// runJob gives the agent a job and waits until its runner shows in status.
func (s *seenHarness) runJob(t *testing.T) {
	t.Helper()
	s.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	waitFor(t, "the job's runner in status", func() bool {
		return slices.ContainsFunc(s.snapshot(t).Runners, func(r status.Runner) bool { return r.Phase == "starting" })
	})
}

func connect(src, dst string, proto uint8, port uint16) nflog.Packet {
	return nflog.Packet{Src: netip.MustParseAddr(src), Dst: netip.MustParseAddr(dst), Proto: proto, DstPort: port}
}

// destinations is what status shows of a repository's jobs.
func (s *seenHarness) destinations(t *testing.T, repo string) []seen.Destination {
	t.Helper()
	for _, r := range s.snapshot(t).Destinations {
		if r.Repo == repo {
			return r.Destinations
		}
	}
	return nil
}

// TestDestinationsRecorded: a new connection from a job on the job network
// shows in status under the job's repository, with the others left out.
func TestDestinationsRecorded(t *testing.T) {
	host := &fakeHost{}
	s := startSeen(t, host)
	defer s.stop(t)
	waitFor(t, "the rule that logs new connections", func() bool { return strings.Contains(host.seenScript(), "log group 4138") })
	s.runJob(t)

	s.log.ch <- []nflog.Packet{
		connect("172.18.0.7", "203.0.113.9", 6, 443),
		connect("172.18.0.7", "203.0.113.9", 6, 443),
		connect("172.18.0.7", "198.51.100.4", 17, 53),
		connect("172.18.0.7", "172.18.0.9", 6, 5432),   // another container on the job's network
		connect("172.18.0.99", "203.0.113.50", 6, 443), // no runner has this address
		connect("10.55.0.1", "203.0.113.60", 6, 443),   // not an address of the job network
	}
	waitFor(t, "the destinations in status", func() bool {
		return len(s.destinations(t, "octo/site")) == 2 && len(s.destinations(t, "")) == 1
	})

	got := s.destinations(t, "octo/site")
	slices.SortFunc(got, func(a, b seen.Destination) int { return strings.Compare(a.Addr, b.Addr) })
	if got[0].Addr != "198.51.100.4" || got[0].Port != 53 || got[0].Proto != "udp" || got[0].Count != 1 {
		t.Errorf("first destination = %+v, want udp to 198.51.100.4:53 seen once", got[0])
	}
	if got[1].Addr != "203.0.113.9" || got[1].Port != 443 || got[1].Proto != "tcp" || got[1].Count != 2 {
		t.Errorf("second destination = %+v, want tcp to 203.0.113.9:443 seen twice", got[1])
	}
	s.waitLog(t, `msg="a job connected somewhere new"`, "repo=octo/site", "dest=203.0.113.9:443")
	if unknown := s.destinations(t, ""); len(unknown) != 1 || unknown[0].Addr != "203.0.113.50" {
		t.Errorf("destinations of containers that are not runners = %+v, want only 203.0.113.50", unknown)
	}
	if line := s.logs.String(); strings.Contains(line, "172.18.0.9") || strings.Contains(line, "203.0.113.60") {
		t.Errorf("the log mentions a connection that is not a container leaving the network:\n%s", line)
	}
}

// TestDestinationsRulesKept: the agent puts the rule back after a flush.
func TestDestinationsRulesKept(t *testing.T) {
	host := &fakeHost{}
	s := startSeen(t, host)
	defer s.stop(t)
	waitFor(t, "the rule", func() bool { return host.seenScript() != "" })
	host.flushRuleset()
	waitFor(t, "the rule put back", func() bool { return host.seenScript() != "" })
	s.waitLog(t, `msg="the rule that logs the destinations jobs connect to was gone; put it back"`)
}

// TestDestinationsNoNft: without nft nothing is recorded, and status says why.
func TestDestinationsNoNft(t *testing.T) {
	s := startSeen(t, &fakeHost{noNft: true})
	defer s.stop(t)
	want := "nft is not installed, so the destinations jobs connect to are not recorded; install nftables and restart boxrunner"
	if got := s.snapshot(t).DestinationsProblem; got != want {
		t.Errorf("problem = %q, want %q", got, want)
	}
	if n := s.opened(); n != 0 {
		t.Errorf("the log was opened %d times without nft", n)
	}
}

// TestDestinationsRuleRefused: a rule nft refuses is a problem in status.
func TestDestinationsRuleRefused(t *testing.T) {
	s := startSeen(t, &fakeHost{failSeen: true})
	defer s.stop(t)
	want := "the rule that logs new connections could not be applied: nft: exit status 1: Error: could not process rule"
	waitFor(t, "the problem", func() bool { return s.snapshot(t).DestinationsProblem == want })
	s.host.mu.Lock()
	s.host.failSeen = false
	s.host.mu.Unlock()
	waitFor(t, "the problem gone once the rule applies", func() bool { return s.snapshot(t).DestinationsProblem == "" })
}

// TestDestinationsSaved: the book is written when the agent stops
// and read when it starts.
func TestDestinationsSaved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "destinations.json")
	s := startSeen(t, &fakeHost{}, func(_ *config.Class, h *harness) { h.destinations = file })
	s.runJob(t)
	s.log.ch <- []nflog.Packet{connect("172.18.0.7", "203.0.113.9", 6, 443)}
	waitFor(t, "the destination", func() bool { return len(s.destinations(t, "octo/site")) == 1 })
	s.stop(t)

	again := startSeen(t, &fakeHost{}, func(_ *config.Class, h *harness) { h.destinations = file })
	defer again.stop(t)
	got := again.destinations(t, "octo/site")
	if len(got) != 1 || got[0].Addr != "203.0.113.9" || got[0].Port != 443 || got[0].Count != 1 {
		t.Errorf("after a restart = %+v, want tcp to 203.0.113.9:443 seen once", got)
	}
}

// TestDestinationsInHeartbeat: the Worker gets the newest destinations.
func TestDestinationsInHeartbeat(t *testing.T) {
	s := startSeen(t, &fakeHost{})
	defer s.stop(t)
	s.runJob(t)
	s.log.ch <- []nflog.Packet{connect("172.18.0.7", "203.0.113.9", 6, 443)}
	waitFor(t, "a heartbeat with the destination", func() bool {
		return strings.Contains(s.github.lastHeartbeat(), `"destinations":[{"repo":"octo/site","addr":"203.0.113.9","port":443,"proto":"tcp","first":"`)
	})
}

// TestDestinationsNameOfEgressClass: a class with an allowlist names the
// address its forwarder opened for a name.
func TestDestinationsNameOfEgressClass(t *testing.T) {
	host := &fakeHost{}
	e := startEgress(t, host, map[string]*container{}, func(_ *config.Class, h *harness) {
		h.docker.jobAddr = "127.0.0.7/8"
	})
	defer e.stop(t)
	dns := <-e.dns
	if rcode := askDNS(t, dns, "api.example.com"); rcode != 0 {
		t.Fatalf("api.example.com was refused with code %d", rcode)
	}
	e.startJob(t)
	waitFor(t, "the job's runner in status", func() bool {
		return slices.ContainsFunc(e.snapshot(t).Runners, func(r status.Runner) bool { return r.Phase == "starting" })
	})
	e.seen.ch <- []nflog.Packet{connect("127.0.0.7", "203.0.113.5", 6, 443), connect("127.0.0.7", "203.0.113.77", 6, 443)}
	waitFor(t, "both destinations", func() bool {
		for _, r := range e.snapshot(t).Destinations {
			if r.Repo == "octo/site" && len(r.Destinations) == 2 {
				return true
			}
		}
		return false
	})
	names := map[string]string{}
	for _, r := range e.snapshot(t).Destinations {
		for _, d := range r.Destinations {
			names[d.Addr] = d.Name
		}
	}
	if names["203.0.113.5"] != "api.example.com" || names["203.0.113.77"] != "" {
		t.Errorf("names = %v, want api.example.com for 203.0.113.5 and none for 203.0.113.77", names)
	}
}

func TestDestinationsForTheHeartbeat(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	repos := []seen.Repo{
		{Repo: "", Destinations: []seen.Destination{{Addr: "203.0.113.200", Port: 443, Proto: "tcp", First: t0.Add(time.Hour), Count: 1}}},
		{Repo: "octo/docs", Destinations: []seen.Destination{{Addr: "203.0.113.2", Port: 443, Proto: "tcp", First: t0.Add(time.Minute), Count: 3}}},
		{Repo: "octo/site", Destinations: []seen.Destination{
			{Addr: "203.0.113.9", Port: 443, Proto: "tcp", Name: "api.example.com", First: t0.Add(2 * time.Minute), Count: 1},
			{Addr: "198.51.100.4", Port: 53, Proto: "udp", First: t0, Count: 5},
		}},
	}
	got := destinationsFor(repos)
	want := []worker.HeartbeatDestination{
		{Repo: "octo/site", Addr: "203.0.113.9", Port: 443, Proto: "tcp", Name: "api.example.com", First: t0.Add(2 * time.Minute), Count: 1},
		{Repo: "octo/docs", Addr: "203.0.113.2", Port: 443, Proto: "tcp", First: t0.Add(time.Minute), Count: 3},
		{Repo: "octo/site", Addr: "198.51.100.4", Port: 53, Proto: "udp", First: t0, Count: 5},
	}
	if !slices.Equal(got, want) {
		t.Errorf("destinationsFor =\n%+v\nwant\n%+v", got, want)
	}

	var many []seen.Repo
	for r := range 8 {
		repo := seen.Repo{Repo: "octo/r" + string(rune('a'+r))}
		for i := range 10 {
			repo.Destinations = append(repo.Destinations, seen.Destination{Addr: "203.0.113.1", Port: uint16(1000 + i), Proto: "tcp", First: t0.Add(time.Duration(r*10+i) * time.Second)})
		}
		many = append(many, repo)
	}
	got = destinationsFor(many)
	if len(got) != heartbeatDestinations || got[0].Repo != "octo/rh" || got[0].Port != 1009 {
		t.Errorf("%d destinations led by %s:%d, want %d led by octo/rh:1009", len(got), got[0].Repo, got[0].Port, heartbeatDestinations)
	}
}

func TestFitHeartbeatDropsOldestDestinationsFirst(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	hb := heartbeatBody(status.Snapshot{Box: "box-1"}, []status.Job{{Runner: "r1", Repo: "octo/site", Class: "boxrunner", Name: "test", Result: "succeeded"}})
	for i := range 40 {
		hb.Destinations = append(hb.Destinations, worker.HeartbeatDestination{Repo: "octo/site", Addr: "203.0.113.9", Port: uint16(1000 + i), Proto: "tcp", First: t0.Add(-time.Duration(i) * time.Second), Count: 1})
	}
	fitted := fitHeartbeat(hb, 1500)
	if len(fitted.Jobs) != 1 {
		t.Errorf("%d jobs kept, want the one job", len(fitted.Jobs))
	}
	if n := len(fitted.Destinations); n == 0 || n >= 40 || fitted.Destinations[0].Port != 1000 {
		t.Errorf("%d destinations kept starting at port %d, want some of the newest and not all", n, fitted.Destinations[0].Port)
	}
}
