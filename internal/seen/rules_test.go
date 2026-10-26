package seen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestScript(t *testing.T) {
	want := `add table inet boxrunner_seen
add chain inet boxrunner_seen forward { type filter hook forward priority 10; policy accept; }
flush chain inet boxrunner_seen forward
add rule inet boxrunner_seen forward iifname "boxrunner0" ct state new limit rate 100/second burst 200 packets log group 4138 snaplen 96
add rule inet boxrunner_seen forward iifname "boxrunner-*" ct state new limit rate 100/second burst 200 packets log group 4138 snaplen 96
`
	if got := Script(4138); got != want {
		t.Errorf("Script =\n%s\nwant\n%s", got, want)
	}
}

type commands struct {
	stdin []string
	calls []string
	out   map[string]string
	fail  map[string]bool
}

func (c *commands) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	c.calls = append(c.calls, call)
	c.stdin = append(c.stdin, stdin)
	if c.fail[call] {
		return "Error: no", errors.New("exit status 1")
	}
	return c.out[call], nil
}

func TestApply(t *testing.T) {
	c := &commands{}
	if err := Apply(context.Background(), c.run); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 1 || c.calls[0] != "nft -f -" || c.stdin[0] != Script(4138) {
		t.Errorf("ran %q with %q, want nft -f - with the script", c.calls, c.stdin)
	}
	c.fail = map[string]bool{"nft -f -": true}
	if err := Apply(context.Background(), c.run); err == nil || !strings.Contains(err.Error(), "Error: no") {
		t.Errorf("Apply = %v, want nft's message", err)
	}
}

const listing = `table inet boxrunner_seen {
	chain forward {
		type filter hook forward priority filter + 10; policy accept;
		iifname "boxrunner0" ct state new limit rate 100/second burst 200 packets log group 4138 snaplen 96
		iifname "boxrunner-*" ct state new limit rate 100/second burst 200 packets log group 4138 snaplen 96
	}
}
`

func TestIntact(t *testing.T) {
	list := "nft list table inet boxrunner_seen"
	for _, tc := range []struct {
		name string
		c    *commands
		want bool
	}{
		{"the table as nft prints it", &commands{out: map[string]string{list: listing}}, true},
		{"no table", &commands{fail: map[string]bool{list: true}}, false},
		{"a table without its rule", &commands{out: map[string]string{list: "table inet boxrunner_seen {\n\tchain forward {\n\t}\n}\n"}}, false},
		{"a table without the rule for the egress bridges", &commands{out: map[string]string{list: strings.ReplaceAll(listing, `"boxrunner-*"`, `"boxrunner-x"`)}}, false},
		{"another group", &commands{out: map[string]string{list: strings.ReplaceAll(listing, "4138", "4137")}}, false},
	} {
		if got := Intact(context.Background(), tc.c.run); got != tc.want {
			t.Errorf("%s: Intact = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRemove(t *testing.T) {
	c := &commands{fail: map[string]bool{"nft list table inet boxrunner_seen": true}}
	if err := Remove(context.Background(), c.run); err != nil || len(c.calls) != 1 {
		t.Errorf("without a table: Remove = %v after %q, want nil after one call", err, c.calls)
	}
	c = &commands{}
	if err := Remove(context.Background(), c.run); err != nil || c.calls[1] != "nft delete table inet boxrunner_seen" {
		t.Errorf("with a table: Remove = %v after %q, want the delete", err, c.calls)
	}
	c = &commands{fail: map[string]bool{"nft delete table inet boxrunner_seen": true}}
	if err := Remove(context.Background(), c.run); err == nil {
		t.Error("a failed delete was not an error")
	}
}
