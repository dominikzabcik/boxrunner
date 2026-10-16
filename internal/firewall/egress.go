package firewall

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// The bridges of egress classes are all named "boxrunner-" and five more
// characters, which no other bridge of boxrunner's is. Their forwarded traffic
// is the nftables table's. It marks the packets it lets through with
// EgressMark, and these chains only back it up: EgressChain keeps the jobs off
// the host as the table does, and EgressForwardChain drops what leaves a
// bridge without the mark, which is all of it when the table is gone.
const (
	EgressChain        = "BOXRUNNER-EGRESS"
	EgressForwardChain = "BOXRUNNER-EGRESS-FWD"
	BridgePrefix       = "boxrunner-"
	EgressMark         = 0x10000000
)

// EgressNet is the bridge of an egress class and the host services its jobs
// may reach.
type EgressNet struct {
	Bridge string
	// Gateway is the host's address on the bridge: the class's DNS forwarder
	// listens there, and no other bridge may reach it.
	Gateway netip.Addr
	// Open are the services of the job network that the class's jobs use,
	// such as the registry mirror.
	Open []netip.AddrPort
}

// EgressRules is the content of the egress chain for a family. The host
// drops what the bridges send it, except the DNS forwarder and the services
// the class uses, as it does for the job bridge. Traffic from elsewhere
// returns to the host's own rules.
func (f Family) EgressRules(nets []EgressNet) [][]string {
	var rules, accept [][]string
	for _, n := range nets {
		if f.Is(n.Gateway) {
			gw := netip.PrefixFrom(n.Gateway, n.Gateway.BitLen()).String()
			for _, proto := range []string{"udp", "tcp"} {
				rules = append(rules, []string{"-d", gw, "!", "-i", n.Bridge, "-p", proto, "--dport", "53", "-j", "DROP"})
				accept = append(accept, []string{"-d", gw, "-i", n.Bridge, "-p", proto, "--dport", "53", "-j", "ACCEPT"})
			}
		}
		for _, o := range n.Open {
			if !o.IsValid() || !f.Is(o.Addr()) {
				continue
			}
			host := netip.PrefixFrom(o.Addr(), o.Addr().BitLen()).String()
			accept = append(accept, []string{"-d", host, "-i", n.Bridge, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(int(o.Port())), "-j", "ACCEPT"})
		}
	}
	rules = append(rules, accept...)
	for _, n := range nets {
		rules = append(rules, []string{"-i", n.Bridge, "-j", "DROP"})
	}
	return rules
}

// egressJump sends the host's input into the egress chain, ahead of the job
// bridge's chain, whose drops of the open services are meant for other bridges.
func egressJump() Jump {
	return Jump{"INPUT", []string{"-m", "comment", "--comment", tag, "-j", EgressChain}}
}

// egressForwardJump sends what the egress bridges forward into the chain that
// drops what the table did not mark.
func egressForwardJump(hook string) Jump {
	return Jump{hook, []string{"-i", BridgePrefix + "+", "-m", "comment", "--comment", tag, "-j", EgressForwardChain}}
}

// markedRule is the one rule of the forward chain.
var markedRule = []string{"-m", "mark", "!", "--mark", fmt.Sprintf("0x%x/0x%x", EgressMark, EgressMark), "-j", "DROP"}

// egressRestore is the iptables-restore input that replaces the egress chain.
func (f Family) egressRestore(nets []EgressNet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*filter\n:%s - [0:0]\n:%s - [0:0]\n", EgressChain, EgressForwardChain)
	for _, r := range f.EgressRules(nets) {
		fmt.Fprintf(&b, "-A %s %s\n", EgressChain, strings.Join(r, " "))
	}
	fmt.Fprintf(&b, "-A %s %s\nCOMMIT\n", EgressForwardChain, strings.Join(markedRule, " "))
	return b.String()
}

// ApplyEgress installs the egress chain for the bridges, or removes it when
// there are none. The chain is complete before its jump comes first in INPUT,
// and the jump is added before an old copy is deleted, so no packet passes
// without it.
func ApplyEgress(ctx context.Context, run Run, nets []EgressNet) error {
	for _, f := range Families {
		if len(nets) == 0 {
			if err := removeEgress(ctx, run, f); err != nil {
				return err
			}
			continue
		}
		if out, err := run(ctx, f.egressRestore(nets), f.Restore, "-w", "--noflush"); err != nil {
			return fmt.Errorf("%s: %w: %s", f.Restore, err, out)
		}
		if err := f.jumpFirst(ctx, run, egressJump()); err != nil {
			return err
		}
		if err := f.jumpFirst(ctx, run, egressForwardJump(forwardHook(ctx, run, f))); err != nil {
			return err
		}
	}
	return nil
}

// jumpFirst leaves exactly one copy of the jump, ahead of every other rule of
// its chain.
func (f Family) jumpFirst(ctx context.Context, run Run, j Jump) error {
	rule := j.Rule
	positions := f.positions(ctx, run, j)
	if len(positions) == 1 && positions[0] == 1 {
		return nil
	}
	if out, err := run(ctx, "", f.Command, append([]string{"-w", "-I", j.Chain, "1"}, rule...)...); err != nil {
		return fmt.Errorf("%s -I %s: %w: %s", f.Command, j.Chain, err, out)
	}
	// The rule inserted at 1 is the first of the copies; the others have moved down.
	copies := f.positions(ctx, run, j)
	if len(copies) < 2 {
		return nil
	}
	for _, n := range slices.Backward(copies[1:]) {
		if out, err := run(ctx, "", f.Command, "-w", "-D", j.Chain, strconv.Itoa(n)); err != nil {
			return fmt.Errorf("%s -D %s %d: %w: %s", f.Command, j.Chain, n, err, out)
		}
	}
	return nil
}

// positions are the rule numbers in a jump's chain that hold the jump.
func (f Family) positions(ctx context.Context, run Run, j Jump) []int {
	out, err := run(ctx, "", f.Command, "-w", "-S", j.Chain)
	if err != nil {
		return nil
	}
	want := "-A " + j.Chain + " " + strings.Join(j.Rule, " ")
	var at []int
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		n++
		if strings.TrimSpace(line) == want {
			at = append(at, n)
		}
	}
	return at
}

func removeEgress(ctx context.Context, run Run, f Family) error {
	deleteAll(ctx, run, f, egressJump())
	for _, hook := range []string{"DOCKER-USER", "FORWARD"} {
		deleteAll(ctx, run, f, egressForwardJump(hook))
	}
	for _, chain := range []string{EgressChain, EgressForwardChain} {
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
	return nil
}

// CheckEgress reports whether every family has exactly the egress chain for
// the bridges, with its one jump ahead of the job bridge's. Absent means no
// chain at all, which is right when there are no bridges.
func CheckEgress(ctx context.Context, run Run, nets []EgressNet) State {
	want := len(nets) > 0
	present, wrong := false, false
	input := Jumps("", "FORWARD")[0]
	for _, f := range Families {
		out, err := run(ctx, "", f.Command, "-w", "-S", EgressChain)
		if err != nil {
			wrong = wrong || want
			continue
		}
		present = true
		rules := f.EgressRules(nets)
		if !want || strings.Count(strings.TrimSpace(out), "\n")+1 != len(rules)+1 {
			wrong = true
		}
		for _, r := range rules {
			if _, err := run(ctx, "", f.Command, append([]string{"-w", "-C", EgressChain}, r...)...); err != nil {
				wrong = true
			}
		}
		own, main := f.positions(ctx, run, egressJump()), f.positions(ctx, run, input)
		if len(own) != 1 || (len(main) > 0 && own[0] > main[0]) {
			wrong = true
		}
		if _, err := run(ctx, "", f.Command, append([]string{"-w", "-C", EgressForwardChain}, markedRule...)...); err != nil {
			wrong = true
		}
		if at := f.positions(ctx, run, egressForwardJump(forwardHook(ctx, run, f))); len(at) != 1 {
			wrong = true
		}
	}
	switch {
	case !present:
		return Absent
	case wrong:
		return Stale
	default:
		return Current
	}
}
