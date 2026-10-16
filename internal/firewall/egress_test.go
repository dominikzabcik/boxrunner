package firewall

import (
	"context"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var (
	agentNet = EgressNet{
		Bridge:  "boxrunner-d4f0b",
		Gateway: netip.MustParseAddr("172.19.0.1"),
		Open:    []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5000"), netip.MustParseAddrPort("172.18.0.1:5001")},
	}
	coderNet = EgressNet{Bridge: "boxrunner-c84a9", Gateway: netip.MustParseAddr("172.20.0.1")}
)

func TestEgressRules(t *testing.T) {
	got := V4.EgressRules([]EgressNet{agentNet, coderNet})
	want := []string{
		"-d 172.19.0.1/32 ! -i boxrunner-d4f0b -p udp --dport 53 -j DROP",
		"-d 172.19.0.1/32 ! -i boxrunner-d4f0b -p tcp --dport 53 -j DROP",
		"-d 172.20.0.1/32 ! -i boxrunner-c84a9 -p udp --dport 53 -j DROP",
		"-d 172.20.0.1/32 ! -i boxrunner-c84a9 -p tcp --dport 53 -j DROP",
		"-d 172.19.0.1/32 -i boxrunner-d4f0b -p udp --dport 53 -j ACCEPT",
		"-d 172.19.0.1/32 -i boxrunner-d4f0b -p tcp --dport 53 -j ACCEPT",
		"-d 172.18.0.1/32 -i boxrunner-d4f0b -p tcp -m tcp --dport 5000 -j ACCEPT",
		"-d 172.18.0.1/32 -i boxrunner-d4f0b -p tcp -m tcp --dport 5001 -j ACCEPT",
		"-d 172.20.0.1/32 -i boxrunner-c84a9 -p udp --dport 53 -j ACCEPT",
		"-d 172.20.0.1/32 -i boxrunner-c84a9 -p tcp --dport 53 -j ACCEPT",
		"-i boxrunner-d4f0b -j DROP",
		"-i boxrunner-c84a9 -j DROP",
		"-i boxrunner-+ -j DROP",
	}
	var lines []string
	for _, r := range got {
		lines = append(lines, strings.Join(r, " "))
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("V4.EgressRules =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	var v6 []string
	for _, r := range V6.EgressRules([]EgressNet{agentNet}) {
		v6 = append(v6, strings.Join(r, " "))
	}
	if !reflect.DeepEqual(v6, []string{"-i boxrunner-d4f0b -j DROP", "-i boxrunner-+ -j DROP"}) {
		t.Errorf("V6.EgressRules = %q, want everything from the bridges dropped", v6)
	}
}

func TestApplyEgressKeepsOneJumpFirstAndRemoveTakesItAway(t *testing.T) {
	ctx := context.Background()
	tb := newTables()
	if err := Apply(ctx, tb.run, plain); err != nil {
		t.Fatal(err)
	}
	egress := "-A INPUT -m comment --comment boxrunner -j BOXRUNNER-EGRESS"
	backstop := "-A DOCKER-USER -i boxrunner-+ -m comment --comment boxrunner -j BOXRUNNER-EGRESS-FWD"
	input := "-A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT"
	nets := []EgressNet{agentNet}

	if got := CheckEgress(ctx, tb.run, nets); got != Absent {
		t.Errorf("CheckEgress before ApplyEgress = %v", got)
	}
	for range 2 {
		if err := ApplyEgress(ctx, tb.run, nets); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{"iptables", "ip6tables"} {
		if got := tb[cmd]["INPUT"]; !reflect.DeepEqual(got, []string{egress, input, "-A INPUT -i eth0 -j ACCEPT"}) {
			t.Errorf("%s INPUT = %q", cmd, got)
		}
	}
	if got := tb["iptables"]["DOCKER-USER"]; !reflect.DeepEqual(got, []string{backstop, "-A DOCKER-USER -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD"}) {
		t.Errorf("iptables DOCKER-USER = %q, want the backstop first", got)
	}
	if got := tb["ip6tables"]["FORWARD"]; !reflect.DeepEqual(got, []string{"-A FORWARD -i boxrunner-+ -m comment --comment boxrunner -j BOXRUNNER-EGRESS-FWD", "-A FORWARD -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-FORWARD"}) {
		t.Errorf("ip6tables FORWARD = %q", got)
	}
	if got := tb["iptables"][EgressForwardChain]; !reflect.DeepEqual(got, []string{"-A BOXRUNNER-EGRESS-FWD -m mark ! --mark 0x10000000/0x10000000 -j DROP"}) {
		t.Errorf("the forward chain = %q, want one rule that drops what the table did not mark", got)
	}
	if got := CheckEgress(ctx, tb.run, nets); got != Current {
		t.Errorf("CheckEgress after ApplyEgress = %v", got)
	}
	if got := CheckEgress(ctx, tb.run, []EgressNet{agentNet, coderNet}); got != Stale {
		t.Errorf("CheckEgress with another bridge = %v", got)
	}

	// The job bridge's jump lands ahead of the egress jump, as it would after
	// install re-applies its rules on a box where they were deleted: apply puts it back.
	tb["iptables"]["INPUT"] = []string{input, egress, "-A INPUT -i eth0 -j ACCEPT"}
	if got := CheckEgress(ctx, tb.run, nets); got != Stale {
		t.Errorf("CheckEgress with the egress jump behind the job bridge's = %v", got)
	}
	if err := ApplyEgress(ctx, tb.run, nets); err != nil {
		t.Fatal(err)
	}
	if got := tb["iptables"]["INPUT"]; !reflect.DeepEqual(got, []string{egress, input, "-A INPUT -i eth0 -j ACCEPT"}) {
		t.Errorf("iptables INPUT after ApplyEgress = %q", got)
	}

	if err := ApplyEgress(ctx, tb.run, nil); err != nil {
		t.Fatal(err)
	}
	if got := CheckEgress(ctx, tb.run, nil); got != Absent {
		t.Errorf("CheckEgress after removing the chain = %v", got)
	}
	if got := tb["iptables"]["INPUT"]; slices.Contains(got, egress) {
		t.Errorf("iptables INPUT still jumps to the egress chain: %q", got)
	}

	if err := ApplyEgress(ctx, tb.run, nets); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, tb.run, "boxrunner0"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tb, newTables()) {
		t.Errorf("after Remove: %q", tb)
	}
}
