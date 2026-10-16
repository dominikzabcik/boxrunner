package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
)

// Table is the nftables table, in the inet family, that holds every class's
// rules. Nothing else of boxrunner's is in it, and nothing of anyone else's.
const Table = "boxrunner"

// Mark is the bit of a packet's mark that the table sets on the packets it
// lets through. An iptables rule drops what comes from an egress bridge
// without it, so a table that someone flushed leaves the bridges closed.
const Mark = firewall.EgressMark

// BridgePrefix starts the name of every egress class's bridge.
const BridgePrefix = firewall.BridgePrefix

// Net is a class's own Docker network. Its bridge is the identity of the
// class's jobs: a job can change its address, but it cannot send a packet
// that arrives on another bridge.
type Net struct {
	Class string
	// Name is the Docker network, Bridge its interface.
	Name   string
	Bridge string
	Subnet netip.Prefix
	// Gateway is the host's address on the bridge, where the class's DNS
	// forwarder listens.
	Gateway netip.Addr
}

// NetOf names the network of a class. The bridge's name is a short hash of
// the class's, since an interface name holds 15 characters.
func NetOf(class string) Net {
	sum := sha256.Sum256([]byte(class))
	return Net{Class: class, Name: "boxrunner-" + class, Bridge: BridgePrefix + hex.EncodeToString(sum[:3])[:5]}
}

// ID names the class's objects in the table.
func (n Net) ID() string { return "c" + strings.TrimPrefix(n.Bridge, BridgePrefix) }

// Class is what the ruleset needs of one class.
type Class struct {
	Net    Net
	Policy Policy
	// Closed refuses every new connection. It holds the place of a class that
	// left the config while its jobs still run.
	Closed bool
	// Open are the services on the host that the class's jobs may reach, apart
	// from DNS: the registry mirror and the Actions cache.
	Open []netip.AddrPort
}

// Sets are the names of the class's sets of open addresses. They carry a hash
// of the entries, so a class whose entries change gets empty sets, and what
// the old entries opened closes.
func (c Class) Sets() (dyn, anyPort string) {
	h := sha256.New()
	for _, r := range c.Policy.Names {
		fmt.Fprintf(h, "name %s %v %s\n", r.Name, r.Wildcard, r.Ports)
	}
	for _, r := range c.Policy.Nets {
		fmt.Fprintf(h, "net %s %s\n", r.Prefix, r.Ports)
	}
	suffix := hex.EncodeToString(h.Sum(nil))[:6]
	return c.Net.ID() + "_dyn_" + suffix, c.Net.ID() + "_any_" + suffix
}

// Ruleset is everything the table holds.
type Ruleset struct {
	Classes []Class
	// Deny are the networks of the box, which no class reaches from its own,
	// whatever its entries say. The Never ranges are added to them.
	Deny []netip.Prefix
	// Group is the NFLOG group that refused packets are logged to.
	Group int
}

const (
	// MaxLive is how many elements a class may have open at once: an address
	// counts once for each port it is open on, or once when open on every port.
	MaxLive = 8192
	// setSize is the nftables size of each set, which stays above MaxLive.
	setSize = 65536
)

// Script is the nft script that makes the table what r says. existing is
// what `nft list table` printed, "" when the table does not exist: it names
// the objects of earlier rulesets that this one removes. Sets and counters
// the new ruleset keeps are left as they are, so addresses opened before a
// restart stay open until they expire.
func (r Ruleset) Script(existing string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	want := map[string]bool{"forward": true, "input": true}

	line("add table inet %s", Table)
	line("add chain inet %s forward { type filter hook forward priority -10; policy accept; }", Table)
	line("flush chain inet %s forward", Table)
	line("add chain inet %s input { type filter hook input priority -10; policy accept; }", Table)
	line("flush chain inet %s input", Table)
	v4 := r.denied()
	for _, c := range r.Classes {
		id, bridge := c.Net.ID(), c.Net.Bridge
		dyn, anyPort := c.Sets()
		for _, name := range []string{"egress_" + id, "host_" + id, "blocked_" + id, dyn, anyPort, id + "_blocked"} {
			want[name] = true
		}
		line("add set inet %s %s { type ipv4_addr . inet_service; flags timeout; size %d; }", Table, dyn, setSize)
		line("add set inet %s %s { type ipv4_addr; flags timeout; size %d; }", Table, anyPort, setSize)
		line("add counter inet %s %s_blocked", Table, id)
		line("add chain inet %s blocked_%s", Table, id)
		line("flush chain inet %s blocked_%s", Table, id)
		line("add rule inet %s blocked_%s limit rate 20/second burst 40 packets log group %d prefix \"%s\" snaplen 96", Table, id, r.Group, id)
		line("add rule inet %s blocked_%s counter name \"%s_blocked\" drop", Table, id, id)

		line("add chain inet %s egress_%s", Table, id)
		line("flush chain inet %s egress_%s", Table, id)
		rule := func(format string, args ...any) {
			line("add rule inet %s egress_%s %s", Table, id, fmt.Sprintf(format, args...))
		}
		pass := fmt.Sprintf("meta mark set meta mark or 0x%x accept", Mark)
		// Whatever a job forges, a packet from this bridge must come from its
		// subnet: a source of another bridge would match that bridge's
		// connections below. A job's network has no IPv6.
		rule("ip saddr != %s goto blocked_%s", c.Net.Subnet, id)
		rule("meta nfproto ipv6 goto blocked_%s", id)
		rule("ip daddr { %s } goto blocked_%s", joinPrefixes(v4), id)
		rule("ct state established %s", pass)
		if !c.Closed {
			rule("ip daddr . tcp dport @%s %s", dyn, pass)
			rule("meta l4proto tcp ip daddr @%s %s", anyPort, pass)
			for _, n := range c.Policy.Nets {
				rule("%s %s", netMatch(n), pass)
			}
		}
		rule("goto blocked_%s", id)

		// What the bridge may reach on the host: its DNS forwarder and the
		// services it uses. Nothing else of the host's answers a job.
		line("add chain inet %s host_%s", Table, id)
		line("flush chain inet %s host_%s", Table, id)
		host := func(format string, args ...any) {
			line("add rule inet %s host_%s %s", Table, id, fmt.Sprintf(format, args...))
		}
		host("meta nfproto ipv6 goto blocked_%s", id)
		if !c.Closed {
			host("ip daddr %s udp dport 53 accept", c.Net.Gateway)
			host("ip daddr %s tcp dport 53 accept", c.Net.Gateway)
			for _, o := range c.Open {
				host("ip daddr %s tcp dport %d accept", o.Addr(), o.Port())
			}
		}
		host("goto blocked_%s", id)
		line("add rule inet %s forward iifname \"%s\" jump egress_%s", Table, bridge, id)
		line("add rule inet %s input iifname \"%s\" jump host_%s", Table, bridge, id)
		line("add rule inet %s input ip daddr %s iifname != { \"%s\", \"lo\" } drop", Table, c.Net.Gateway, bridge)
	}
	stale := listed(existing)
	for _, name := range stale.chains {
		if !want[name] {
			line("flush chain inet %s %s", Table, name)
		}
	}
	for _, name := range stale.chains {
		if !want[name] {
			line("delete chain inet %s %s", Table, name)
		}
	}
	for _, name := range stale.sets {
		if !want[name] {
			line("delete set inet %s %s", Table, name)
		}
	}
	for _, name := range stale.counters {
		if !want[name] {
			line("delete counter inet %s %s", Table, name)
		}
	}
	return b.String()
}

// Replace is a script that deletes the table and builds it again in one
// transaction, so no packet meets an empty table in between.
func (r Ruleset) Replace() string {
	return fmt.Sprintf("add table inet %s\ndelete table inet %s\n", Table, Table) + r.Script("")
}

// Intact reports whether a listing of the table still has every chain, set
// and hook this ruleset puts in it, as a flush of the rules would not leave.
func (r Ruleset) Intact(listing string) bool {
	have := listed(listing)
	for _, name := range []string{"forward", "input"} {
		if !slices.Contains(have.chains, name) {
			return false
		}
	}
	for _, c := range r.Classes {
		id := c.Net.ID()
		dyn, anyPort := c.Sets()
		for _, name := range []string{"egress_" + id, "host_" + id, "blocked_" + id} {
			if !slices.Contains(have.chains, name) {
				return false
			}
		}
		if !slices.Contains(have.sets, dyn) || !slices.Contains(have.sets, anyPort) || !slices.Contains(have.counters, id+"_blocked") {
			return false
		}
		for _, hook := range []string{`iifname "%s" jump egress_%s`, `iifname "%s" jump host_%s`} {
			if !strings.Contains(listing, fmt.Sprintf(hook, c.Net.Bridge, id)) {
				return false
			}
		}
	}
	return true
}

// denied are the destinations a class never reaches.
func (r Ruleset) denied() []netip.Prefix {
	all := slices.Concat(Never, r.Deny)
	slices.SortFunc(all, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(all)
}

func joinPrefixes(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}

// netMatch is the match of a rule that allows an entry's network.
func netMatch(n NetRule) string {
	switch {
	case n.Ports.Any:
		return fmt.Sprintf("meta l4proto tcp ip daddr %s", n.Prefix)
	case len(n.Ports.List) == 1:
		return fmt.Sprintf("ip daddr %s tcp dport %d", n.Prefix, n.Ports.List[0])
	default:
		ports := make([]string, len(n.Ports.List))
		for i, p := range n.Ports.List {
			ports[i] = fmt.Sprint(p)
		}
		return fmt.Sprintf("ip daddr %s tcp dport { %s }", n.Prefix, strings.Join(ports, ", "))
	}
}

// objects are the names of a table's chains, sets and counters.
type objects struct{ chains, sets, counters []string }

var (
	chainLine   = regexp.MustCompile(`(?m)^\s*chain (\S+) \{`)
	setLine     = regexp.MustCompile(`(?m)^\s*set (\S+) \{`)
	counterLine = regexp.MustCompile(`(?m)^\s*counter (\S+) \{`)
)

// listed reads the objects of an `nft list table` listing.
func listed(listing string) objects {
	names := func(re *regexp.Regexp) []string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(listing, -1) {
			out = append(out, m[1])
		}
		return out
	}
	return objects{names(chainLine), names(setLine), names(counterLine)}
}

// ElementScript sets the elements open in a class's sets, each for its time.
// For every set it adds the elements, deletes them and adds them again in one
// transaction. The first add makes the delete find something, which nftables
// refuses to delete otherwise, and the delete makes the second add give an
// element that was open already its new time: the nftables wiki says the time
// of an element cannot be changed, and some kernels renew it on an add and
// others leave it.
func ElementScript(c Class, elements []Element) string {
	dyn, anyPort := c.Sets()
	var perPort, perAddr, perPortKeys, perAddrKeys []string
	for _, e := range elements {
		timeout := fmt.Sprintf(" timeout %ds", int(e.TTL.Seconds()))
		if e.Port == 0 {
			perAddr = append(perAddr, e.Addr.String()+timeout)
			perAddrKeys = append(perAddrKeys, e.Addr.String())
			continue
		}
		perPort = append(perPort, fmt.Sprintf("%s . %d%s", e.Addr, e.Port, timeout))
		perPortKeys = append(perPortKeys, fmt.Sprintf("%s . %d", e.Addr, e.Port))
	}
	var b strings.Builder
	for _, set := range []struct {
		name          string
		with, without []string
	}{{dyn, perPort, perPortKeys}, {anyPort, perAddr, perAddrKeys}} {
		if len(set.with) == 0 {
			continue
		}
		add := fmt.Sprintf("add element inet %s %s { %s }\n", Table, set.name, strings.Join(set.with, ", "))
		fmt.Fprintf(&b, "%sdelete element inet %s %s { %s }\n%s", add, Table, set.name, strings.Join(set.without, ", "), add)
	}
	return b.String()
}

// HostNet is what the host's rules let a class's jobs reach on the host: its
// DNS forwarder at the bridge's gateway, and the registry mirror and Actions
// cache that the box serves at mirrorGateway when the class uses them.
func HostNet(n Net, docker, cache bool, services firewall.Services, mirrorGateway netip.Addr) firewall.EgressNet {
	h := firewall.EgressNet{Bridge: n.Bridge, Gateway: n.Gateway}
	if services.Mirror && docker && mirrorGateway.IsValid() {
		h.Open = append(h.Open, netip.AddrPortFrom(mirrorGateway, layout.MirrorPort))
	}
	if services.Cache && cache && mirrorGateway.IsValid() {
		h.Open = append(h.Open, netip.AddrPortFrom(mirrorGateway, layout.CachePort))
	}
	return h
}
