package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/dnsfwd"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/nflog"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

// egressClass is a class with an allowlist as the agent serves it.
type egressClass struct {
	class config.Class
	net   egress.Net
	// rules is what the class puts in the table.
	rules  egress.Class
	server *dnsfwd.Server
	// problem is why the class is not served, "" when it is. A class is served
	// only once its network, the nftables rules and the host's rules are in place.
	problem string
}

// egressState is what the agent keeps for its egress classes.
type egressState struct {
	mu       sync.Mutex
	classes  map[string]*egressClass
	enforcer *egress.Enforcer
	// ruleset is what the agent last put in the table.
	ruleset egress.Ruleset
	// verify orders the checks that put the rules back, which the guard and
	// each runner start run.
	verify sync.Mutex
	ledger *egress.Ledger
	// baseline are the packets the table had counted before this agent started.
	baseline map[string]uint64
}

func (a *Agent) egressClass(name string) (*egressClass, bool) {
	a.egress.mu.Lock()
	defer a.egress.mu.Unlock()
	c, ok := a.egress.classes[name]
	return c, ok
}

// egressServes reports whether a class has no allowlist, or has one in force.
func (a *Agent) egressServes(c config.Class) bool {
	if c.Egress == nil {
		return true
	}
	e, ok := a.egressClass(c.Name)
	return ok && e.problem == ""
}

// egressNet is the network a class's jobs run on, or nil for the job network.
func (a *Agent) egressNet(class string) *egress.Net {
	if e, ok := a.egressClass(class); ok && e.problem == "" {
		return &e.net
	}
	return nil
}

// convergeEgress makes the box what the egress classes need: a network each,
// the nftables table, the host's input rules for their bridges. A class for
// which any of it fails is not served, so its jobs never run with less than
// its allowlist. Networks of classes that left the config are removed once
// no job runs on them; until then they stay closed.
func (a *Agent) convergeEgress(ctx context.Context, services firewall.Services, mirrorGateway netip.Addr) {
	var configured []config.Class
	for _, c := range a.Config.Classes {
		if c.Egress != nil {
			configured = append(configured, c)
		}
	}
	a.egress.classes = map[string]*egressClass{}
	left, err := a.Docker.Networks(ctx, layout.LabelEgress)
	if err != nil {
		a.failEgress(configured, fmt.Sprintf("listing Docker's networks: %v", err))
		return
	}
	if len(configured) == 0 && len(left) == 0 && !egress.Present(ctx, a.Run) {
		return
	}
	if _, err := a.Run(ctx, "", "nft", "--version"); err != nil {
		a.failEgress(configured, "nft is not installed; run sudo boxrunner install")
		return
	}

	// The box's networks are out of every class's reach, whatever its entries cover.
	all, err := a.Docker.Networks(ctx, "")
	if err != nil {
		a.failEgress(configured, fmt.Sprintf("reading Docker's networks: %v", err))
		return
	}
	var classes []egress.Class
	var nets []firewall.EgressNet
	var deny []netip.Prefix
	for _, n := range all {
		if subnet, ok := n.Subnet(); ok {
			deny = append(deny, subnet)
		}
	}
	existing := map[string]docker.Network{}
	for _, n := range left {
		existing[n.Labels[layout.LabelEgress]] = n
	}
	// closed keeps a network that jobs may still run on shut: they keep what
	// they have open, and nothing new.
	closed := func(class string, n docker.Network) {
		en := egress.NetOf(class)
		var subnetOK, gatewayOK bool
		en.Subnet, subnetOK = n.Subnet()
		en.Gateway, gatewayOK = n.Gateway()
		if subnetOK && gatewayOK {
			classes = append(classes, egress.Class{Net: en, Closed: true})
			nets = append(nets, firewall.EgressNet{Bridge: en.Bridge, Gateway: en.Gateway})
		}
	}
	served := map[string]bool{}
	for _, c := range configured {
		served[c.Name] = true
		n, problem := a.ensureEgressNet(ctx, c.Name)
		e := &egressClass{class: c, net: n, problem: problem}
		a.egress.classes[c.Name] = e
		if problem != "" {
			a.Log.Error("not serving the class; its egress allowlist cannot be put in force", "class", c.Name, "problem", problem)
			if old, ok := existing[c.Name]; ok {
				closed(c.Name, old)
			}
			continue
		}
		host := egress.HostNet(n, c.Docker, c.Cache, services, mirrorGateway)
		e.rules = egress.Class{Net: n, Policy: *c.Egress, Open: host.Open}
		classes = append(classes, e.rules)
		nets = append(nets, host)
	}
	for class, n := range existing {
		if served[class] {
			continue
		}
		if len(n.Containers) == 0 {
			if err := a.Docker.RemoveNetwork(ctx, n.Name); err != nil {
				a.Log.Warn("removing the network of a class that left the config", "class", class, "err", err)
			}
			continue
		}
		a.Log.Warn("a class left the config or lost its egress allowlist while its jobs run; their network is closed to new connections until they finish", "class", class)
		closed(class, n)
	}

	if len(classes) == 0 && len(configured) == 0 {
		// Nothing of the egress rules is left to keep.
		if err := errors.Join(egress.Remove(ctx, a.Run), firewall.ApplyEgress(ctx, a.Run, nil)); err != nil {
			a.Log.Warn("removing the rules of egress classes that are gone", "err", err)
		}
		return
	}
	a.egress.ruleset = egress.Ruleset{Classes: classes, Deny: deny, Group: layout.NFLogGroup}
	if err := a.egress.enforcer.Apply(ctx, a.egress.ruleset); err != nil {
		a.failEgress(configured, err.Error())
		return
	}
	if err := firewall.ApplyEgress(ctx, a.Run, nets); err != nil {
		a.failEgress(configured, err.Error())
		return
	}
	if counts, err := a.egress.enforcer.Blocked(ctx); err == nil {
		a.egress.baseline = counts
	}
	for _, e := range a.egress.classes {
		if e.problem != "" {
			continue
		}
		upstream := a.EgressUpstream
		if upstream == nil {
			upstream = &dnsfwd.Resolver{}
		}
		e.server = &dnsfwd.Server{
			Policy: *e.class.Egress, Clients: e.net.Subnet, Upstream: upstream,
			Open: func(ctx context.Context, g []egress.Grant) (time.Duration, error) {
				return a.egress.enforcer.Open(ctx, e.rules, g)
			},
			Event: func(ev dnsfwd.Event) { a.onLookup(e, ev) },
		}
	}
}

// failEgress marks every configured class as not served.
func (a *Agent) failEgress(configured []config.Class, problem string) {
	for _, c := range configured {
		a.egress.classes[c.Name] = &egressClass{class: c, net: egress.NetOf(c.Name), problem: problem}
	}
	if len(configured) > 0 {
		a.Log.Error("not serving the classes with an egress allowlist", "problem", problem)
	}
}

// ensureEgressNet finds or creates a class's network.
func (a *Agent) ensureEgressNet(ctx context.Context, class string) (egress.Net, string) {
	n := egress.NetOf(class)
	network, ok, err := a.Docker.Network(ctx, n.Name)
	if err == nil && !ok {
		err = a.Docker.CreateNetwork(ctx, docker.NetworkSpec{Name: n.Name, Bridge: n.Bridge, Labels: map[string]string{layout.LabelEgress: class}})
		if err == nil {
			network, ok, err = a.Docker.Network(ctx, n.Name)
		}
	}
	switch {
	case err != nil:
		return n, fmt.Sprintf("creating the Docker network %s: %v", n.Name, err)
	case !ok:
		return n, fmt.Sprintf("the Docker network %s is missing", n.Name)
	case network.Driver != "bridge" || network.Options["com.docker.network.bridge.name"] != n.Bridge || network.Labels[layout.LabelEgress] != class:
		return n, fmt.Sprintf("a Docker network named %s exists but is not boxrunner's; remove it with `docker network rm %s`", n.Name, n.Name)
	}
	var found bool
	if n.Subnet, found = network.Subnet(); !found {
		return n, fmt.Sprintf("the Docker network %s has no IPv4 subnet", n.Name)
	}
	if n.Gateway, found = network.Gateway(); !found {
		return n, fmt.Sprintf("the Docker network %s has no IPv4 gateway", n.Name)
	}
	return n, ""
}

// PacketSource is where the egress rules' logged packets come from.
type PacketSource interface {
	Read() ([]nflog.Packet, error)
	Close() error
}

func openPackets(group uint16) (PacketSource, error) { return nflog.Open(group) }

// serveEgress starts the forwarder of each class that is ready, and the
// readers of what the rules refuse. It returns a function that stops them.
func (a *Agent) serveEgress(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, e := range a.egress.classes {
		if e.problem == "" {
			if err := a.serveForwarder(ctx, &wg, e); err != nil {
				e.problem = err.Error()
				a.Log.Error("not serving the class; its DNS forwarder cannot start", "class", e.class.Name, "err", err)
			}
		}
	}
	if len(a.egress.classes) > 0 || len(a.egress.ruleset.Classes) > 0 {
		wg.Go(func() { a.countBlocked(ctx) })
		wg.Go(func() { a.logBlocked(ctx) })
		wg.Go(func() { a.guardEgress(ctx) })
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

func (a *Agent) serveForwarder(ctx context.Context, wg *sync.WaitGroup, e *egressClass) error {
	listen := a.EgressListen
	if listen == nil {
		listen = func(n egress.Net) (net.PacketConn, net.Listener, error) {
			addr := netip.AddrPortFrom(n.Gateway, 53).String()
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				return nil, nil, err
			}
			l, err := net.Listen("tcp", addr)
			if err != nil {
				pc.Close()
				return nil, nil, err
			}
			return pc, l, nil
		}
	}
	pc, l, err := listen(e.net)
	if err != nil {
		return err
	}
	a.Log.Info("serving DNS for the class's jobs", "class", e.class.Name, "addr", pc.LocalAddr().String(), "entries", len(e.class.Egress.Entries))
	wg.Go(func() {
		if err := e.server.ServeUDP(ctx, pc); err != nil {
			a.Log.Error("serving DNS over UDP", "class", e.class.Name, "err", err)
		}
	})
	wg.Go(func() {
		if err := e.server.ServeTCP(ctx, l); err != nil {
			a.Log.Error("serving DNS over TCP", "class", e.class.Name, "err", err)
		}
	})
	return nil
}

// countBlocked reads the packets the table has dropped every few seconds. The
// count only grows: when the table is replaced its counters start again, and
// what they count then is all new.
func (a *Agent) countBlocked(ctx context.Context) {
	interval := cmpDuration(a.EgressCountEvery, 5*time.Second)
	last := maps.Clone(a.egress.baseline)
	reported := map[string]uint64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		counts, err := a.egress.enforcer.Blocked(ctx)
		if err != nil {
			if ctx.Err() == nil {
				a.Log.Warn("reading the egress rules' counters", "err", err)
			}
			continue
		}
		for id, now := range counts {
			if now >= last[id] {
				reported[id] += now - last[id]
			} else {
				reported[id] += now
			}
			last[id] = now
		}
		a.egress.mu.Lock()
		for _, e := range a.egress.classes {
			a.egress.ledger.SetPackets(e.class.Name, reported[e.net.ID()])
		}
		a.egress.mu.Unlock()
	}
}

func cmpDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// logBlocked reads the packets the table logged as it dropped them, and says
// who sent them where.
func (a *Agent) logBlocked(ctx context.Context) {
	open := a.OpenPackets
	if open == nil {
		open = openPackets
	}
	src, err := open(layout.NFLogGroup)
	if err != nil {
		a.Log.Warn("cannot read the egress rules' log; refused packets are counted but not described", "err", err)
		return
	}
	stop := context.AfterFunc(ctx, func() { src.Close() })
	defer stop()
	for {
		packets, err := src.Read()
		if err != nil {
			if ctx.Err() == nil {
				a.Log.Warn("reading the egress rules' log", "err", err)
			}
			return
		}
		for _, p := range packets {
			a.onBlockedPacket(ctx, p)
		}
	}
}

func (a *Agent) onBlockedPacket(ctx context.Context, p nflog.Packet) {
	a.egress.mu.Lock()
	var e *egressClass
	for _, c := range a.egress.classes {
		if c.net.ID() == p.Prefix {
			e = c
		}
	}
	a.egress.mu.Unlock()
	if e == nil {
		return
	}
	dest := netip.AddrPortFrom(p.Dst.Unmap(), p.DstPort).String()
	if p.Proto != 6 && p.Proto != 17 {
		dest = p.Dst.Unmap().String() + " (protocol " + fmt.Sprint(p.Proto) + ")"
	}
	if !a.egress.ledger.Admit(e.class.Name, egress.Packet, dest) {
		return
	}
	b := egress.Block{Kind: egress.Packet, Dest: dest}
	if e.server != nil {
		b.Name, _ = e.server.NameOf(p.Dst.Unmap())
	}
	b.Runner, b.Repo = a.jobAt(ctx, e.net, p.Src.Unmap())
	a.egress.ledger.Keep(e.class.Name, b)
	a.Log.Warn("egress refused a connection", "class", e.class.Name, "runner", b.Runner, "repo", b.Repo, "dest", b.Dest, "name", b.Name, "proto", p.Proto)
}

func (a *Agent) onLookup(e *egressClass, ev dnsfwd.Event) {
	switch ev.Kind {
	case dnsfwd.Refused:
		a.egress.ledger.CountLookup(e.class.Name)
		if !a.egress.ledger.Admit(e.class.Name, egress.Lookup, ev.Name) {
			return
		}
		// Asking Docker who the job is takes a moment the job's refusal need not wait for.
		go func() {
			b := egress.Block{Kind: egress.Lookup, Dest: ev.Name}
			b.Runner, b.Repo = a.jobAt(context.Background(), e.net, ev.From)
			a.egress.ledger.Keep(e.class.Name, b)
			a.Log.Warn("egress refused a lookup", "class", e.class.Name, "runner", b.Runner, "repo", b.Repo, "name", ev.Name)
		}()
	case dnsfwd.Withheld:
		if a.egress.ledger.Admit(e.class.Name, "withheld", ev.Name+" "+ev.Addr.String()) {
			a.Log.Warn("egress did not open an address an allowed name resolved to", "class", e.class.Name, "name", ev.Name, "addr", ev.Addr, "why", ev.Reason)
		}
	case dnsfwd.Failed:
		if a.egress.ledger.Admit(e.class.Name, "failed", ev.Name) {
			a.Log.Error("egress could not answer an allowed name", "class", e.class.Name, "name", ev.Name, "err", ev.Err)
		}
	}
}

// jobAt names the runner whose container has the address on the class's
// network, and the repository it works for.
func (a *Agent) jobAt(ctx context.Context, n egress.Net, addr netip.Addr) (runner, repo string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	network, ok, err := a.Docker.Network(ctx, n.Name)
	if err != nil || !ok {
		return "", ""
	}
	for _, ep := range network.Containers {
		if at, ok := ep.Addr(); ok && at == addr {
			runner = ep.Name
		}
	}
	runners := a.current().Runners
	if i := slices.IndexFunc(runners, func(r status.Runner) bool { return r.Name == runner }); i >= 0 {
		repo = runners[i].Repo
	}
	return runner, repo
}

// egressView is what status shows of the classes with an allowlist.
func (a *Agent) egressView() []status.Egress {
	var out []status.Egress
	for _, c := range a.Config.Classes {
		if c.Egress == nil {
			continue
		}
		view := status.Egress{Class: c.Name, Entries: c.Egress.Entries}
		if e, ok := a.egressClass(c.Name); !ok {
			view.Problem = "the agent has not set it up yet"
		} else {
			view.Problem = e.problem
		}
		counts := a.egress.ledger.Counts(c.Name)
		view.Packets, view.Lookups, view.Recent = counts.Packets, counts.Lookups, counts.Recent
		out = append(out, view)
	}
	return out
}

// guardEgress puts the table back when someone flushed it. The bridges stay
// closed meanwhile, since the host's rules drop what the table did not mark.
func (a *Agent) guardEgress(ctx context.Context) {
	interval := cmpDuration(a.EgressGuardEvery, 10*time.Second)
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		// The table is one command to check and the host's rules take dozens,
		// and the backstop covers a gap in them, so they are checked less often.
		if err := a.verifyEgress(ctx, tick%6 == 0); err != nil && ctx.Err() == nil {
			a.Log.Error("the egress rules are gone and could not be put back; jobs of the classes with an allowlist have no network", "err", err)
		}
	}
}

// verifyEgress makes sure the table, and with host the host's rules too, are
// in place, and puts them back when they are not.
func (a *Agent) verifyEgress(ctx context.Context, host bool) error {
	a.egress.verify.Lock()
	defer a.egress.verify.Unlock()
	a.egress.mu.Lock()
	rules := a.egress.ruleset
	a.egress.mu.Unlock()
	if len(rules.Classes) == 0 {
		return nil
	}
	whole, err := a.egress.enforcer.Verify(ctx, rules)
	if err != nil {
		return err
	}
	if !whole {
		a.Log.Warn("the egress rules were gone; put them back")
	}
	if host && firewall.CheckEgress(ctx, a.Run, a.hostNets()) != firewall.Current {
		if err := firewall.ApplyEgress(ctx, a.Run, a.hostNets()); err != nil {
			return err
		}
		a.Log.Warn("the host's rules for the egress bridges were incomplete; put them back")
	}
	return nil
}

// hostNets are the host's input rules for the bridges the table holds.
func (a *Agent) hostNets() []firewall.EgressNet {
	a.egress.mu.Lock()
	defer a.egress.mu.Unlock()
	var nets []firewall.EgressNet
	for _, c := range a.egress.ruleset.Classes {
		nets = append(nets, firewall.EgressNet{Bridge: c.Net.Bridge, Gateway: c.Net.Gateway, Open: c.Open})
	}
	return nets
}
