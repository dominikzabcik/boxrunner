package firewall

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRestoreInput(t *testing.T) {
	want := `*filter
:BOXRUNNER-INPUT - [0:0]
:BOXRUNNER-FORWARD - [0:0]
-A BOXRUNNER-INPUT ! -i boxrunner0 -j RETURN
-A BOXRUNNER-INPUT -p udp --dport 53 -j RETURN
-A BOXRUNNER-INPUT -p tcp --dport 53 -j RETURN
-A BOXRUNNER-INPUT -j DROP
-A BOXRUNNER-FORWARD -p udp --dport 53 -j RETURN
-A BOXRUNNER-FORWARD -p tcp --dport 53 -j RETURN
-A BOXRUNNER-FORWARD -d fc00::/7 -j DROP
-A BOXRUNNER-FORWARD -d fe80::/10 -j DROP
COMMIT
`
	if got := V6.RestoreInput(withMirror); got != want {
		t.Errorf("V6.RestoreInput =\n%s\nwant\n%s", got, want)
	}
	if got := V4.RestoreInput(plain); !strings.Contains(got, "-A BOXRUNNER-FORWARD -d 100.64.0.0/10 -j DROP\n-A BOXRUNNER-FORWARD -d 169.254.0.0/16 -j DROP\n") {
		t.Errorf("V4.RestoreInput misses the Tailscale or metadata range:\n%s", got)
	}
	input := ":BOXRUNNER-FORWARD - [0:0]\n" +
		"-A BOXRUNNER-INPUT -d 172.18.0.1/32 ! -i boxrunner0 -p tcp -m tcp --dport 5000 -j DROP\n" +
		"-A BOXRUNNER-INPUT ! -i boxrunner0 -j RETURN\n" +
		"-A BOXRUNNER-INPUT -p udp --dport 53 -j RETURN\n" +
		"-A BOXRUNNER-INPUT -p tcp --dport 53 -j RETURN\n" +
		"-A BOXRUNNER-INPUT -d 172.18.0.1/32 -p tcp -m tcp --dport 5000 -j ACCEPT\n" +
		"-A BOXRUNNER-INPUT -j DROP\n"
	if got := V4.RestoreInput(withMirror); !strings.Contains(got, input) {
		t.Errorf("V4.RestoreInput with the mirror =\n%s\nwant it to contain\n%s", got, input)
	}
}

var (
	plain      = Rules{Bridge: "boxrunner0"}
	withMirror = Rules{Bridge: "boxrunner0", Open: []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5000")}}
)

// tables simulates iptables and ip6tables: chain name to rules, per command.
type tables map[string]map[string][]string

func newTables() tables {
	t := tables{}
	for _, cmd := range []string{"iptables", "ip6tables"} {
		t[cmd] = map[string][]string{"INPUT": {"-A INPUT -i eth0 -j ACCEPT"}, "FORWARD": nil}
	}
	t["iptables"]["DOCKER-USER"] = nil
	return t
}

var errNo = errors.New("exit status 1")

func (t tables) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	cmd := strings.TrimSuffix(name, "-restore")
	chains := t[cmd]
	if strings.HasSuffix(name, "-restore") {
		for _, line := range strings.Split(stdin, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				chains[strings.Fields(line[1:])[0]] = nil
			case strings.HasPrefix(line, "-A "):
				c := strings.Fields(line)[1]
				chains[c] = append(chains[c], line)
			}
		}
		return "", nil
	}
	args = args[1:] // -w
	op, chain := args[0], args[1]
	rules, exists := chains[chain]
	if !exists {
		return "", errNo
	}
	rule := func(from int) string { return strings.Join(append([]string{"-A", chain}, args[from:]...), " ") }
	switch op {
	case "-S":
		return strings.Join(append([]string{"-N " + chain}, rules...), "\n") + "\n", nil
	case "-C":
		if slices.Contains(rules, rule(2)) {
			return "", nil
		}
		return "", errNo
	case "-I":
		chains[chain] = append([]string{rule(3)}, rules...)
	case "-D":
		i := slices.Index(rules, rule(2))
		if i < 0 {
			return "", errNo
		}
		if chains[chain] = slices.Delete(rules, i, i+1); len(chains[chain]) == 0 {
			chains[chain] = nil
		}
	case "-F":
		chains[chain] = nil
	case "-X":
		delete(chains, chain)
	}
	return "", nil
}

func TestApplyCheckRemove(t *testing.T) {
	ctx := context.Background()
	tb := newTables()
	if got := Check(ctx, tb.run, plain); got != Absent {
		t.Fatalf("Check on a clean host = %v", got)
	}
	for range 2 {
		if err := Apply(ctx, tb.run, withMirror); err != nil {
			t.Fatal(err)
		}
	}
	if got := Check(ctx, tb.run, withMirror); got != Current {
		t.Errorf("Check after Apply with the mirror = %v", got)
	}
	if got := Check(ctx, tb.run, plain); got != Stale {
		t.Errorf("Check without the mirror after Apply with it = %v", got)
	}
	if err := Apply(ctx, tb.run, plain); err != nil {
		t.Fatal(err)
	}
	if got := Check(ctx, tb.run, plain); got != Current {
		t.Errorf("Check after Apply = %v", got)
	}
	if got := tb["iptables"][InputChain]; len(got) != 4 || slices.ContainsFunc(got, func(r string) bool { return strings.Contains(r, "5000") }) {
		t.Errorf("iptables %s after Apply without the mirror = %q", InputChain, got)
	}
	jump := "-i boxrunner0 -m comment --comment boxrunner -j "
	want := map[string][]string{
		"INPUT":       {"-A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT", "-A INPUT -i eth0 -j ACCEPT"},
		"DOCKER-USER": {"-A DOCKER-USER " + jump + "BOXRUNNER-FORWARD"},
		"FORWARD":     nil,
	}
	for chain, rules := range want {
		if got := tb["iptables"][chain]; !reflect.DeepEqual(got, rules) {
			t.Errorf("iptables %s = %q", chain, got)
		}
	}
	if got := tb["ip6tables"]["FORWARD"]; !reflect.DeepEqual(got, []string{"-A FORWARD " + jump + "BOXRUNNER-FORWARD"}) {
		t.Errorf("ip6tables FORWARD without DOCKER-USER = %q", got)
	}

	tb["iptables"][ForwardChain] = tb["iptables"][ForwardChain][1:]
	if got := Check(ctx, tb.run, plain); got != Stale {
		t.Errorf("Check with a rule missing = %v", got)
	}

	if err := Remove(ctx, tb.run, "boxrunner0"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tb, newTables()) {
		t.Errorf("after Remove: %q", tb)
	}
	if got := Check(ctx, tb.run, plain); got != Absent {
		t.Errorf("Check after Remove = %v", got)
	}
}

func TestApplyReplacesTheBridgeOnlyInputJump(t *testing.T) {
	ctx := context.Background()
	tb := newTables()
	legacy := "-A INPUT -i boxrunner0 -m comment --comment boxrunner -j BOXRUNNER-INPUT"
	tb["iptables"]["INPUT"] = append([]string{legacy}, tb["iptables"]["INPUT"]...)
	if err := Apply(ctx, tb.run, withMirror); err != nil {
		t.Fatal(err)
	}
	want := []string{"-A INPUT -m comment --comment boxrunner -j BOXRUNNER-INPUT", "-A INPUT -i eth0 -j ACCEPT"}
	if got := tb["iptables"]["INPUT"]; !reflect.DeepEqual(got, want) {
		t.Errorf("iptables INPUT after Apply = %q", got)
	}
	tb["iptables"]["INPUT"] = append([]string{legacy}, tb["iptables"]["INPUT"]...)
	if err := Remove(ctx, tb.run, "boxrunner0"); err != nil {
		t.Fatal(err)
	}
	if got := tb["iptables"]["INPUT"]; !reflect.DeepEqual(got, []string{"-A INPUT -i eth0 -j ACCEPT"}) {
		t.Errorf("iptables INPUT after Remove = %q", got)
	}
}

func TestRestoreInputOpensEachService(t *testing.T) {
	r := Rules{Bridge: "boxrunner0", Open: []netip.AddrPort{netip.MustParseAddrPort("172.18.0.1:5000"), netip.MustParseAddrPort("172.18.0.1:5001")}}
	input := ":BOXRUNNER-FORWARD - [0:0]\n" +
		"-A BOXRUNNER-INPUT -d 172.18.0.1/32 ! -i boxrunner0 -p tcp -m tcp --dport 5000 -j DROP\n" +
		"-A BOXRUNNER-INPUT -d 172.18.0.1/32 ! -i boxrunner0 -p tcp -m tcp --dport 5001 -j DROP\n" +
		"-A BOXRUNNER-INPUT ! -i boxrunner0 -j RETURN\n" +
		"-A BOXRUNNER-INPUT -p udp --dport 53 -j RETURN\n" +
		"-A BOXRUNNER-INPUT -p tcp --dport 53 -j RETURN\n" +
		"-A BOXRUNNER-INPUT -d 172.18.0.1/32 -p tcp -m tcp --dport 5000 -j ACCEPT\n" +
		"-A BOXRUNNER-INPUT -d 172.18.0.1/32 -p tcp -m tcp --dport 5001 -j ACCEPT\n" +
		"-A BOXRUNNER-INPUT -j DROP\n"
	if got := V4.RestoreInput(r); !strings.Contains(got, input) {
		t.Errorf("V4.RestoreInput with the mirror and the cache =\n%s\nwant it to contain\n%s", got, input)
	}
}
