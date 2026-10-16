package egress

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestParseReadsEveryKindOfEntry(t *testing.T) {
	p, err := Parse([]string{
		"github", "api.example.com", "*.example.org:8443", "registry.example.net:*",
		"10.20.0.0/16:5432", "203.0.113.7",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		want  Ports
		exact bool
		ok    bool
	}{
		{"api.example.com", Ports{List: []uint16{80, 443}}, true, true},
		{"API.Example.COM.", Ports{List: []uint16{80, 443}}, true, true},
		{"www.example.org", Ports{List: []uint16{8443}}, false, true},
		{"a.b.example.org", Ports{List: []uint16{8443}}, false, true},
		{"example.org", Ports{}, false, false},
		{"notexample.org", Ports{}, false, false},
		{"registry.example.net", Ports{Any: true}, true, true},
		{"example.com", Ports{}, false, false},
		{"github.com", Ports{List: []uint16{443}}, true, true},
		{"broker.actions.githubusercontent.com", Ports{List: []uint16{443}}, false, true},
		{"productionresultssa19.blob.core.windows.net", Ports{List: []uint16{443}}, true, true},
		{"attacker.blob.core.windows.net", Ports{}, false, false},
	} {
		ports, exact, ok := p.Match(c.name)
		if !reflect.DeepEqual(ports, c.want) || exact != c.exact || ok != c.ok {
			t.Errorf("Match(%q) = %v, %v, %v; want %v, %v, %v", c.name, ports, exact, ok, c.want, c.exact, c.ok)
		}
	}
	wantNets := []NetRule{
		{Prefix: netip.MustParsePrefix("10.20.0.0/16"), Ports: Ports{List: []uint16{5432}}},
		{Prefix: netip.MustParsePrefix("203.0.113.7/32"), Ports: DefaultPorts()},
	}
	if !reflect.DeepEqual(p.Nets, wantNets) {
		t.Errorf("Nets = %+v, want %+v", p.Nets, wantNets)
	}
	wantEntries := []string{"github", "api.example.com", "*.example.org:8443", "registry.example.net:*", "10.20.0.0/16:5432", "203.0.113.7"}
	if !reflect.DeepEqual(p.Entries, wantEntries) {
		t.Errorf("Entries = %q, want %q", p.Entries, wantEntries)
	}
}

func TestParseUnionsThePortsOfOverlappingEntries(t *testing.T) {
	p, err := Parse([]string{"github", "*.example.com:8080", "api.example.com:9000"})
	if err != nil {
		t.Fatal(err)
	}
	ports, exact, ok := p.Match("api.example.com")
	if want := (Ports{List: []uint16{8080, 9000}}); !reflect.DeepEqual(ports, want) || !exact || !ok {
		t.Errorf("Match(api.example.com) = %v, %v, %v; want %v, true, true", ports, exact, ok, want)
	}
}

func TestParseRefusesWhatItCannotAllow(t *testing.T) {
	for _, c := range []struct{ entry, want string }{
		{"db", "fully qualified host name"},
		{"*.com", "wildcard needs a domain"},
		{"*", "fully qualified host name"},
		{"a.*.example.com", `"*" is not a host name`},
		{"exa mple.com", "is not a host name"},
		{"example.com:0", `port "0"`},
		{"example.com:99999", `port "99999"`},
		{"example.com:http", `port "http"`},
		{"example.com:", `port ""`},
		{"[2001:db8::1", "missing ]"},
		{"[2001:db8::1]443", "expected :port"},
		{"10.20.0.1/16", "host bits set; did you mean 10.20.0.0/16"},
		{"10.20.0.0/33", "not an address range"},
		{"0.0.0.0/0", "every address"},
		{"169.254.169.254", "never reachable"},
		{"169.254.0.0/16:80", "never reachable"},
		{"127.0.0.1:8080", "never reachable"},
		{"168.63.129.16", "never reachable"},
		{"224.0.0.251:5353", "never reachable"},
		{"2001:db8::1", "IPv6 is not supported"},
		{"[2001:db8::1]:443", "IPv6 is not supported"},
		{"2001:db8::/32", "IPv6 is not supported"},
		{"::/0", "IPv6 is not supported"},
		{"0.0.0.0/1", "never reachable"},
	} {
		_, err := Parse([]string{"github", c.entry})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(github, %q) error = %v, want one that says %q", c.entry, err, c.want)
		}
	}
}

func TestParseNeedsTheHostsARunnerReaches(t *testing.T) {
	_, err := Parse([]string{"registry.example.net"})
	if err == nil {
		t.Fatal("Parse without GitHub's hosts succeeded")
	}
	for _, host := range RunnerHosts {
		if !strings.Contains(err.Error(), host+":443 is not allowed") {
			t.Errorf("error does not name %s:\n%v", host, err)
		}
	}
	for _, entries := range [][]string{
		{"github.com", "api.github.com", "*.actions.githubusercontent.com"},
		{"*.github.com", "github.com", "*.githubusercontent.com"},
	} {
		if _, err := Parse(entries); err != nil {
			t.Errorf("Parse(%q) = %v", entries, err)
		}
	}
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), `start the list with "github"`) {
		t.Errorf("Parse(nil) = %v, want a hint to start with github", err)
	}
}

func TestGitHubPresetListsTheHostsGitHubDocuments(t *testing.T) {
	p, err := Parse([]string{"github"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{
		"github.com", "api.github.com", "codeload.github.com", "pipelinesghubeus1.actions.githubusercontent.com",
		"broker.actions.githubusercontent.com", "results-receiver.actions.githubusercontent.com",
		"productionresultssa0.blob.core.windows.net", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
		"ghcr.io", "pkg-containers.githubusercontent.com", "npm.pkg.github.com", "github-cloud.githubusercontent.com",
	} {
		if !p.AllowsName(host, 443) {
			t.Errorf("the github preset does not allow %s:443", host)
		}
	}
	for _, host := range []string{"example.com", "raw.githubusercontent.com", "evil.blob.core.windows.net", "productionresultssa20.blob.core.windows.net", "githubusercontent.com"} {
		if p.AllowsName(host, 443) {
			t.Errorf("the github preset allows %s:443", host)
		}
	}
	if p.AllowsName("github.com", 22) || p.AllowsName("github.com", 80) {
		t.Error("the github preset allows a port other than 443")
	}
}

func TestPrivateAndNeverRanges(t *testing.T) {
	for _, c := range []struct {
		addr           string
		private, never bool
	}{
		{"10.20.0.5", true, false},
		{"172.31.255.255", true, false},
		{"172.32.0.1", false, false},
		{"192.168.1.1", true, false},
		{"100.64.0.1", true, false},
		{"8.8.8.8", false, false},
		{"169.254.169.254", false, true},
		{"127.0.0.1", false, true},
		{"::ffff:169.254.169.254", false, true},
		{"168.63.129.16", false, true},
	} {
		a := netip.MustParseAddr(c.addr)
		if got := IsPrivate(a); got != c.private {
			t.Errorf("IsPrivate(%s) = %v", c.addr, got)
		}
		if got := IsNever(a); got != c.never {
			t.Errorf("IsNever(%s) = %v", c.addr, got)
		}
	}
}

func TestPortsString(t *testing.T) {
	for _, c := range []struct {
		p    Ports
		want string
	}{
		{DefaultPorts(), "80, 443"},
		{Ports{Any: true}, "*"},
		{onePort(5432), "5432"},
		{onePort(443).Union(onePort(80)).Union(onePort(443)), "80, 443"},
		{onePort(443).Union(Ports{Any: true}), "*"},
	} {
		if got := c.p.String(); got != c.want {
			t.Errorf("%#v.String() = %q, want %q", c.p, got, c.want)
		}
	}
}
