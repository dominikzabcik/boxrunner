// Package firewall keeps job containers away from the host and private
// networks. The rules live in two chains of their own, filled atomically with
// iptables-restore and reached by jumps tagged with a comment, so applying them
// again replaces them and uninstall removes exactly them.
package firewall

import (
	"context"
	"fmt"
	"strings"
)

const (
	InputChain   = "BOXRUNNER-INPUT"
	ForwardChain = "BOXRUNNER-FORWARD"
	tag          = "boxrunner"
)

type Family struct {
	Name    string
	Command string
	Restore string
	// Blocked are the destinations forwarded traffic from the bridge may not reach.
	Blocked []string
}

var (
	V4 = Family{"IPv4", "iptables", "iptables-restore", []string{
		"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
	}}
	V6 = Family{"IPv6", "ip6tables", "ip6tables-restore", []string{"fc00::/7", "fe80::/10"}}

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

// Chains is the content of boxrunner's own chains for a family.
func (f Family) Chains() []Chain {
	input := append(append([][]string{}, dns...), []string{"-j", "DROP"})
	forward := append([][]string{}, dns...)
	for _, cidr := range f.Blocked {
		forward = append(forward, []string{"-d", cidr, "-j", "DROP"})
	}
	return []Chain{{InputChain, input}, {ForwardChain, forward}}
}

// RestoreInput is the iptables-restore --noflush input that creates or
// replaces the chains in one commit.
func (f Family) RestoreInput() string {
	var b strings.Builder
	b.WriteString("*filter\n")
	for _, c := range f.Chains() {
		fmt.Fprintf(&b, ":%s - [0:0]\n", c.Name)
	}
	for _, c := range f.Chains() {
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

// Jumps send traffic from the bridge into boxrunner's chains. Forwarded
// traffic is caught in DOCKER-USER, which Docker evaluates before its own
// accept rules, or in FORWARD when Docker has no DOCKER-USER for the family.
func Jumps(bridge, forwardHook string) []Jump {
	rule := func(target string) []string {
		return []string{"-i", bridge, "-m", "comment", "--comment", tag, "-j", target}
	}
	return []Jump{{"INPUT", rule(InputChain)}, {forwardHook, rule(ForwardChain)}}
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
func Apply(ctx context.Context, run Run, bridge string) error {
	for _, f := range Families {
		if out, err := run(ctx, f.RestoreInput(), f.Restore, "-w", "--noflush"); err != nil {
			return fmt.Errorf("%s: %w: %s", f.Restore, err, out)
		}
		for _, j := range Jumps(bridge, forwardHook(ctx, run, f)) {
			if _, err := run(ctx, "", f.Command, append([]string{"-w", "-C", j.Chain}, j.Rule...)...); err == nil {
				continue
			}
			if out, err := run(ctx, "", f.Command, append([]string{"-w", "-I", j.Chain, "1"}, j.Rule...)...); err != nil {
				return fmt.Errorf("%s -I %s: %w: %s", f.Command, j.Chain, err, out)
			}
		}
	}
	return nil
}

type State int

const (
	Absent State = iota
	Current
	Stale
)

// Check reports whether every family has exactly boxrunner's rules, none of
// them, or something in between.
func Check(ctx context.Context, run Run, bridge string) State {
	found, missing := false, false
	for _, f := range Families {
		for _, c := range f.Chains() {
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
		for _, j := range Jumps(bridge, forwardHook(ctx, run, f)) {
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
				for range 10 {
					if _, err := run(ctx, "", f.Command, append([]string{"-w", "-D", j.Chain}, j.Rule...)...); err != nil {
						break
					}
				}
			}
		}
		for _, c := range f.Chains() {
			if _, err := run(ctx, "", f.Command, "-w", "-S", c.Name); err != nil {
				continue
			}
			if out, err := run(ctx, "", f.Command, "-w", "-F", c.Name); err != nil {
				return fmt.Errorf("%s -F %s: %w: %s", f.Command, c.Name, err, out)
			}
			if out, err := run(ctx, "", f.Command, "-w", "-X", c.Name); err != nil {
				return fmt.Errorf("%s -X %s: %w: %s", f.Command, c.Name, err, out)
			}
		}
	}
	return nil
}
