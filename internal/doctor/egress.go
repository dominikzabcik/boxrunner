package doctor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/dnsfwd"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

// egressChecks checks each class that has an allowlist: that its rules are in
// place, what the agent says it refused, and, from a container on the
// class's own network, what a job can and cannot reach.
func (c Checks) egressChecks(ctx context.Context, d *docker.Client, services firewall.Services, mirrorGateway netip.Addr, sysbox bool, snap *status.Snapshot) []Result {
	var classes []config.Class
	for _, k := range c.Config.ResolveIsolation(sysbox).Classes {
		if k.Egress != nil {
			classes = append(classes, k)
		}
	}
	if len(classes) == 0 {
		return nil
	}
	var results []Result
	add := func(class, what string, ok bool, format string, args ...any) {
		results = append(results, Result{"egress " + class + " " + what, ok, fmt.Sprintf(format, args...)})
	}
	if _, err := c.Exec(ctx, "", "nft", "--version"); err != nil {
		for _, k := range classes {
			add(k.Name, "rules", false, "nft is not installed; run sudo boxrunner install")
		}
		return results
	}
	listing, tableErr := c.Exec(ctx, "", "nft", "list", "table", "inet", egress.Table)
	var nets []firewall.EgressNet
	for _, k := range classes {
		n := egress.NetOf(k.Name)
		if network, ok, err := d.Network(ctx, n.Name); err == nil && ok {
			n.Gateway, _ = network.Gateway()
		}
		nets = append(nets, egress.HostNet(n, k.Docker, k.Cache, services, mirrorGateway))
	}
	// The agent also keeps the bridge of a class that left the config while its jobs run.
	configured := map[string]bool{}
	for _, k := range classes {
		configured[k.Name] = true
	}
	if left, err := d.Networks(ctx, layout.LabelEgress); err == nil {
		for _, network := range left {
			class := network.Labels[layout.LabelEgress]
			if gateway, ok := network.Gateway(); !configured[class] && len(network.Containers) > 0 && ok {
				nets = append(nets, firewall.EgressNet{Bridge: egress.NetOf(class).Bridge, Gateway: gateway})
			}
		}
	}
	hostRules := firewall.CheckEgress(ctx, c.Exec, nets)
	for _, k := range classes {
		n := egress.NetOf(k.Name)
		whole := tableErr == nil && egress.Ruleset{Classes: []egress.Class{{Net: n, Policy: *k.Egress}}}.Intact(listing)
		switch {
		case !whole:
			add(k.Name, "rules", false, "the nftables table %s is missing or has no rules for the class; the agent sets it up when it starts", egress.Table)
		case hostRules != firewall.Current:
			add(k.Name, "rules", false, "the host's input rules for the class's bridge %s are incomplete; restart boxrunner", n.Bridge)
		default:
			add(k.Name, "rules", true, "the table %s and the host's rules for bridge %s are in place", egress.Table, n.Bridge)
		}
		if snap != nil {
			results = append(results, Refusals(*snap, k.Name))
		}
		results = append(results, c.egressProbe(ctx, d, k)...)
	}
	return results
}

// Refusals says what the agent reports of a class's allowlist.
func Refusals(s status.Snapshot, class string) Result {
	name := "egress " + class + " agent"
	for _, e := range s.Egress {
		if e.Class != class {
			continue
		}
		if e.Problem != "" {
			return Result{name, false, "the agent does not serve the class: " + e.Problem}
		}
		detail := fmt.Sprintf("%d entries; %s", len(e.Entries), e.Summary())
		if n := len(e.Recent); n > 0 {
			last := e.Recent[n-1]
			detail += fmt.Sprintf("; latest at %s: %s", last.Time.Local().Format("15:04:05"), last.String())
		}
		return Result{name, true, detail}
	}
	return Result{name, false, "the agent does not know the class; restart boxrunner"}
}

// EgressProbe is what a probe container tries from a class's network.
type EgressProbe struct {
	// Allowed is a host the class allows, and Denied one it does not, with
	// DeniedAddr an address the host of that name has.
	Allowed, Denied string
	DeniedAddr      netip.Addr
	// Gateway is the host's address on the class's bridge. Resolver is the
	// box's own upstream resolver.
	Gateway, Resolver netip.Addr
}

// ExampleHosts are names reserved for examples, which a class's allowlist
// has no reason to hold and which resolve on the internet.
var ExampleHosts = []string{"example.com", "example.net", "example.org"}

// Script is the shell the probe container runs: one "name exitcode" line per try.
func (p EgressProbe) Script() string {
	var b strings.Builder
	b.WriteString(`probe() { timeout 3 bash -c "exec 3<>/dev/tcp/$2/$3" 2>/dev/null; echo "$1 $?"; }
look() { timeout 6 getent hosts "$2" >/dev/null 2>&1; echo "$1 $?"; }
ask() { timeout 3 bash -c "exec 3<>/dev/udp/$2/53; printf '\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x06github\x03com\x00\x00\x01\x00\x01' >&3; head -c1 <&3 >/dev/null" 2>/dev/null; echo "$1 $?"; }
`)
	fmt.Fprintf(&b, "look lookup-allowed %s\nprobe allowed %s 443\n", p.Allowed, p.Allowed)
	if p.Denied != "" {
		fmt.Fprintf(&b, "look lookup-denied %s\nprobe denied %s 443\n", p.Denied, p.Denied)
	}
	if p.DeniedAddr.IsValid() {
		fmt.Fprintf(&b, "probe address %s 443\n", p.DeniedAddr)
	}
	fmt.Fprintf(&b, "probe host %s 22\nprobe metadata 169.254.169.254 80\n", p.Gateway)
	if p.Resolver.IsValid() {
		fmt.Fprintf(&b, "probe resolver-tcp %s 53\nask resolver-udp %s\n", p.Resolver, p.Resolver)
	}
	return b.String()
}

// JudgeEgress judges what the probe container reported, as "name exitcode"
// lines. What the class allows must answer, and everything else must not.
func JudgeEgress(class string, p EgressProbe, logs string) []Result {
	codes := ParseProbes(logs)
	var results []Result
	add := func(what string, ok bool, format string, args ...any) {
		results = append(results, Result{"egress " + class + " " + what, ok, fmt.Sprintf(format, args...)})
	}
	// refused is for a name: a lookup that fails or a connection that never
	// starts both count. dropped is for an address, where the host's rules must
	// drop the packet, so the try times out (124); a refusal or an unreachable
	// answer means something past the rules saw it.
	refused := func(name string) (bool, string) {
		code, ok := codes[name]
		switch {
		case !ok:
			return false, "the probe gave no answer"
		case code == 0:
			return false, "a job container got through"
		}
		return true, ""
	}
	dropped := func(name string) (bool, string) {
		code, ok := codes[name]
		switch {
		case !ok:
			return false, "the probe gave no answer"
		case code == 0:
			return false, "a job container got through"
		case code != 124:
			return false, fmt.Sprintf("a job container got an answer (exit %d); the rules should drop the packet", code)
		}
		return true, ""
	}
	look, tcp := codes["lookup-allowed"], codes["allowed"]
	_, lookSeen := codes["lookup-allowed"]
	_, tcpSeen := codes["allowed"]
	add("allows", lookSeen && tcpSeen && look == 0 && tcp == 0, "%s resolves and answers on port 443 from a job container (lookup exit %d, connect exit %d)", p.Allowed, look, tcp)

	switch {
	case p.Denied == "":
		add("refuses a name", true, "skipped: the class allows example.com, example.net and example.org, so there is no name to try")
	default:
		okLookup, why := refused("lookup-denied")
		okConnect, why2 := refused("denied")
		if okLookup && okConnect {
			add("refuses a name", true, "%s does not resolve for a job, and cannot be reached", p.Denied)
		} else {
			add("refuses a name", false, "%s: %s", p.Denied, strings.TrimSpace(why+" "+why2))
		}
	}
	if !p.DeniedAddr.IsValid() {
		add("refuses an address", true, "skipped: the box could not look up %s to try its address", p.Denied)
	} else if ok, why := dropped("address"); ok {
		add("refuses an address", true, "%s, the address of %s, is unreachable by number", p.DeniedAddr, p.Denied)
	} else {
		add("refuses an address", false, "%s, the address of %s: %s", p.DeniedAddr, p.Denied, why)
	}
	okHost, whyHost := dropped("host")
	okMeta, whyMeta := dropped("metadata")
	if okHost && okMeta {
		add("host", true, "the host at %s and the metadata service are unreachable from jobs", p.Gateway)
	} else {
		add("host", false, "%s", strings.TrimSpace(whyHost+" "+whyMeta))
	}
	if !p.Resolver.IsValid() {
		add("resolver", true, "skipped: the box's resolvers are all on this machine")
	} else {
		okTCP, whyTCP := dropped("resolver-tcp")
		okUDP, whyUDP := dropped("resolver-udp")
		if okTCP && okUDP {
			add("resolver", true, "a job cannot ask %s, the box's resolver, or any other DNS server", p.Resolver)
		} else {
			add("resolver", false, "%s: %s", p.Resolver, strings.TrimSpace(whyTCP+" "+whyUDP))
		}
	}
	return results
}

// egressProbe runs a container like a job of the class on its network.
func (c Checks) egressProbe(ctx context.Context, d *docker.Client, class config.Class) []Result {
	n := egress.NetOf(class.Name)
	name := "egress " + class.Name + " probe"
	network, ok, err := d.Network(ctx, n.Name)
	if err != nil || !ok {
		return []Result{{name, false, fmt.Sprintf("the Docker network %s is missing; the agent creates it when it starts", n.Name)}}
	}
	gateway, _ := network.Gateway()
	p := EgressProbe{Allowed: "github.com", Gateway: gateway, Resolver: upstreamResolver()}
	for _, host := range ExampleHosts {
		if !class.Egress.AllowsName(host, 443) {
			p.Denied = host
			break
		}
	}
	if p.Denied != "" {
		if addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", p.Denied); err == nil && len(addrs) > 0 {
			p.DeniedAddr = addrs[0]
		}
	}
	container := "boxrunner-doctor-egress-" + class.Name
	d.Remove(ctx, container)
	spec := docker.ContainerSpec{
		Name: container, Image: layout.Image, Cmd: []string{"bash", "-c", p.Script()}, Tty: true,
		CgroupParent: layout.IdleSlice, Network: n.Name, DNS: []string{gateway.String()}, NanoCPUs: 1e9, Memory: 256 << 20, PidsLimit: 64,
	}
	if class.Isolation == config.Sysbox {
		spec.Runtime = layout.SysboxRuntime
	}
	id, err := d.Create(ctx, spec)
	if docker.NotFound(err) {
		return []Result{{name, false, fmt.Sprintf("the job image %s is missing; run install", layout.Image)}}
	}
	if err != nil {
		return []Result{{name, false, err.Error()}}
	}
	defer d.Remove(context.WithoutCancel(ctx), id)
	if err := d.Start(ctx, id); err != nil {
		return []Result{{name, false, err.Error()}}
	}
	waited, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := d.Wait(waited, id); err != nil {
		return []Result{{name, false, err.Error()}}
	}
	logs, _ := d.Logs(ctx, id)
	return JudgeEgress(class.Name, p, logs)
}

// upstreamResolver is the box's own resolver that is not on this machine, or
// the zero address when there is none.
func upstreamResolver() netip.Addr {
	for _, path := range []string{dnsfwd.ResolvConfPath, "/run/systemd/resolve/resolv.conf"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, s := range dnsfwd.ParseResolvConf(string(b)) {
			if a := s.Addr(); a.Is4() && !a.IsLoopback() && !a.IsUnspecified() {
				return a
			}
		}
	}
	return netip.Addr{}
}
