// Package firewall keeps job containers away from the host and private
// networks, except for the services on the host that serve jobs alone: the
// registry mirror and the Actions cache. The rules live in two chains of their own, filled atomically with
// iptables-restore and reached by jumps tagged with a comment, so applying them
// again replaces them and uninstall removes exactly them.
package firewall

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/dominikzabcik/boxrunner/internal/layout"
)

const (
	InputChain   = "BOXRUNNER-INPUT"
	ForwardChain = "BOXRUNNER-FORWARD"
	tag          = "boxrunner"
)

// Rules are what boxrunner's chains hold for one bridge.
type Rules struct {
	Bridge string
	// Open are the services that serve jobs alone, such as the registry
	// mirror: the host addresses and ports jobs may reach, and that nothing
	// else may reach.
	Open []netip.AddrPort
}

// Services are the host's services that a box's jobs may reach, each at its
// port on the bridge's gateway.
type Services struct {
	Mirror bool
	Cache  bool
}

// Rules are boxrunner's rules with the services open at gateway.
func (s Services) Rules(gateway netip.Addr) Rules {
	r := Rules{Bridge: layout.Bridge}
	if s.Mirror {
		r.Open = append(r.Open, netip.AddrPortFrom(gateway, layout.MirrorPort))
	}
	if s.Cache {
		r.Open = append(r.Open, netip.AddrPortFrom(gateway, layout.CachePort))
	}
	return r
}

// String names the open services, such as "the registry mirror and the Actions cache".
func (s Services) String() string {
	var names []string
	if s.Mirror {
		names = append(names, "the registry mirror")
	}
	if s.Cache {
		names = append(names, "the Actions cache")
	}
	return strings.Join(names, " and ")
}

type Family struct {
	Name    string
	Command string
	Restore string
	// Blocked are the destinations forwarded traffic from the bridge may not reach.
	Blocked []string
	// Is reports whether an address belongs to the family.
	Is func(netip.Addr) bool
}

var (
	V4 = Family{"IPv4", "iptables", "iptables-restore", []string{
		"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
	}, netip.Addr.Is4}
	V6 = Family{"IPv6", "ip6tables", "ip6tables-restore", []string{"fc00::/7", "fe80::/10"}, netip.Addr.Is6}

	Families = []Family{V4, V6}
)

// dns lets name lookups through. Docker's resolver forwards them to the
// host's upstream servers, which are often on a private address.
var dns = [][]string{
	{"-p", "udp", "--dport", "53", "-j", "RETURN"},
	{"-p", "tcp", "--dport", "53", "-j", "RETURN"},
}

type Chain struct {
	Name  string
	Rules [][]string
}

// Chains is the content of boxrunner's own chains for a family. Everything
// the host receives passes the input chain, so that the open services answer
// only the bridge; traffic from elsewhere returns to the host's own rules.
func (f Family) Chains(r Rules) []Chain {
	var input, open [][]string
	for _, addr := range r.Open {
		if !addr.IsValid() || !f.Is(addr.Addr()) {
			continue
		}
		host := netip.PrefixFrom(addr.Addr(), addr.Addr().BitLen()).String()
		port := strconv.Itoa(int(addr.Port()))
		input = append(input, []string{"-d", host, "!", "-i", r.Bridge, "-p", "tcp", "-m", "tcp", "--dport", port, "-j", "DROP"})
		open = append(open, []string{"-d", host, "-p", "tcp", "-m", "tcp", "--dport", port, "-j", "ACCEPT"})
	}
	input = append(input, []string{"!", "-i", r.Bridge, "-j", "RETURN"})
	input = append(append(append(input, dns...), open...), []string{"-j", "DROP"})
	forward := append([][]string{}, dns...)
	for _, cidr := range f.Blocked {
		forward = append(forward, []string{"-d", cidr, "-j", "DROP"})
	}
	return []Chain{{InputChain, input}, {ForwardChain, forward}}
}

// RestoreInput is the iptables-restore --noflush input that creates or
// replaces the chains in one commit.
func (f Family) RestoreInput(r Rules) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	for _, c := range f.Chains(r) {
		fmt.Fprintf(&b, ":%s - [0:0]\n", c.Name)
	}
	for _, c := range f.Chains(r) {
		for _, r := range c.Rules {
			fmt.Fprintf(&b, "-A %s %s\n", c.Name, strings.Join(r, " "))
		}
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

type Jump struct {
	Chain string
	Rule  []string
}

// Jumps send traffic into boxrunner's chains: everything the host receives,
// and what it forwards from the bridge. Forwarded traffic is caught in
// DOCKER-USER, which Docker evaluates before its own accept rules, or in
// FORWARD when Docker has no DOCKER-USER for the family.
func Jumps(bridge, forwardHook string) []Jump {
	tagged := []string{"-m", "comment", "--comment", tag, "-j"}
	return []Jump{
		{"INPUT", slices.Concat(tagged, []string{InputChain})},
		{forwardHook, slices.Concat([]string{"-i", bridge}, tagged, []string{ForwardChain})},
	}
}

// legacyInput is the input jump of boxrunner before the registry mirror,
// which sent only the bridge's traffic. Apply and Remove delete it.
func legacyInput(bridge string) Jump {
	return Jump{"INPUT", []string{"-i", bridge, "-m", "comment", "--comment", tag, "-j", InputChain}}
}

// Run runs a command with stdin and returns its combined output.
type Run func(ctx context.Context, stdin, name string, args ...string) (string, error)

func forwardHook(ctx context.Context, run Run, f Family) string {
	if _, err := run(ctx, "", f.Command, "-w", "-S", "DOCKER-USER"); err == nil {
		return "DOCKER-USER"
	}
	return "FORWARD"
}

// Apply installs or replaces the rules for every family.
func Apply(ctx context.Context, run Run, r Rules) error {
	for _, f := range Families {
		if out, err := run(ctx, f.RestoreInput(r), f.Restore, "-w", "--noflush"); err != nil {
			return fmt.Errorf("%s: %w: %s", f.Restore, err, out)
		}
		for _, j := range Jumps(r.Bridge, forwardHook(ctx, run, f)) {
			if _, err := run(ctx, "", f.Command, append([]string{"-w", "-C", j.Chain}, j.Rule...)...); err == nil {
				continue
			}
			if out, err := run(ctx, "", f.Command, append([]string{"-w", "-I", j.Chain, "1"}, j.Rule...)...); err != nil {
				return fmt.Errorf("%s -I %s: %w: %s", f.Command, j.Chain, err, out)
			}
		}
		deleteAll(ctx, run, f, legacyInput(r.Bridge))
	}
	return nil
}

// deleteAll deletes every copy of a jump.
func deleteAll(ctx context.Context, run Run, f Family, j Jump) {
	for range 10 {
		if _, err := run(ctx, "", f.Command, append([]string{"-w", "-D", j.Chain}, j.Rule...)...); err != nil {
			return
		}
	}
}

type State int

const (
	Absent State = iota
	Current
	Stale
)

// Check reports whether every family has exactly boxrunner's rules, none of
// them, or something in between.
func Check(ctx context.Context, run Run, r Rules) State {
	found, missing := false, false
	for _, f := range Families {
		for _, c := range f.Chains(r) {
			out, err := run(ctx, "", f.Command, "-w", "-S", c.Name)
			if err != nil {
				missing = true
				continue
			}
			found = true
			if strings.Count(strings.TrimSpace(out), "\n")+1 != len(c.Rules)+1 {
				missing = true
			}
			for _, r := range c.Rules {
				if _, err := run(ctx, "", f.Command, append([]string{"-w", "-C", c.Name}, r...)...); err != nil {
					missing = true
				}
			}
		}
		for _, j := range Jumps(r.Bridge, forwardHook(ctx, run, f)) {
			if _, err := run(ctx, "", f.Command, append([]string{"-w", "-C", j.Chain}, j.Rule...)...); err != nil {
				missing = true
			} else {
				found = true
			}
		}
	}
	switch {
	case !found:
		return Absent
	case missing:
		return Stale
	default:
		return Current
	}
}

// Remove deletes the jumps and chains, whichever exist.
func Remove(ctx context.Context, run Run, bridge string) error {
	for _, f := range Families {
		for _, hook := range []string{"DOCKER-USER", "FORWARD"} {
			for _, j := range Jumps(bridge, hook) {
				deleteAll(ctx, run, f, j)
			}
		}
		deleteAll(ctx, run, f, legacyInput(bridge))
		if err := removeEgress(ctx, run, f); err != nil {
			return err
		}
		for _, chain := range []string{InputChain, ForwardChain} {
			if _, err := run(ctx, "", f.Command, "-w", "-S", chain); err != nil {
				continue
			}
			if out, err := run(ctx, "", f.Command, "-w", "-F", chain); err != nil {
				return fmt.Errorf("%s -F %s: %w: %s", f.Command, chain, err, out)
			}
			if out, err := run(ctx, "", f.Command, "-w", "-X", chain); err != nil {
				return fmt.Errorf("%s -X %s: %w: %s", f.Command, chain, err, out)
			}
		}
	}
	return nil
}
