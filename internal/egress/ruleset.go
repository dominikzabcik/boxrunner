package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Table is the nftables table, in the inet family, that holds every class's
// rules. Nothing else of boxrunner's is in it, and nothing of anyone else's.
const Table = "boxrunner"

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
	return Net{Class: class, Name: "boxrunner-" + class, Bridge: "boxrunner-" + hex.EncodeToString(sum[:3])[:5]}
}

// ID names the class's objects in the table.
func (n Net) ID() string { return "c" + strings.TrimPrefix(n.Bridge, "boxrunner-") }

// Class is what the ruleset needs of one class.
type Class struct {
	Net    Net
	Policy Policy
	// Closed refuses every new connection. It holds the place of a class that
	// left the config while its jobs still run.
	Closed bool
}

// Ruleset is everything the table holds.
type Ruleset struct {
	Classes []Class
	// Deny are the networks of the box, which no class reaches from its own,
	// whatever its entries say. The Never ranges are added to them.
	Deny []netip.Prefix
	// Group is the NFLOG group that refused connections are logged to.
	Group int
}

const (
	// MaxLive is how many addresses a class may have open at once.
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
	want := map[string]bool{}

	line("add table inet %s", Table)
	line("add chain inet %s forward { type filter hook forward priority -10; policy accept; }", Table)
	line("flush chain inet %s forward", Table)
	stale := listed(existing)
	for _, c := range r.Classes {
		id := c.Net.ID()
		for _, name := range []string{"egress_" + id, "blocked_" + id, id + "_dyn4", id + "_dyn6", id + "_any4", id + "_any6", id + "_blocked"} {
			want[name] = true
		}
		line("add set inet %s %s_dyn4 { type ipv4_addr . inet_service; flags timeout; size %d; }", Table, id, setSize)
		line("add set inet %s %s_dyn6 { type ipv6_addr . inet_service; flags timeout; size %d; }", Table, id, setSize)
		line("add set inet %s %s_any4 { type ipv4_addr; flags timeout; size %d; }", Table, id, setSize)
		line("add set inet %s %s_any6 { type ipv6_addr; flags timeout; size %d; }", Table, id, setSize)
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
		rule("ct state established,related accept")
		if !c.Closed {
			v4, v6 := r.denied()
			rule("ip daddr { %s } goto blocked_%s", joinPrefixes(v4), id)
			rule("ip6 daddr { %s } goto blocked_%s", joinPrefixes(v6), id)
			rule("ip daddr . tcp dport @%s_dyn4 accept", id)
			rule("ip6 daddr . tcp dport @%s_dyn6 accept", id)
			rule("meta l4proto tcp ip daddr @%s_any4 accept", id)
			rule("meta l4proto tcp ip6 daddr @%s_any6 accept", id)
			for _, n := range c.Policy.Nets {
				rule("%s accept", netMatch(n))
			}
		}
		rule("goto blocked_%s", id)
		line("add rule inet %s forward iifname \"%s\" jump egress_%s", Table, c.Net.Bridge, id)
	}
	for _, name := range stale.chains {
		if !want[name] && name != "forward" {
			line("flush chain inet %s %s", Table, name)
		}
	}
	for _, name := range stale.chains {
		if !want[name] && name != "forward" {
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

// denied are the destinations a class never reaches, by family.
func (r Ruleset) denied() (v4, v6 []netip.Prefix) {
	all := slices.Concat(Never, r.Deny)
	slices.SortFunc(all, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	for _, p := range slices.Compact(all) {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return v4, v6
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
	family := "ip"
	if n.Prefix.Addr().Is6() {
		family = "ip6"
	}
	switch {
	case n.Ports.Any:
		return fmt.Sprintf("meta l4proto tcp %s daddr %s", family, n.Prefix)
	case len(n.Ports.List) == 1:
		return fmt.Sprintf("%s daddr %s tcp dport %d", family, n.Prefix, n.Ports.List[0])
	default:
		ports := make([]string, len(n.Ports.List))
		for i, p := range n.Ports.List {
			ports[i] = fmt.Sprint(p)
		}
		return fmt.Sprintf("%s daddr %s tcp dport { %s }", family, n.Prefix, strings.Join(ports, ", "))
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

// ElementScript opens grants in a class's sets. An address that is open
// already has its time renewed.
func ElementScript(n Net, grants []Grant) string {
	id := n.ID()
	var dyn4, dyn6, any4, any6 []string
	for _, g := range grants {
		timeout := fmt.Sprintf("timeout %ds", int(g.TTL.Seconds()))
		switch {
		case g.Ports.Any && g.Addr.Is4():
			any4 = append(any4, fmt.Sprintf("%s %s", g.Addr, timeout))
		case g.Ports.Any:
			any6 = append(any6, fmt.Sprintf("%s %s", g.Addr, timeout))
		default:
			for _, port := range g.Ports.List {
				e := fmt.Sprintf("%s . %d %s", g.Addr, port, timeout)
				if g.Addr.Is4() {
					dyn4 = append(dyn4, e)
				} else {
					dyn6 = append(dyn6, e)
				}
			}
		}
	}
	var b strings.Builder
	for _, set := range []struct {
		name     string
		elements []string
	}{{id + "_dyn4", dyn4}, {id + "_dyn6", dyn6}, {id + "_any4", any4}, {id + "_any6", any6}} {
		if len(set.elements) > 0 {
			fmt.Fprintf(&b, "add element inet %s %s { %s }\n", Table, set.name, strings.Join(set.elements, ", "))
		}
	}
	return b.String()
}
