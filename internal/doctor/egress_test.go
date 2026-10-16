package doctor

import (
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

var probe = EgressProbe{
	Allowed: "github.com", Denied: "example.com", DeniedAddr: netip.MustParseAddr("93.184.216.34"),
	Gateway: netip.MustParseAddr("172.19.0.1"), Resolver: netip.MustParseAddr("10.0.0.2"),
}

func TestEgressProbeScript(t *testing.T) {
	got := probe.Script()
	for _, line := range []string{
		"look lookup-allowed github.com\nprobe allowed github.com 443\n",
		"look lookup-denied example.com\nprobe denied example.com 443\n",
		"probe address 93.184.216.34 443\n",
		"probe host 172.19.0.1 22\nprobe metadata 169.254.169.254 80\n",
		"probe resolver-tcp 10.0.0.2 53\nask resolver-udp 10.0.0.2\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("the script does not contain %q:\n%s", line, got)
		}
	}
	bare := EgressProbe{Allowed: "github.com", Gateway: probe.Gateway}.Script()
	for _, line := range []string{"lookup-denied", "probe address", "resolver"} {
		if strings.Contains(bare, line) {
			t.Errorf("a probe with nothing to refuse tries %q:\n%s", line, bare)
		}
	}
}

func results(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

func TestJudgeEgressPassesAClassThatAllowsAndRefusesAsItShould(t *testing.T) {
	logs := "lookup-allowed 0\r\nallowed 0\r\nlookup-denied 2\r\ndenied 124\r\naddress 124\r\nhost 124\r\nmetadata 124\r\nresolver-tcp 124\r\nresolver-udp 124\r\n"
	got := results(JudgeEgress("agent", probe, logs))
	want := map[string]string{
		"egress agent allows":             "github.com resolves and answers on port 443 from a job container (lookup exit 0, connect exit 0)",
		"egress agent refuses a name":     "example.com does not resolve for a job, and cannot be reached",
		"egress agent refuses an address": "93.184.216.34, the address of example.com, is unreachable by number",
		"egress agent host":               "the host at 172.19.0.1 and the metadata service are unreachable from jobs",
		"egress agent resolver":           "a job cannot ask 10.0.0.2, the box's resolver, or any other DNS server",
	}
	if len(got) != len(want) {
		t.Errorf("JudgeEgress gave %d results: %v", len(got), got)
	}
	for name, detail := range want {
		if r, ok := got[name]; !ok || !r.OK || r.Detail != detail {
			t.Errorf("%s = %+v, want ok with %q", name, r, detail)
		}
	}
}

func TestJudgeEgressFailsWhatGetsThrough(t *testing.T) {
	for _, c := range []struct {
		name, logs, check string
	}{
		{"a name that resolves", "lookup-allowed 0\nallowed 0\nlookup-denied 0\ndenied 124\naddress 124\nhost 124\nmetadata 124\nresolver-tcp 124\nresolver-udp 124\n", "egress agent refuses a name"},
		{"a name that connects", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 0\naddress 124\nhost 124\nmetadata 124\nresolver-tcp 124\nresolver-udp 124\n", "egress agent refuses a name"},
		{"an address by number", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 0\nhost 124\nmetadata 124\nresolver-tcp 124\nresolver-udp 124\n", "egress agent refuses an address"},
		{"the host", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 124\nhost 0\nmetadata 124\nresolver-tcp 124\nresolver-udp 124\n", "egress agent host"},
		{"a host that answers with a refusal", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 124\nhost 1\nmetadata 124\nresolver-tcp 124\nresolver-udp 124\n", "egress agent host"},
		{"an address that answers with a refusal", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 1\nhost 124\nmetadata 124\nresolver-tcp 124\nresolver-udp 124\n", "egress agent refuses an address"},
		{"the metadata service", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 124\nhost 124\nmetadata 0\nresolver-tcp 124\nresolver-udp 124\n", "egress agent host"},
		{"the box's resolver over TCP", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 124\nhost 124\nmetadata 124\nresolver-tcp 0\nresolver-udp 124\n", "egress agent resolver"},
		{"another DNS server over UDP", "lookup-allowed 0\nallowed 0\nlookup-denied 2\ndenied 124\naddress 124\nhost 124\nmetadata 124\nresolver-tcp 124\nresolver-udp 0\n", "egress agent resolver"},
		{"a probe that gave no answer", "lookup-allowed 0\nallowed 0\n", "egress agent host"},
	} {
		r := results(JudgeEgress("agent", probe, c.logs))[c.check]
		if r.OK {
			t.Errorf("%s: %s passed: %+v", c.name, c.check, r)
		}
	}
	allows := results(JudgeEgress("agent", probe, "lookup-allowed 2\nallowed 124\n"))["egress agent allows"]
	if allows.OK {
		t.Errorf("a class whose allowed host does not answer passed: %+v", allows)
	}
}

func TestJudgeEgressSkipsWhatItCannotTry(t *testing.T) {
	p := EgressProbe{Allowed: "github.com", Gateway: probe.Gateway}
	got := results(JudgeEgress("agent", p, "lookup-allowed 0\nallowed 0\nhost 124\nmetadata 124\n"))
	for _, name := range []string{"egress agent refuses a name", "egress agent refuses an address", "egress agent resolver"} {
		if r := got[name]; !r.OK || !strings.HasPrefix(r.Detail, "skipped:") {
			t.Errorf("%s = %+v, want a skipped pass", name, r)
		}
	}
}

func TestRefusals(t *testing.T) {
	now := time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)
	snap := status.Snapshot{Egress: []status.Egress{
		{Class: "agent", Entries: []string{"github", "api.example.com"}, Packets: 12, Lookups: 1, Recent: []egress.Block{
			{Time: now, Kind: egress.Lookup, Dest: "data.evil.example"},
			{Time: now.Add(time.Second), Kind: egress.Packet, Dest: "203.0.113.9:22", Name: "api.example.com"},
		}},
		{Class: "coder", Entries: []string{"github"}, Problem: "nft is not installed; run sudo boxrunner install"},
	}}
	agent := Refusals(snap, "agent")
	wantDetail := "2 entries; 12 packets dropped and 1 lookup refused since the agent started; latest at " + now.Add(time.Second).Local().Format("15:04:05") + ": packet to 203.0.113.9:22 (api.example.com)"
	if want := (Result{"egress agent agent", true, wantDetail}); !reflect.DeepEqual(agent, want) {
		t.Errorf("Refusals(agent) = %+v, want %+v", agent, want)
	}
	if got, want := Refusals(snap, "coder"), (Result{"egress coder agent", false, "the agent does not serve the class: nft is not installed; run sudo boxrunner install"}); !reflect.DeepEqual(got, want) {
		t.Errorf("Refusals(coder) = %+v, want %+v", got, want)
	}
	if got := Refusals(snap, "other"); got.OK {
		t.Errorf("Refusals(other) = %+v, want a failure", got)
	}
}

func TestEgressRulesJudgesTheListTheAgentRuns(t *testing.T) {
	running, _ := egress.Parse([]string{"github", "api.example.com"})
	edited, _ := egress.Parse([]string{"github", "api.example.com", "registry.example.com"})
	table := egress.Ruleset{Classes: []egress.Class{{Net: egress.NetOf("agent"), Policy: running}}}.Script("")
	// What nft lists back of the script: the objects and the jumps it adds.
	listing := regexp.MustCompile(`(?m)^add (chain|set|counter) inet boxrunner (\S+).*$`).ReplaceAllString(table, "$1 $2 {")
	listing = regexp.MustCompile(`(?m)^add rule inet boxrunner (forward|input) `).ReplaceAllString(listing, "")
	snap := &status.Snapshot{Egress: []status.Egress{{Class: "agent", Entries: running.Entries}}}

	got := EgressRules(config.Class{Name: "agent", Egress: &edited}, snap, listing, firewall.Current)
	want := []Result{
		{"egress agent rules", true, "the table boxrunner and the host's rules for bridge " + egress.NetOf("agent").Bridge + " are in place"},
		{"egress agent config", false, "the config's egress list is not the one the agent runs; restart boxrunner to apply it"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EgressRules after an edit =\n%+v\nwant\n%+v", got, want)
	}
	if got := EgressRules(config.Class{Name: "agent", Egress: &running}, snap, listing, firewall.Current); len(got) != 1 || !got[0].OK {
		t.Errorf("EgressRules with the list the agent runs = %+v, want one passing result", got)
	}
	if got := EgressRules(config.Class{Name: "agent", Egress: &running}, snap, "", firewall.Current); got[0].OK {
		t.Errorf("EgressRules with no table = %+v, want it to fail", got)
	}
}
