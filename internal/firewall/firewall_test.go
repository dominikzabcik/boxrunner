package firewall

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRestoreInput(t *testing.T) {
	want := `*filter
:BOXRUNNER-INPUT - [0:0]
:BOXRUNNER-FORWARD - [0:0]
-A BOXRUNNER-INPUT -p udp --dport 53 -j RETURN
-A BOXRUNNER-INPUT -p tcp --dport 53 -j RETURN
-A BOXRUNNER-INPUT -j DROP
-A BOXRUNNER-FORWARD -p udp --dport 53 -j RETURN
-A BOXRUNNER-FORWARD -p tcp --dport 53 -j RETURN
-A BOXRUNNER-FORWARD -d fc00::/7 -j DROP
-A BOXRUNNER-FORWARD -d fe80::/10 -j DROP
COMMIT
`
	if got := V6.RestoreInput(); got != want {
		t.Errorf("V6.RestoreInput =\n%s\nwant\n%s", got, want)
	}
	if got := V4.RestoreInput(); !strings.Contains(got, "-A BOXRUNNER-FORWARD -d 100.64.0.0/10 -j DROP\n-A BOXRUNNER-FORWARD -d 169.254.0.0/16 -j DROP\n") {
		t.Errorf("V4.RestoreInput misses the Tailscale or metadata range:\n%s", got)
	}
}

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
	if got := Check(ctx, tb.run, "boxrunner0"); got != Absent {
		t.Fatalf("Check on a clean host = %v", got)
	}
	for range 2 {
		if err := Apply(ctx, tb.run, "boxrunner0"); err != nil {
			t.Fatal(err)
		}
	}
	if got := Check(ctx, tb.run, "boxrunner0"); got != Current {
		t.Errorf("Check after Apply = %v", got)
	}
	jump := "-i boxrunner0 -m comment --comment boxrunner -j "
	want := map[string][]string{
		"INPUT":       {"-A INPUT " + jump + "BOXRUNNER-INPUT", "-A INPUT -i eth0 -j ACCEPT"},
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
	if got := Check(ctx, tb.run, "boxrunner0"); got != Stale {
		t.Errorf("Check with a rule missing = %v", got)
	}

	if err := Remove(ctx, tb.run, "boxrunner0"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tb, newTables()) {
		t.Errorf("after Remove: %q", tb)
	}
	if got := Check(ctx, tb.run, "boxrunner0"); got != Absent {
		t.Errorf("Check after Remove = %v", got)
	}
}
