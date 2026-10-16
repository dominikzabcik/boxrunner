package egress

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/firewall"
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
		Classes: []Class{{
			Net:    agentNet(),
			Policy: mustPolicy(t, "10.20.0.0/16:5432", "203.0.113.0/24:*", "10.1.1.1"),
			Open:   []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5000"), netip.MustParseAddrPort("172.18.0.1:5001")},
		}},
		Deny:  []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("172.19.0.0/16")},
		Group: 1337,
	}
	dyn, anyPort := r.Classes[0].Sets()
	if dyn != "cd4f0b_dyn_DYNSUM" || anyPort != "cd4f0b_any_DYNSUM" {
		t.Logf("sets %s %s", dyn, anyPort)
	}
	want := `add table inet boxrunner
add chain inet boxrunner forward { type filter hook forward priority -10; policy accept; }
flush chain inet boxrunner forward
add chain inet boxrunner input { type filter hook input priority -10; policy accept; }
flush chain inet boxrunner input
add set inet boxrunner SETDYN { type ipv4_addr . inet_service; flags timeout; size 65536; }
add set inet boxrunner SETANY { type ipv4_addr; flags timeout; size 65536; }
add counter inet boxrunner cd4f0b_blocked
add chain inet boxrunner blocked_cd4f0b
flush chain inet boxrunner blocked_cd4f0b
add rule inet boxrunner blocked_cd4f0b limit rate 20/second burst 40 packets log group 1337 prefix "cd4f0b" snaplen 96
add rule inet boxrunner blocked_cd4f0b counter name "cd4f0b_blocked" drop
add chain inet boxrunner egress_cd4f0b
flush chain inet boxrunner egress_cd4f0b
add rule inet boxrunner egress_cd4f0b ip saddr != 172.19.0.0/16 goto blocked_cd4f0b
add rule inet boxrunner egress_cd4f0b meta nfproto ipv6 goto blocked_cd4f0b
add rule inet boxrunner egress_cd4f0b ip daddr { 0.0.0.0/8, 100.100.100.200/32, 127.0.0.0/8, 168.63.129.16/32, 169.254.0.0/16, 172.18.0.0/16, 172.19.0.0/16, 192.0.0.192/32, 224.0.0.0/4, 240.0.0.0/4 } goto blocked_cd4f0b
add rule inet boxrunner egress_cd4f0b ct state established meta mark set meta mark or 0x10000000 accept
add rule inet boxrunner egress_cd4f0b ip daddr . tcp dport @SETDYN meta mark set meta mark or 0x10000000 accept
add rule inet boxrunner egress_cd4f0b meta l4proto tcp ip daddr @SETANY meta mark set meta mark or 0x10000000 accept
add rule inet boxrunner egress_cd4f0b ip daddr 10.20.0.0/16 tcp dport 5432 meta mark set meta mark or 0x10000000 accept
add rule inet boxrunner egress_cd4f0b meta l4proto tcp ip daddr 203.0.113.0/24 meta mark set meta mark or 0x10000000 accept
add rule inet boxrunner egress_cd4f0b ip daddr 10.1.1.1/32 tcp dport { 80, 443 } meta mark set meta mark or 0x10000000 accept
add rule inet boxrunner egress_cd4f0b goto blocked_cd4f0b
add chain inet boxrunner host_cd4f0b
flush chain inet boxrunner host_cd4f0b
add rule inet boxrunner host_cd4f0b meta nfproto ipv6 goto blocked_cd4f0b
add rule inet boxrunner host_cd4f0b ip daddr 172.19.0.1 udp dport 53 accept
add rule inet boxrunner host_cd4f0b ip daddr 172.19.0.1 tcp dport 53 accept
add rule inet boxrunner host_cd4f0b ip daddr 172.18.0.1 tcp dport 5000 accept
add rule inet boxrunner host_cd4f0b ip daddr 172.18.0.1 tcp dport 5001 accept
add rule inet boxrunner host_cd4f0b goto blocked_cd4f0b
add rule inet boxrunner forward iifname "boxrunner-d4f0b" jump egress_cd4f0b
add rule inet boxrunner input iifname "boxrunner-d4f0b" jump host_cd4f0b
add rule inet boxrunner input ip daddr 172.19.0.1 iifname != { "boxrunner-d4f0b", "lo" } drop
`
	want = strings.NewReplacer("SETDYN", dyn, "SETANY", anyPort).Replace(want)
	if got := r.Script(""); got != want {
		t.Errorf("Script =\n%s\nwant\n%s", got, want)
	}
}

func TestSetsChangeWithTheEntries(t *testing.T) {
	a := Class{Net: agentNet(), Policy: mustPolicy(t, "api.example.com")}
	same := Class{Net: agentNet(), Policy: mustPolicy(t, "api.example.com")}
	other := Class{Net: agentNet(), Policy: mustPolicy(t, "api.example.com:8443")}
	adyn, aany := a.Sets()
	sdyn, sany := same.Sets()
	odyn, oany := other.Sets()
	if adyn != sdyn || aany != sany {
		t.Errorf("the same entries name different sets: %s %s and %s %s", adyn, aany, sdyn, sany)
	}
	if adyn == odyn || aany == oany || !strings.HasPrefix(adyn, "cd4f0b_dyn_") || !strings.HasPrefix(oany, "cd4f0b_any_") {
		t.Errorf("a changed port keeps the sets: %s %s and %s %s", adyn, aany, odyn, oany)
	}
}

func TestIntactNoticesAFlushedTable(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}}
	listing := `table inet boxrunner {
	counter cd4f0b_blocked {
		packets 0 bytes 0
	}
	set ` + func() string { d, _ := r.Classes[0].Sets(); return d }() + ` {
	}
	set ` + func() string { _, a := r.Classes[0].Sets(); return a }() + ` {
	}
	chain blocked_cd4f0b {
	}
	chain egress_cd4f0b {
	}
	chain host_cd4f0b {
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname "boxrunner-d4f0b" jump egress_cd4f0b
	}
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname "boxrunner-d4f0b" jump host_cd4f0b
	}
}
`
	if !r.Intact(listing) {
		t.Error("Intact says a whole table is not")
	}
	for label, broken := range map[string]string{
		"no table":         "",
		"a missing chain":  strings.Replace(listing, "chain egress_cd4f0b {", "chain other {", 1),
		"a lost jump":      strings.Replace(listing, `iifname "boxrunner-d4f0b" jump egress_cd4f0b`, "", 1),
		"a lost host jump": strings.Replace(listing, `iifname "boxrunner-d4f0b" jump host_cd4f0b`, "", 1),
		"an old set":       strings.Replace(listing, "cd4f0b_dyn_", "cd4f0b_dyn_x", 1),
		"no input chain":   strings.Replace(listing, "chain input {", "chain output {", 1),
	} {
		if r.Intact(broken) {
			t.Errorf("Intact accepts %s", label)
		}
	}
}

func TestReplaceDeletesAndBuildsInOneScript(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}, Group: 1}
	got := r.Replace()
	if !strings.HasPrefix(got, "add table inet boxrunner\ndelete table inet boxrunner\nadd table inet boxrunner\n") || strings.Count(got, "delete table") != 1 {
		t.Errorf("Replace =\n%s", got)
	}
}

func TestScriptOfAClosedClassAllowsOnlyWhatIsEstablished(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Closed: true}}, Group: 1}
	got := r.Script("")
	if !strings.Contains(got, "egress_cd4f0b ct state established meta mark set meta mark or 0x10000000 accept\nadd rule inet boxrunner egress_cd4f0b goto blocked_cd4f0b\n") {
		t.Errorf("a closed class's chain is not just established connections:\n%s", got)
	}
	if strings.Contains(got, "@") || strings.Contains(got, "dport 53 accept") {
		t.Errorf("a closed class opens something:\n%s", got)
	}
}

// listingOf is what `nft list table` would print for the classes' objects,
// plus the chains of a class that left.
func listingOf(r Ruleset, extra string) string {
	var b strings.Builder
	b.WriteString("table inet boxrunner {\n")
	for _, c := range r.Classes {
		dyn, anyPort := c.Sets()
		fmt.Fprintf(&b, "\tcounter %s_blocked {\n\t\tpackets 4 bytes 240\n\t}\n\tset %s {\n\t}\n\tset %s {\n\t}\n", c.Net.ID(), dyn, anyPort)
		for _, chain := range []string{"blocked_", "egress_", "host_"} {
			fmt.Fprintf(&b, "\tchain %s%s {\n\t}\n", chain, c.Net.ID())
		}
		fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority filter - 10; policy accept;\n\t\tiifname %q jump egress_%s\n\t}\n", c.Net.Bridge, c.Net.ID())
		fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority filter - 10; policy accept;\n\t\tiifname %q jump host_%s\n\t}\n", c.Net.Bridge, c.Net.ID())
	}
	b.WriteString(extra + "}\n")
	return b.String()
}

const leftBehind = `	counter cc84a9_blocked {
		packets 3 bytes 180
	}
	set cc84a9_dyn_000000 {
	}
	set cc84a9_any_000000 {
	}
	chain blocked_cc84a9 {
	}
	chain egress_cc84a9 {
	}
	chain host_cc84a9 {
	}
`

func TestScriptRemovesWhatEarlierRulesetsLeftBehind(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}}
	dyn, anyPort := r.Classes[0].Sets()
	got := r.Script(listingOf(r, leftBehind) + "\tset cd4f0b_dyn_old {\n\t}\n")
	for _, want := range []string{
		"flush chain inet boxrunner egress_cc84a9\n", "flush chain inet boxrunner blocked_cc84a9\n", "flush chain inet boxrunner host_cc84a9\n",
		"delete chain inet boxrunner egress_cc84a9\n", "delete chain inet boxrunner blocked_cc84a9\n", "delete chain inet boxrunner host_cc84a9\n",
		"delete set inet boxrunner cc84a9_dyn_000000\n", "delete set inet boxrunner cd4f0b_dyn_old\n", "delete counter inet boxrunner cc84a9_blocked\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Script does not contain %q:\n%s", want, got)
		}
	}
	for _, kept := range []string{
		"delete chain inet boxrunner egress_cd4f0b", "delete set inet boxrunner " + dyn, "delete set inet boxrunner " + anyPort,
		"delete counter inet boxrunner cd4f0b_blocked", "delete chain inet boxrunner forward", "delete chain inet boxrunner input",
		"flush set", "delete table",
	} {
		if strings.Contains(got, kept) {
			t.Errorf("Script contains %q, which would close addresses or drop the hooks:\n%s", kept, got)
		}
	}
	if i, j := strings.Index(got, "flush chain inet boxrunner forward"), strings.Index(got, "delete chain inet boxrunner egress_cc84a9"); i < 0 || j < i {
		t.Errorf("the forward chain must be flushed before the chains it jumps to are deleted:\n%s", got)
	}
}

func TestElementScript(t *testing.T) {
	c := Class{Net: agentNet(), Policy: mustPolicy(t)}
	dyn, anyPort := c.Sets()
	var elements []Element
	for _, g := range []Grant{
		{Addr: netip.MustParseAddr("203.0.113.5"), Ports: DefaultPorts(), TTL: 5 * time.Minute},
		{Addr: netip.MustParseAddr("198.51.100.7"), Ports: Ports{Any: true}, TTL: 10 * time.Minute},
		{Addr: netip.MustParseAddr("203.0.113.6"), Ports: onePort(8443), TTL: time.Hour},
		{Addr: netip.MustParseAddr("2001:db8::9"), Ports: Ports{Any: true}, TTL: 10 * time.Minute},
	} {
		elements = append(elements, g.Elements()...)
	}
	add := "add element inet boxrunner " + dyn + " { 203.0.113.5 . 80 timeout 300s, 203.0.113.5 . 443 timeout 300s, 203.0.113.6 . 8443 timeout 3600s }\n"
	del := "delete element inet boxrunner " + dyn + " { 203.0.113.5 . 80, 203.0.113.5 . 443, 203.0.113.6 . 8443 }\n"
	addAny := "add element inet boxrunner " + anyPort + " { 198.51.100.7 timeout 600s }\n"
	delAny := "delete element inet boxrunner " + anyPort + " { 198.51.100.7 }\n"
	if got, want := ElementScript(c, elements), add+del+add+addAny+delAny+addAny; got != want {
		t.Errorf("ElementScript =\n%s\nwant\n%s", got, want)
	}
}

// nft records the scripts it is given and answers the listing it is told to.
type nft struct {
	mu      sync.Mutex
	listing string
	scripts []string
	fail    func(script string) error
}

func (n *nft) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
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

func agentClass(t *testing.T) Class { return Class{Net: agentNet(), Policy: mustPolicy(t)} }

// opened is the script that sets one address on one port for ttl seconds.
func opened(c Class, addr string, port uint16, ttl int) string {
	dyn, _ := c.Sets()
	el := fmt.Sprintf("%s . %d", addr, port)
	add := fmt.Sprintf("add element inet boxrunner %s { %s timeout %ds }\n", dyn, el, ttl)
	return add + fmt.Sprintf("delete element inet boxrunner %s { %s }\n", dyn, el) + add
}

func TestOpenSkipsAddressesThatHaveMostOfTheirTimeLeft(t *testing.T) {
	f := &nft{}
	now := time.Unix(1_000_000, 0)
	e := &Enforcer{Run: f.run, Now: func() time.Time { return now }}
	c := agentClass(t)
	grants := []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(443), TTL: 10 * time.Minute}}
	for range 2 {
		if _, err := e.Open(context.Background(), c, grants); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{opened(c, "203.0.113.5", 443, 600)}; !reflect.DeepEqual(f.scripts, want) {
		t.Fatalf("scripts = %q, want one script for the same grant twice", f.scripts)
	}
	now = now.Add(6 * time.Minute)
	if _, err := e.Open(context.Background(), c, grants); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 2 || f.scripts[1] != opened(c, "203.0.113.5", 443, 600) {
		t.Errorf("scripts = %q, want the element set again once it had less than half its time left", f.scripts)
	}
	if _, err := e.Open(context.Background(), c, []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(80), TTL: time.Minute}}); err != nil || len(f.scripts) != 3 || f.scripts[2] != opened(c, "203.0.113.5", 80, 60) {
		t.Errorf("another port was not opened: %v, scripts %q", err, f.scripts)
	}
	if _, err := e.Open(context.Background(), c, []Grant{{Addr: netip.MustParseAddr("2001:db8::1"), Ports: onePort(80), TTL: time.Minute}}); err != nil || len(f.scripts) != 3 {
		t.Errorf("an IPv6 address was opened: %v, %d scripts", err, len(f.scripts))
	}
}

func TestOpenKeepsATimeForEachPortOfAnAddress(t *testing.T) {
	f := &nft{}
	e := &Enforcer{Run: f.run, Now: func() time.Time { return time.Unix(1_000_000, 0) }}
	c := agentClass(t)
	addr := netip.MustParseAddr("203.0.113.5")
	if _, err := e.Open(context.Background(), c, []Grant{{Addr: addr, Ports: DefaultPorts(), TTL: 10 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	// Another name on the same address that allows 443 finds that element open
	// and leaves it, and opens the port the first name did not.
	if _, err := e.Open(context.Background(), c, []Grant{{Addr: addr, Ports: Ports{List: []uint16{443, 8443}}, TTL: 10 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 2 || f.scripts[1] != opened(c, "203.0.113.5", 8443, 600) {
		t.Errorf("scripts = %q, want the second to set only port 8443", f.scripts)
	}
}

func TestOpenReportsFailureAndDoesNotRememberIt(t *testing.T) {
	f := &nft{fail: func(string) error { return errors.New("exit status 1") }}
	e := &Enforcer{Run: f.run}
	grants := []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(443), TTL: time.Minute}}
	if _, err := e.Open(context.Background(), agentClass(t), grants); err == nil {
		t.Fatal("Open succeeded though nft failed")
	}
	f.fail = nil
	if _, err := e.Open(context.Background(), agentClass(t), grants); err != nil || len(f.scripts) != 2 {
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
	if _, err := e.Open(context.Background(), agentClass(t), grants); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Open(context.Background(), agentClass(t), []Grant{{Addr: netip.MustParseAddr("203.0.113.5"), Ports: onePort(443), TTL: time.Hour}}); !errors.Is(err, ErrFull) {
		t.Errorf("Open past the limit = %v, want ErrFull", err)
	}
	if _, err := e.Open(context.Background(), agentClass(t), grants[:1]); err != nil {
		t.Errorf("renewing an address that is open = %v", err)
	}
}

func TestApplyReplacesTheTableAtomicallyWhenTheScriptFails(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}}
	ctx := context.Background()

	f := &nft{listing: listingOf(r, leftBehind)}
	if err := (&Enforcer{Run: f.run}).Apply(ctx, r); err != nil || len(f.scripts) != 1 || !strings.Contains(f.scripts[0], "delete set inet boxrunner cc84a9_dyn_000000") {
		t.Fatalf("Apply = %v, scripts %q", err, f.scripts)
	}

	f = &nft{listing: listingOf(r, "")}
	failures := 0
	f.fail = func(string) error {
		if failures++; failures == 1 {
			return errors.New("exit status 1")
		}
		return nil
	}
	if err := (&Enforcer{Run: f.run}).Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 2 || !strings.HasPrefix(f.scripts[1], "add table inet boxrunner\ndelete table inet boxrunner\n") {
		t.Errorf("after a failed script, scripts = %q; want one transaction that deletes the table and builds it again", f.scripts)
	}

	f = &nft{}
	f.fail = func(string) error { return errors.New("exit status 1") }
	if err := (&Enforcer{Run: f.run}).Apply(ctx, r); err == nil {
		t.Error("Apply succeeded though nft failed on a new table")
	}
	if len(f.scripts) != 1 {
		t.Errorf("a new table that fails was replaced: %q", f.scripts)
	}
}

func TestVerifyAppliesAgainOnlyWhenTheTableIsNotWhole(t *testing.T) {
	r := Ruleset{Classes: []Class{{Net: agentNet(), Policy: mustPolicy(t)}}}
	f := &nft{listing: listingOf(r, "")}
	e := &Enforcer{Run: f.run}
	if whole, err := e.Verify(context.Background(), r); err != nil || !whole || len(f.scripts) != 0 {
		t.Errorf("Verify of a whole table = %v, %v, scripts %q", whole, err, f.scripts)
	}
	f.listing = ""
	if whole, err := e.Verify(context.Background(), r); err != nil || whole || len(f.scripts) != 1 {
		t.Errorf("Verify of a flushed table = %v, %v, scripts %d", whole, err, len(f.scripts))
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
	f.listing = listingOf(Ruleset{}, leftBehind)
	if err := Remove(context.Background(), f.run); err != nil || !reflect.DeepEqual(f.scripts, []string{"delete table"}) {
		t.Errorf("Remove = %v, scripts %q", err, f.scripts)
	}
}

func TestHostNetOpensWhatTheClassUses(t *testing.T) {
	n := agentNet()
	mirror := netip.MustParseAddr("172.18.0.1")
	both := firewall.Services{Mirror: true, Cache: true}
	for _, c := range []struct {
		name          string
		docker, cache bool
		services      firewall.Services
		want          []netip.AddrPort
	}{
		{"a class with Docker and the cache", true, true, both, []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5000"), netip.MustParseAddrPort("172.18.0.1:5001")}},
		{"a class with Docker only", true, false, both, []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5000")}},
		{"a class with the cache only", false, true, both, []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5001")}},
		{"a class that uses neither", false, false, both, nil},
		{"a box that serves neither", true, true, firewall.Services{}, nil},
	} {
		got := HostNet(n, c.docker, c.cache, c.services, mirror)
		if got.Bridge != n.Bridge || got.Gateway != n.Gateway || !reflect.DeepEqual(got.Open, c.want) {
			t.Errorf("%s: HostNet = %+v, want open %v", c.name, got, c.want)
		}
	}
}
