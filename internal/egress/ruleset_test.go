package egress

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustPolicy(t *testing.T, entries ...string) Policy {
	t.Helper()
	p, err := Parse(append([]string{"github.com", "api.github.com", "*.actions.githubusercontent.com"}, entries...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func agentNet() Net {
	n := NetOf("agent")
	n.Subnet, n.Gateway = netip.MustParsePrefix("172.19.0.0/16"), netip.MustParseAddr("172.19.0.1")
	return n
}

func TestNetOfNamesTheClassesBridge(t *testing.T) {
	n := NetOf("agent")
	if n.Name != "boxrunner-agent" || n.Bridge != "boxrunner-d4f0b" || n.ID() != "cd4f0b" {
		t.Errorf("NetOf(agent) = %+v, id %s", n, n.ID())
	}
	long := NetOf(strings.Repeat("a", 47))
	if len(long.Bridge) > 15 {
		t.Errorf("bridge %q is longer than an interface name may be", long.Bridge)
	}
	if NetOf("agent").Bridge == NetOf("coder").Bridge {
		t.Error("two classes share a bridge")
	}
}

func TestScriptBuildsTheClassesChain(t *testing.T) {
	r := Ruleset{
		Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t, "10.20.0.0/16:5432", "203.0.113.0/24:*", "[2001:db8::/32]:8443")}},
		Deny:    []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("172.19.0.0/16")},
		Group:   1337,
	}
	want := `add table inet boxrunner
add chain inet boxrunner forward { type filter hook forward priority -10; policy accept; }
flush chain inet boxrunner forward
add set inet boxrunner cd4f0b_dyn4 { type ipv4_addr . inet_service; flags timeout; size 65536; }
add set inet boxrunner cd4f0b_dyn6 { type ipv6_addr . inet_service; flags timeout; size 65536; }
add set inet boxrunner cd4f0b_any4 { type ipv4_addr; flags timeout; size 65536; }
add set inet boxrunner cd4f0b_any6 { type ipv6_addr; flags timeout; size 65536; }
add counter inet boxrunner cd4f0b_blocked
add chain inet boxrunner blocked_cd4f0b
flush chain inet boxrunner blocked_cd4f0b
add rule inet boxrunner blocked_cd4f0b limit rate 20/second burst 40 packets log group 1337 prefix "cd4f0b" snaplen 96
add rule inet boxrunner blocked_cd4f0b counter name "cd4f0b_blocked" drop
add chain inet boxrunner egress_cd4f0b
flush chain inet boxrunner egress_cd4f0b
add rule inet boxrunner egress_cd4f0b ct state established,related accept
add rule inet boxrunner egress_cd4f0b ip daddr { 0.0.0.0/8, 100.100.100.200/32, 127.0.0.0/8, 168.63.129.16/32, 169.254.0.0/16, 172.18.0.0/16, 172.19.0.0/16, 192.0.0.192/32, 224.0.0.0/4, 240.0.0.0/4 } goto blocked_cd4f0b
add rule inet boxrunner egress_cd4f0b ip6 daddr { ::/128, ::1/128, fd00:ec2::254/128, fe80::/10, ff00::/8 } goto blocked_cd4f0b
add rule inet boxrunner egress_cd4f0b ip daddr . tcp dport @cd4f0b_dyn4 accept
add rule inet boxrunner egress_cd4f0b ip6 daddr . tcp dport @cd4f0b_dyn6 accept
add rule inet boxrunner egress_cd4f0b meta l4proto tcp ip daddr @cd4f0b_any4 accept
add rule inet boxrunner egress_cd4f0b meta l4proto tcp ip6 daddr @cd4f0b_any6 accept
add rule inet boxrunner egress_cd4f0b ip daddr 10.20.0.0/16 tcp dport 5432 accept
add rule inet boxrunner egress_cd4f0b meta l4proto tcp ip daddr 203.0.113.0/24 accept
add rule inet boxrunner egress_cd4f0b ip6 daddr 2001:db8::/32 tcp dport 8443 accept
add rule inet boxrunner egress_cd4f0b goto blocked_cd4f0b
add rule inet boxrunner forward iifname "boxrunner-d4f0b" jump egress_cd4f0b
`
	if got := r.Script(""); got != want {
		t.Errorf("Script =\n%s\nwant\n%s", got, want)
	}
}

func TestScriptOfAClosedClassAllowsOnlyWhatIsEstablished(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Closed: true}}, Group: 1}
	got := r.Script("")
	if !strings.Contains(got, "egress_cd4f0b ct state established,related accept\nadd rule inet boxrunner egress_cd4f0b goto blocked_cd4f0b\n") {
		t.Errorf("a closed class's chain is not just established connections:\n%s", got)
	}
	if strings.Contains(got, "accept\nadd rule inet boxrunner egress_cd4f0b ip daddr") || strings.Contains(got, "@cd4f0b_dyn4 accept") {
		t.Errorf("a closed class opens something:\n%s", got)
	}
}

const earlier = `table inet boxrunner {
	counter cc84a9_blocked {
		packets 3 bytes 180
	}
	counter cd4f0b_blocked {
		packets 4 bytes 240
	}
	set cc84a9_dyn4 {
		type ipv4_addr . inet_service
		flags timeout
	}
	set cd4f0b_dyn4 {
		type ipv4_addr . inet_service
		flags timeout
	}
	chain blocked_cc84a9 {
		counter name "cc84a9_blocked" drop
	}
	chain egress_cc84a9 {
		goto blocked_cc84a9
	}
	chain egress_cd4f0b {
		goto blocked_cd4f0b
	}
	chain forward {
		type filter hook forward priority -10; policy accept;
		iifname "boxrunner-c84a9" jump egress_cc84a9
	}
}
`

func TestScriptRemovesWhatEarlierRulesetsLeftBehind(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}}
	got := r.Script(earlier)
	for _, want := range []string{
		"flush chain inet boxrunner egress_cc84a9\n", "flush chain inet boxrunner blocked_cc84a9\n",
		"delete chain inet boxrunner egress_cc84a9\n", "delete chain inet boxrunner blocked_cc84a9\n",
		"delete set inet boxrunner cc84a9_dyn4\n", "delete counter inet boxrunner cc84a9_blocked\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Script does not contain %q:\n%s", want, got)
		}
	}
	for _, kept := range []string{"delete chain inet boxrunner egress_cd4f0b", "delete set inet boxrunner cd4f0b_dyn4", "delete counter inet boxrunner cd4f0b_blocked", "delete chain inet boxrunner forward", "flush chain inet boxrunner forward\nflush"} {
		if strings.Contains(got, kept) {
			t.Errorf("Script contains %q, which would close addresses or drop the hook:\n%s", kept, got)
		}
	}
	if i, j := strings.Index(got, "flush chain inet boxrunner forward"), strings.Index(got, "delete chain inet boxrunner egress_cc84a9"); i < 0 || j < i {
		t.Errorf("the forward chain must be flushed before the chains it jumps to are deleted:\n%s", got)
	}
}

func TestElementScript(t *testing.T) {
	grants := []Grant{
		{Addr: netip.MustParseAddr("203.0.113.5"), Ports: DefaultPorts(), TTL: 5 * time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::5"), Ports: onePort(8443), TTL: time.Hour},
		{Addr: netip.MustParseAddr("198.51.100.7"), Ports: Ports{Any: true}, TTL: 10 * time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::9"), Ports: Ports{Any: true}, TTL: 10 * time.Minute},
	}
	want := `add element inet boxrunner cd4f0b_dyn4 { 203.0.113.5 . 80 timeout 300s, 203.0.113.5 . 443 timeout 300s }
add element inet boxrunner cd4f0b_dyn6 { 2001:db8::5 . 8443 timeout 3600s }
add element inet boxrunner cd4f0b_any4 { 198.51.100.7 timeout 600s }
add element inet boxrunner cd4f0b_any6 { 2001:db8::9 timeout 600s }
`
	if got := ElementScript(agentNet(), grants); got != want {
		t.Errorf("ElementScript =\n%s\nwant\n%s", got, want)
	}
}

// nft records the scripts it is given and answers the listing it is told to.
type nft struct {
	listing string
	scripts []string
	fail    func(script string) error
}

func (n *nft) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	switch {
	case cmd == "nft list table inet boxrunner":
		if n.listing == "" {
			return "Error: No such file or directory", errors.New("exit status 1")
		}
		return n.listing, nil
	case cmd == "nft -f -":
		n.scripts = append(n.scripts, stdin)
		if n.fail != nil {
			return "Error: nft failed", n.fail(stdin)
		}
	case cmd == "nft delete table inet boxrunner":
		n.scripts = append(n.scripts, "delete table")
		n.listing = ""
	case cmd == "nft list counters table inet boxrunner":
		return `table inet boxrunner {
	counter cd4f0b_blocked {
		packets 17 bytes 1020
	}
	counter cc84a9_blocked {
		packets 0 bytes 0
	}
}
`, nil
	}
	return "", nil
}

func TestOpenSkipsAddressesThatHaveMostOfTheirTimeLeft(t *testing.T) {
	f := &nft{}
	now := time.Unix(1_000_000, 0)
	e := &Enforcer{Run: f.run, Now: func() time.Time { return now }}
	grants := []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(443), TTL: 10 * time.Minute}}
	for range 2 {
		if err := e.Open(context.Background(), agentNet(), grants); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.scripts) != 1 {
		t.Fatalf("nft ran %d times for the same grant, want 1", len(f.scripts))
	}
	now = now.Add(6 * time.Minute)
	if err := e.Open(context.Background(), agentNet(), grants); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 2 {
		t.Errorf("nft ran %d times, want the grant renewed once it had less than half its time left", len(f.scripts))
	}
	if err := e.Open(context.Background(), agentNet(), []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(80), TTL: time.Minute}}); err != nil || len(f.scripts) != 3 {
		t.Errorf("another port was not opened: %v, %d scripts", err, len(f.scripts))
	}
}

func TestOpenReportsFailureAndDoesNotRememberIt(t *testing.T) {
	f := &nft{fail: func(string) error { return errors.New("exit status 1") }}
	e := &Enforcer{Run: f.run}
	grants := []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(443), TTL: time.Minute}}
	if err := e.Open(context.Background(), agentNet(), grants); err == nil {
		t.Fatal("Open succeeded though nft failed")
	}
	f.fail = nil
	if err := e.Open(context.Background(), agentNet(), grants); err != nil || len(f.scripts) != 2 {
		t.Errorf("a failed grant was remembered as open: %v, %d scripts", err, len(f.scripts))
	}
}

func TestOpenRefusesPastTheLimit(t *testing.T) {
	f := &nft{}
	e := &Enforcer{Run: f.run}
	var grants []Grant
	for i := range MaxLive {
		grants = append(grants, Grant{Addr: netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), Ports: onePort(443), TTL: time.Hour})
	}
	if err := e.Open(context.Background(), agentNet(), grants); err != nil {
		t.Fatal(err)
	}
	if err := e.Open(context.Background(), agentNet(), []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(443), TTL: time.Hour}}); !errors.Is(err, ErrFull) {
		t.Errorf("Open past the limit = %v, want ErrFull", err)
	}
	if err := e.Open(context.Background(), agentNet(), grants[:1]); err != nil {
		t.Errorf("renewing an address that is open = %v", err)
	}
}

func TestApplyKeepsOpenAddressesAndReplacesATableOfAnotherShape(t *testing.T) {
	f := &nft{listing: earlier}
	e := &Enforcer{Run: f.run}
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}}
	if err := e.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 1 || !strings.Contains(f.scripts[0], "delete set inet boxrunner cc84a9_dyn4") {
		t.Fatalf("scripts = %q", f.scripts)
	}

	f = &nft{listing: earlier}
	failures := 0
	f.fail = func(string) error {
		if failures++; failures == 1 {
			return errors.New("exit status 1")
		}
		return nil
	}
	e = &Enforcer{Run: f.run}
	if err := e.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 3 || f.scripts[1] != "delete table" || strings.Contains(f.scripts[2], "delete set") {
		t.Errorf("after a failed script, scripts = %q; want the table deleted and built again", f.scripts)
	}

	f = &nft{}
	f.fail = func(string) error { return errors.New("exit status 1") }
	if err := (&Enforcer{Run: f.run}).Apply(context.Background(), r); err == nil {
		t.Error("Apply succeeded though nft failed on a new table")
	}
}

func TestBlockedReadsTheCountersByClass(t *testing.T) {
	f := &nft{}
	got, err := (&Enforcer{Run: f.run}).Blocked(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]uint64{"cd4f0b": 17, "cc84a9": 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("Blocked = %v, want %v", got, want)
	}
}

func TestRemoveDeletesTheTableOnlyWhenThereIsOne(t *testing.T) {
	f := &nft{}
	if err := Remove(context.Background(), f.run); err != nil || len(f.scripts) != 0 {
		t.Errorf("Remove without a table = %v, scripts %q", err, f.scripts)
	}
	f.listing = earlier
	if err := Remove(context.Background(), f.run); err != nil || !reflect.DeepEqual(f.scripts, []string{"delete table"}) {
		t.Errorf("Remove = %v, scripts %q", err, f.scripts)
	}
}
