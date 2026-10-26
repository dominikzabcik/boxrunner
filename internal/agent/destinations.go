package agent

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/dnsfwd"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/nflog"
	"github.com/dominikzabcik/boxrunner/internal/seen"
)

const (
	// A job's address is looked up in Docker when its first connection is
	// logged, and remembered for this long; one that is not a job's is
	// remembered for less, so a container still being created is found soon.
	sourceFor   = 10 * time.Second
	noSourceFor = 3 * time.Second
	maxSources  = 2048
	// A box that sees many new destinations logs this many of them a minute.
	loggedPerMinute = 30
	// statusDestinations is how many destinations of each repository status shows.
	statusDestinations = 10
	// saveDestinationsEvery is how often the book is written to disk.
	saveDestinationsEvery = 5 * time.Minute
)

// destinationsState is what the agent keeps of the destinations jobs connect to.
type destinationsState struct {
	book *seen.Book
	// active is set when the box has nft, so the rule is applied and kept.
	active bool

	mu sync.Mutex
	// problem is why destinations are not recorded, "" while they are.
	problem string
	sources map[netip.Addr]source
	// window and logged limit the log lines about new destinations.
	window time.Time
	logged int
}

// source is the container at an address on a job network. Its repo is empty
// when no runner of the agent has the address, as for a container someone
// started on the network by hand or one that finished a moment ago.
type source struct {
	ok           bool
	runner, repo string
	// net is the job's network, which a connection stays inside of when it
	// goes to another container.
	net netip.Prefix
	// server is the DNS forwarder of the job's class, when it has one.
	server *dnsfwd.Server
	until  time.Time
}

func (d *destinationsState) setProblem(why string) (changed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	changed = d.problem != why
	d.problem = why
	return changed
}

func (d *destinationsState) why() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.problem
}

// convergeDestinations puts the rule that logs new connections in place. A
// box without nft records nothing and says so in status.
func (a *Agent) convergeDestinations(ctx context.Context) {
	if _, err := a.Run(ctx, "", "nft", "--version"); err != nil {
		a.dest.setProblem("nft is not installed, so the destinations jobs connect to are not recorded; install nftables and restart boxrunner")
		a.Log.Warn("not recording the destinations jobs connect to: nft is not installed")
		return
	}
	a.dest.active = true
	if err := seen.Apply(ctx, a.Run); err != nil {
		a.dest.setProblem("the rule that logs new connections could not be applied: " + err.Error())
		a.Log.Error("the rule that logs the destinations jobs connect to could not be applied", "err", err)
	}
}

// verifyDestinations puts the rule back when it is gone, as after `nft flush
// ruleset`.
func (a *Agent) verifyDestinations(ctx context.Context) {
	if !a.dest.active || seen.Intact(ctx, a.Run) {
		return
	}
	if err := seen.Apply(ctx, a.Run); err != nil {
		if a.dest.setProblem("the rule that logs new connections is gone and could not be put back: "+err.Error()) && ctx.Err() == nil {
			a.Log.Error("the rule that logs the destinations jobs connect to is gone and could not be put back", "err", err)
		}
		return
	}
	a.dest.setProblem("")
	a.Log.Warn("the rule that logs the destinations jobs connect to was gone; put it back")
}

// logSeen reads the logged new connections until ctx ends.
func (a *Agent) logSeen(ctx context.Context) {
	a.readLog(ctx, layout.SeenNFLogGroup, a.onSeenPacket,
		"cannot read the log of new connections; destinations are not recorded until it opens")
}

// saveSeen writes the book to disk now and then, and when the agent stops.
func (a *Agent) saveSeen(ctx context.Context) {
	every := cmpDuration(a.SaveDestinationsEvery, saveDestinationsEvery)
	save := func() {
		if err := a.dest.book.Save(a.DestinationsFile); err != nil {
			a.Log.Warn("saving the destinations jobs connected to", "err", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-time.After(every):
			save()
		}
	}
}

func (a *Agent) onSeenPacket(ctx context.Context, p nflog.Packet) {
	src, dst := p.Src.Unmap(), p.Dst.Unmap()
	who := a.sourceOf(ctx, src)
	if !who.ok {
		return
	}
	// A job talking to a container on its own network is not leaving it.
	if who.net.Contains(dst) {
		return
	}
	name := ""
	if who.server != nil {
		name, _ = who.server.NameOf(dst)
	}
	d, isNew := a.dest.book.Note(who.repo, dst, p.DstPort, p.Proto, name)
	if isNew && a.dest.mayLog(time.Now()) {
		a.Log.Info("a job connected somewhere new", "repo", who.repo, "runner", who.runner, "dest", d.String())
	}
}

// mayLog reports whether another line about a new destination fits in the
// minute.
func (d *destinationsState) mayLog(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.window) >= time.Minute {
		d.window, d.logged = now, 0
	}
	d.logged++
	return d.logged <= loggedPerMinute
}

// sourceOf names the job that has the address on one of the job networks and
// the repository it works for.
func (a *Agent) sourceOf(ctx context.Context, addr netip.Addr) source {
	now := time.Now()
	a.dest.mu.Lock()
	if s, ok := a.dest.sources[addr]; ok && now.Before(s.until) {
		a.dest.mu.Unlock()
		return s
	}
	a.dest.mu.Unlock()

	s := a.findSource(ctx, addr)
	s.until = now.Add(noSourceFor)
	if s.repo != "" {
		s.until = now.Add(sourceFor)
	}
	a.dest.mu.Lock()
	defer a.dest.mu.Unlock()
	if a.dest.sources == nil || len(a.dest.sources) >= maxSources {
		a.dest.sources = map[netip.Addr]source{}
	}
	a.dest.sources[addr] = s
	return s
}

func (a *Agent) findSource(ctx context.Context, addr netip.Addr) source {
	name := layout.Network
	var server *dnsfwd.Server
	a.egress.mu.Lock()
	for _, e := range a.egress.classes {
		if e.net.Subnet.Contains(addr) {
			name, server = e.net.Name, e.server
		}
	}
	a.egress.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	network, ok, err := a.Docker.Network(ctx, name)
	if err != nil || !ok {
		return source{}
	}
	for _, ep := range network.Containers {
		prefix, err := netip.ParsePrefix(ep.IPv4Address)
		if err != nil || prefix.Addr() != addr {
			continue
		}
		for _, r := range a.current().Runners {
			if r.Name == ep.Name {
				return source{ok: true, runner: r.Name, repo: r.Repo, net: prefix.Masked(), server: server}
			}
		}
		return source{ok: true, runner: ep.Name, net: prefix.Masked(), server: server}
	}
	// No container has the address, but a packet that came in on the
	// network's bridge with a source inside its subnet is the network's.
	if subnet, found := network.Subnet(); found && subnet.Contains(addr) {
		return source{ok: true, net: subnet, server: server}
	}
	return source{}
}
