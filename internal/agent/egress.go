package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
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
	// problem is why the agent could not set the class up when it started.
	// lost and dns are why it stopped serving the class since then: its
	// network is gone, or its DNS forwarder stopped. The class is served only
	// while all three are "" and the rules are in place.
	problem, lost, dns string
	// restart makes the forwarder listen again.
	restart func()
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
	// broken is why the rules are not in place, "" while they are.
	broken string
	// boxNets are the subnets of Docker's networks as the guard last listed
	// them. The table denies them all once verifyEgress has run.
	boxNets []netip.Prefix
	// purge is set while jobs that ran without the table may still run. The
	// verify lock guards it.
	purge bool
}

// why is why a class is not served, "" when it is. The caller holds mu.
func (s *egressState) why(e *egressClass) string {
	return cmp.Or(e.problem, s.broken, e.lost, e.dns)
}

// egressProblem is why a class with an allowlist is not served, "" when it
// is, and false when the agent has not set the class up.
func (a *Agent) egressProblem(name string) (string, bool) {
	a.egress.mu.Lock()
	defer a.egress.mu.Unlock()
	e, ok := a.egress.classes[name]
	if !ok {
		return "", false
	}
	return a.egress.why(e), true
}

// egressServes reports whether a class has no allowlist, or has one in force.
func (a *Agent) egressServes(c config.Class) bool {
	if c.Egress == nil {
		return true
	}
	problem, ok := a.egressProblem(c.Name)
	return ok && problem == ""
}

// egressNet is the network of a class with an allowlist in force, nil when
// the class has none or it is not in force.
func (a *Agent) egressNet(class string) *egress.Net {
	a.egress.mu.Lock()
	defer a.egress.mu.Unlock()
	e, ok := a.egress.classes[class]
	if !ok || a.egress.why(e) != "" {
		return nil
	}
	n := e.net
	return &n
}

// mark sets one of the reasons a class is not served, and reports whether it
// changed. A change makes supervise start or stop the class's listeners now.
func (a *Agent) mark(reason *string, why string) bool {
	a.egress.mu.Lock()
	changed := *reason != why
	*reason = why
	a.egress.mu.Unlock()
	if changed {
		select {
		case a.resync <- struct{}{}:
		default:
		}
	}
	return changed
}

// convergeEgress makes the box what the egress classes need: a network each,
// the nftables table, the host's input rules for their bridges. A class for
// which any of it fails is not served, so its jobs never run with less than
// its allowlist. Jobs left on the classes' networks from before the agent
// started are removed first. Networks of classes that left the config are
// removed once no job runs on them; until then they stay closed.
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
	// A job that ran while the agent was down had no one to put its rules
	// back, so it may hold a connection they refuse. What cannot be removed
	// keeps its network closed below.
	for i, n := range left {
		left[i].Containers = a.removeJobs(ctx, n, 0, "it ran while the agent was not running")
	}
	if _, err := a.Run(ctx, "", "nft", "--version"); err != nil {
		a.failEgress(configured, "nft is not installed; run sudo boxrunner install")
		return
	}

	// The classes' networks are made before the box's networks are listed, so
	// a first start denies them as a restart does.
	type setup struct {
		net     egress.Net
		problem string
	}
	setups := map[string]setup{}
	for _, c := range configured {
		n, problem := a.ensureEgressNet(ctx, c.Name)
		setups[c.Name] = setup{n, problem}
	}
	// The box's networks are out of every class's reach, whatever its entries cover.
	all, err := a.Docker.Networks(ctx, "")
	if err != nil {
		a.failEgress(configured, fmt.Sprintf("reading Docker's networks: %v", err))
		return
	}
	var classes []egress.Class
	var nets []firewall.EgressNet
	deny := subnets(all, nil)
	a.egress.boxNets = deny
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
		n, problem := setups[c.Name].net, setups[c.Name].problem
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

// subnets are the IPv4 subnets of Docker's networks and of the classes,
// sorted.
func subnets(all []docker.Network, classes []egress.Class) []netip.Prefix {
	var out []netip.Prefix
	for _, n := range all {
		if subnet, ok := n.Subnet(); ok {
			out = append(out, subnet)
		}
	}
	for _, c := range classes {
		out = append(out, c.Net.Subnet)
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
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

// sameNet reports whether a Docker network is still the class's as the rules know it.
func sameNet(network docker.Network, n egress.Net) bool {
	subnet, _ := network.Subnet()
	gateway, _ := network.Gateway()
	return subnet == n.Subnet && gateway == n.Gateway && network.Options["com.docker.network.bridge.name"] == n.Bridge &&
		network.Labels[layout.LabelEgress] == n.Class
}

// checkEgressNets makes a served class's network again when someone removed
// it, as `docker network prune` does while no job runs on it. It comes back
// on the same subnet and bridge, so the rules hold as they are, and the
// forwarder listens again on its gateway. Until then the class is not served.
func (a *Agent) checkEgressNets(ctx context.Context) {
	a.egress.mu.Lock()
	var classes []*egressClass
	for _, e := range a.egress.classes {
		if e.problem == "" {
			classes = append(classes, e)
		}
	}
	a.egress.mu.Unlock()
	for _, e := range classes {
		n := e.net
		network, ok, err := a.Docker.Network(ctx, n.Name)
		if err != nil {
			continue
		}
		why := ""
		switch {
		case ok && sameNet(network, n):
		case ok:
			why = fmt.Sprintf("its Docker network %s is no longer the one its rules name; restart boxrunner", n.Name)
		default:
			err := a.Docker.CreateNetwork(ctx, docker.NetworkSpec{Name: n.Name, Bridge: n.Bridge, Labels: map[string]string{layout.LabelEgress: n.Class}, Subnet: n.Subnet, Gateway: n.Gateway})
			if err == nil {
				if network, ok, err = a.Docker.Network(ctx, n.Name); err == nil && (!ok || !sameNet(network, n)) {
					err = errors.New("it came back with another subnet")
				}
			}
			if err != nil {
				why = fmt.Sprintf("its Docker network %s was removed and could not be made again on %s: %v; restart boxrunner", n.Name, n.Subnet, err)
				break
			}
			a.Log.Warn("the class's Docker network was removed; made it again", "class", n.Class, "network", n.Name, "subnet", n.Subnet)
			a.egress.mu.Lock()
			restart := e.restart
			a.egress.mu.Unlock()
			if restart != nil {
				restart()
			}
		}
		if a.mark(&e.lost, why) && why != "" {
			a.Log.Error("not serving the class", "class", n.Class, "problem", why)
		}
	}
}

// removeJobs removes the containers on an egress class's network, each after
// grace to stop, and returns those it could not remove.
func (a *Agent) removeJobs(ctx context.Context, n docker.Network, grace time.Duration, why string) map[string]docker.Endpoint {
	var mu sync.Mutex
	var wg sync.WaitGroup
	left := map[string]docker.Endpoint{}
	for id, ep := range n.Containers {
		wg.Go(func() {
			if grace > 0 {
				if err := a.Docker.Stop(ctx, id, grace); err != nil {
					a.Log.Warn("stopping a job of an egress class failed; removing it", "runner", ep.Name, "err", err)
				}
			}
			if err := a.Docker.Remove(ctx, id); err != nil {
				a.Log.Error("removing a job of an egress class failed", "runner", ep.Name, "network", n.Name, "err", err)
				mu.Lock()
				left[id] = ep
				mu.Unlock()
				return
			}
			a.Log.Warn("removed a job of an egress class", "runner", ep.Name, "network", n.Name, "why", why)
		})
	}
	wg.Wait()
	return left
}

// stopEgressJobs removes every container on the egress classes' networks. It
// fails when it could not list them all or remove one.
func (a *Agent) stopEgressJobs(ctx context.Context, grace time.Duration, why string) error {
	nets, err := a.Docker.Networks(ctx, layout.LabelEgress)
	if err != nil {
		a.Log.Error("listing the egress classes' networks to remove their jobs", "err", err)
		return err
	}
	for _, n := range nets {
		if left := a.removeJobs(ctx, n, grace, why); len(left) > 0 {
			err = errors.New("some jobs of egress classes could not be removed")
		}
	}
	return err
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
		if e.problem == "" && e.server != nil {
			// The first sockets open before supervise starts, so the class
			// is served only once its forwarder listens.
			pc, l, err := a.listenDNS(e.net)
			if err != nil {
				a.forwarderStopped(e, err)
			}
			wg.Go(func() { a.serveForwarder(ctx, e, pc, l) })
		}
	}
	if len(a.egress.classes) > 0 || len(a.egress.ruleset.Classes) > 0 {
		wg.Go(func() { a.countBlocked(ctx) })
		wg.Go(func() { a.logBlocked(ctx) })
	}
	if a.dest.active {
		wg.Go(func() { a.logSeen(ctx) })
	}
	wg.Go(func() { a.saveSeen(ctx) })
	wg.Go(func() { a.guard(ctx) })
	return func() {
		cancel()
		wg.Wait()
	}
}

// listenDNS opens the sockets of a class's forwarder.
func (a *Agent) listenDNS(n egress.Net) (net.PacketConn, net.Listener, error) {
	if a.EgressListen != nil {
		return a.EgressListen(n)
	}
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

func (a *Agent) forwarderStopped(e *egressClass, err error) {
	if a.mark(&e.dns, "its DNS forwarder stopped: "+err.Error()) {
		a.Log.Error("not serving the class until its DNS forwarder listens again", "class", e.class.Name, "err", err)
	}
}

// serveForwarder keeps a class's forwarder answering until ctx ends, on the
// sockets given and then on new ones whenever it stops. While it does not
// listen, the class is not served.
func (a *Agent) serveForwarder(ctx context.Context, e *egressClass, pc net.PacketConn, l net.Listener) {
	retry := cmpDuration(a.EgressRetry, time.Second)
	wait := retry
	for {
		var err error
		if pc == nil {
			pc, l, err = a.listenDNS(e.net)
		}
		if err == nil {
			err = a.forward(ctx, e, pc, l)
			pc, l = nil, nil
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Asked to listen again.
			wait = retry
			continue
		}
		a.forwarderStopped(e, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// forward answers on the sockets until ctx ends, the class's restart is
// called or a server fails, which is the only time it returns an error.
func (a *Agent) forward(ctx context.Context, e *egressClass, pc net.PacketConn, l net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.egress.mu.Lock()
	e.restart = cancel
	a.egress.mu.Unlock()
	a.mark(&e.dns, "")
	a.Log.Info("serving DNS for the class's jobs", "class", e.class.Name, "addr", pc.LocalAddr().String(), "entries", len(e.class.Egress.Entries))
	errs := make(chan error, 2)
	go func() { errs <- e.server.ServeUDP(ctx, pc) }()
	go func() { errs <- e.server.ServeTCP(ctx, l) }()
	first := <-errs
	cancel()
	return errors.Join(first, <-errs)
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
	a.readLog(ctx, layout.NFLogGroup, a.onBlockedPacket,
		"cannot read the egress rules' log; refused packets are counted, and described again once it opens")
}

// readLog hands every packet logged to a group to handle until ctx ends. When
// reading fails it says why, with complaint, and opens the log again.
func (a *Agent) readLog(ctx context.Context, group uint16, handle func(context.Context, nflog.Packet), complaint string) {
	open := a.OpenPackets
	if open == nil {
		open = openPackets
	}
	retry := cmpDuration(a.EgressRetry, time.Second)
	wait := retry
	for {
		read, err := readPackets(ctx, open, group, handle)
		if ctx.Err() != nil {
			return
		}
		if read {
			wait = retry
		}
		a.Log.Warn(complaint, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// readPackets reads the log until it fails, and reports whether it read anything.
func readPackets(ctx context.Context, open func(uint16) (PacketSource, error), group uint16, handle func(context.Context, nflog.Packet)) (bool, error) {
	src, err := open(group)
	if err != nil {
		return false, err
	}
	defer src.Close()
	stop := context.AfterFunc(ctx, func() { src.Close() })
	defer stop()
	read := false
	for {
		packets, err := src.Read()
		if err != nil {
			return read, err
		}
		read = true
		for _, p := range packets {
			handle(ctx, p)
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
		if problem, ok := a.egressProblem(c.Name); !ok {
			view.Problem = "the agent has not set it up yet"
		} else {
			view.Problem = problem
		}
		counts := a.egress.ledger.Counts(c.Name)
		view.Packets, view.Lookups, view.Recent = counts.Packets, counts.Lookups, counts.Recent
		out = append(out, view)
	}
	return out
}

// guard puts the firewall rules back when someone deleted them, and the
// egress classes' networks, and denies the classes the box's networks as
// they come and go.
func (a *Agent) guard(ctx context.Context) {
	interval := cmpDuration(a.EgressGuardEvery, 10*time.Second)
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if all, err := a.Docker.Networks(ctx, ""); err == nil {
			a.egress.mu.Lock()
			// A class's own network stays denied while checkEgressNets makes it again.
			a.egress.boxNets = subnets(all, a.egress.ruleset.Classes)
			a.egress.mu.Unlock()
		} else if ctx.Err() == nil {
			a.Log.Warn("listing Docker's networks to deny new ones to the egress classes", "err", err)
		}
		// The table is one command to check and the host's rules take dozens,
		// so they are checked less often. The egress bridges' rules matter
		// only without the table: its drops are final whatever iptables says.
		host := tick%6 == 0
		if host {
			a.egress.verify.Lock()
			a.verifyJobRules(ctx)
			a.egress.verify.Unlock()
		}
		if err := a.verifyEgress(ctx, host); err != nil && ctx.Err() == nil {
			a.Log.Error("the egress rules are gone and could not be put back; not serving the classes with an allowlist", "err", err)
		}
		a.checkEgressNets(ctx)
		a.verifyDestinations(ctx)
	}
}

// verifyEgress makes sure the table, and with host the host's rules too, are
// in place, and puts them back when they are not. When the box's networks
// changed since the table was built, as when another tool made one, it builds
// the classes' chains again in one transaction that keeps their sets, so
// what jobs have open stays open. A missing table means the
// classes' jobs ran without it, and `nft flush ruleset` takes the host's
// backstop with it, so a job may hold a connection the rules refuse: every
// job of an egress class is removed. While the rules cannot be put back, no
// egress class is served.
func (a *Agent) verifyEgress(ctx context.Context, host bool) error {
	a.egress.verify.Lock()
	defer a.egress.verify.Unlock()
	a.egress.mu.Lock()
	rules, boxNets := a.egress.ruleset, a.egress.boxNets
	a.egress.mu.Unlock()
	if len(rules.Classes) == 0 {
		return nil
	}
	whole, err := a.egress.enforcer.Verify(ctx, rules)
	if !whole {
		a.egress.purge, host = true, true
		// What took the table, such as `nft flush ruleset`, may have taken
		// the job network's rules too.
		a.verifyJobRules(ctx)
	}
	why := "the egress rules are gone and could not be put back: "
	if err == nil && !slices.Equal(boxNets, rules.Deny) {
		added, removed := diff(boxNets, rules.Deny), diff(rules.Deny, boxNets)
		rules.Deny = boxNets
		if err = a.egress.enforcer.Apply(ctx, rules); err == nil {
			a.egress.mu.Lock()
			a.egress.ruleset = rules
			a.egress.mu.Unlock()
			a.Log.Info("the box's Docker networks changed; denied them to the egress classes", "added", added, "removed", removed)
		} else {
			why = "the box's Docker networks changed and the egress rules could not deny them: "
		}
	}
	if err == nil && host && firewall.CheckEgress(ctx, a.Run, a.hostNets()) != firewall.Current {
		if err = firewall.ApplyEgress(ctx, a.Run, a.hostNets()); err == nil {
			a.Log.Warn("the host's rules for the egress bridges were incomplete; put them back")
		}
	}
	if err == nil && !whole {
		a.Log.Warn("the egress rules were gone; put them back")
	}
	if a.egress.purge && a.stopEgressJobs(ctx, 0, "the egress rules were gone while it ran") == nil {
		a.egress.purge = false
	}
	if err == nil {
		why = ""
	} else {
		why += err.Error()
	}
	a.mark(&a.egress.broken, why)
	return err
}

// diff are the prefixes of a that b does not have.
func diff(a, b []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range a {
		if !slices.Contains(b, p) {
			out = append(out, p)
		}
	}
	return out
}

// verifyJobRules puts the job network's firewall rules back when they are
// not what the agent applied when it started, as after `nft flush ruleset`.
// Without them the jobs of classes without an allowlist reach private
// addresses. They go before the egress bridges' rules, so the egress chain's
// jump lands ahead of theirs. The caller holds the verify lock.
func (a *Agent) verifyJobRules(ctx context.Context) {
	if a.jobRules.Bridge == "" || firewall.Check(ctx, a.Run, a.jobRules) == firewall.Current {
		return
	}
	if err := firewall.Apply(ctx, a.Run, a.jobRules); err != nil {
		if ctx.Err() == nil {
			a.Log.Error("the job network's firewall rules are incomplete and could not be put back", "err", err)
		}
		return
	}
	a.Log.Warn("the job network's firewall rules were incomplete; put them back")
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
