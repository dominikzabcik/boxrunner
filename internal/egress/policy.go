// Package egress is a class's outbound allowlist: the entries a class lists,
// what they allow, the network rules that enforce them and a ledger of what
// they refused. docs/decisions.md#an-egress-allowlist-per-class says how the
// parts fit.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Ports are the TCP ports an entry allows.
type Ports struct {
	// Any allows every port, as the entry ":*" does.
	Any bool
	// List holds the ports in ascending order; it is empty when Any is set.
	List []uint16
}

// DefaultPorts are the ports of an entry that names none: the web's.
func DefaultPorts() Ports { return Ports{List: []uint16{80, 443}} }

func onePort(p uint16) Ports { return Ports{List: []uint16{p}} }

// Contains reports whether the port is allowed.
func (p Ports) Contains(port uint16) bool {
	return p.Any || slices.Contains(p.List, port)
}

// Union is the ports either allows.
func (p Ports) Union(q Ports) Ports {
	if p.Any || q.Any {
		return Ports{Any: true}
	}
	list := slices.Concat(p.List, q.List)
	slices.Sort(list)
	return Ports{List: slices.Compact(list)}
}

// String writes the ports as an entry does: "443", "80, 443" or "*".
func (p Ports) String() string {
	if p.Any {
		return "*"
	}
	parts := make([]string, len(p.List))
	for i, port := range p.List {
		parts[i] = strconv.Itoa(int(port))
	}
	return strings.Join(parts, ", ")
}

// NameRule allows the addresses a hostname resolves to.
type NameRule struct {
	// Name is the host, or for a wildcard the suffix after "*.".
	Name     string
	Wildcard bool
	Ports    Ports
}

func (r NameRule) matches(name string) bool {
	if r.Wildcard {
		return strings.HasSuffix(name, "."+r.Name)
	}
	return name == r.Name
}

// NetRule allows the addresses of a network.
type NetRule struct {
	Prefix netip.Prefix
	Ports  Ports
}

// Policy is what a class's entries allow. Everything else is refused.
type Policy struct {
	Names []NameRule
	Nets  []NetRule
	// Entries are the entries as written, after the presets expand.
	Entries []string
}

// Match says which ports a name's addresses may be reached on. Exact is set
// when a rule names the host itself rather than a wildcard.
func (p Policy) Match(name string) (ports Ports, exact, ok bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, r := range p.Names {
		if !r.matches(name) {
			continue
		}
		ports = ports.Union(r.Ports)
		exact = exact || !r.Wildcard
		ok = true
	}
	return ports, exact, ok
}

// AllowsName reports whether the host may be reached on the port.
func (p Policy) AllowsName(name string, port uint16) bool {
	ports, _, ok := p.Match(name)
	return ok && ports.Contains(port)
}

// AllowsAddr reports whether an address of a network entry may be reached on
// the port. It does not know the addresses names resolve to.
func (p Policy) AllowsAddr(addr netip.Addr, port uint16) bool {
	return slices.ContainsFunc(p.Nets, func(r NetRule) bool {
		return r.Prefix.Contains(addr) && r.Ports.Contains(port)
	})
}

// Presets are entries a class can name in place of their hosts.
var Presets = map[string][]string{
	// What GitHub lists for self-hosted runners under "Accessible domains by
	// function": https://docs.github.com/en/actions/reference/runners/self-hosted-runners#accessible-domains-by-function
	// Its "*.blob.core.windows.net" is replaced by the twenty storage accounts
	// of the results service that GitHub's meta API lists under
	// domains.actions (https://docs.github.com/en/rest/meta/meta), because
	// anyone can open a storage account of their own on that wildcard.
	"github": slices.Concat([]string{
		// Essential operations, including the broker and scale set services.
		"github.com", "api.github.com", "*.actions.githubusercontent.com",
		// Downloading actions.
		"codeload.github.com",
		// Job summaries, logs, artifacts and caches.
		"results-receiver.actions.githubusercontent.com",
		// Runner updates and release assets.
		"objects.githubusercontent.com", "objects-origin.githubusercontent.com", "github-releases.githubusercontent.com",
		"github-registry-files.githubusercontent.com", "release-assets.githubusercontent.com",
		// GitHub Packages and the container registry.
		"*.pkg.github.com", "pkg-containers.githubusercontent.com", "ghcr.io",
		// Git LFS.
		"github-cloud.githubusercontent.com", "github-cloud.s3.amazonaws.com",
	}, resultsStorage()),
}

func resultsStorage() []string {
	var hosts []string
	for i := range 20 {
		hosts = append(hosts, "productionresultssa"+strconv.Itoa(i)+".blob.core.windows.net")
	}
	return hosts
}

// RunnerHosts are what a runner must reach to take a job from GitHub. A
// class's policy that leaves one out could never run a job.
var RunnerHosts = []string{"github.com", "api.github.com", "broker.actions.githubusercontent.com"}

// Never are the destinations no entry can allow: this machine, multicast, the
// link-local range and the cloud metadata services.
var Never = parsePrefixes(
	"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
	"168.63.129.16/32", "100.100.100.200/32", "192.0.0.192/32",
	"::/128", "::1/128", "fe80::/10", "ff00::/8", "fd00:ec2::254/128",
)

// Private are the ranges that are not the internet's: they are reachable only
// by an entry that names them or an exact host that resolves to them.
var Private = parsePrefixes("10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7")

func parsePrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// IsPrivate reports whether an address is in a private range.
func IsPrivate(a netip.Addr) bool {
	return slices.ContainsFunc(Private, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) })
}

// IsNever reports whether no entry can allow an address.
func IsNever(a netip.Addr) bool {
	return slices.ContainsFunc(Never, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) })
}

// Parse reads a class's entries. It reports every entry it cannot read.
func Parse(entries []string) (Policy, error) {
	var p Policy
	var errs []error
	if len(entries) == 0 {
		return p, errors.New(`empty: a class with no destinations cannot reach GitHub; start the list with "github", or leave egress out to open the class`)
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if hosts, ok := Presets[strings.ToLower(entry)]; ok {
			for _, host := range hosts {
				if err := p.add(host + ":443"); err != nil {
					errs = append(errs, fmt.Errorf("preset %q: %w", entry, err))
				}
			}
			p.Entries = append(p.Entries, strings.ToLower(entry))
		} else if err := p.add(entry); err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", entry, err))
		} else {
			p.Entries = append(p.Entries, entry)
		}
	}
	for _, host := range RunnerHosts {
		if !p.AllowsName(host, 443) {
			errs = append(errs, fmt.Errorf(`%s:443 is not allowed, so its runners could not take a job; start the list with "github"`, host))
		}
	}
	return p, errors.Join(errs...)
}

// add reads one entry that is not a preset.
func (p *Policy) add(entry string) error {
	target, ports, err := splitPort(entry)
	if err != nil {
		return err
	}
	prefix, isNet, err := parseNet(target)
	if err != nil {
		return err
	}
	if isNet {
		if prefix.Bits() == 0 {
			return errors.New("every address; leave egress out to open the class")
		}
		for _, n := range Never {
			if prefix.Overlaps(n) {
				return fmt.Errorf("%s is never reachable from a job: it is this machine, multicast, link-local or a cloud metadata service", n)
			}
		}
		p.Nets = append(p.Nets, NetRule{Prefix: prefix, Ports: ports})
		return nil
	}
	rule, err := parseName(target)
	if err != nil {
		return err
	}
	rule.Ports = ports
	p.Names = append(p.Names, rule)
	return nil
}

// splitPort separates an optional port from an entry. IPv6 addresses with a
// port are written in brackets, as in "[2001:db8::]:443".
func splitPort(entry string) (target string, ports Ports, err error) {
	var port string
	switch {
	case strings.HasPrefix(entry, "["):
		end := strings.IndexByte(entry, ']')
		if end < 0 {
			return "", Ports{}, errors.New("missing ]")
		}
		target = entry[1:end]
		switch rest := entry[end+1:]; {
		case rest == "":
			return target, DefaultPorts(), nil
		case rest[0] == ':':
			port = rest[1:]
		default:
			return "", Ports{}, errors.New("expected :port after ]")
		}
	case strings.Count(entry, ":") > 1:
		return entry, DefaultPorts(), nil
	default:
		var found bool
		if target, port, found = strings.Cut(entry, ":"); !found {
			return target, DefaultPorts(), nil
		}
	}
	if port == "*" {
		return target, Ports{Any: true}, nil
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", Ports{}, fmt.Errorf("port %q is not 1 to 65535 or *", port)
	}
	return target, onePort(uint16(n)), nil
}

// parseNet reads an address or a network; ok is false for anything that is
// not one, which may be a hostname.
func parseNet(s string) (netip.Prefix, bool, error) {
	if strings.Contains(s, "/") {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, false, fmt.Errorf("%q is not an address range such as 10.20.0.0/16", s)
		}
		if prefix != prefix.Masked() {
			return netip.Prefix{}, false, fmt.Errorf("%s has host bits set; did you mean %s?", s, prefix.Masked())
		}
		return prefix.Masked(), true, nil
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), true, nil
	}
	return netip.Prefix{}, false, nil
}

// parseName reads a host such as "registry.example" or "*.example".
func parseName(s string) (NameRule, error) {
	name := strings.ToLower(strings.TrimSuffix(s, "."))
	rule := NameRule{Name: name}
	if suffix, ok := strings.CutPrefix(name, "*."); ok {
		rule = NameRule{Name: suffix, Wildcard: true}
	}
	labels := strings.Split(rule.Name, ".")
	switch {
	case rule.Wildcard && len(labels) < 2:
		return NameRule{}, errors.New("a wildcard needs a domain under it, such as *.example.com")
	case len(labels) < 2:
		return NameRule{}, errors.New("not an address, a range, a preset or a fully qualified host name")
	}
	for _, label := range labels {
		if !validLabel(label) {
			return NameRule{}, fmt.Errorf("%q is not a host name; use letters, digits and dashes, and * only as the first label", label)
		}
	}
	return rule, nil
}

func validLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range l {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Grant opens an address for a class's jobs on the ports an entry allows,
// for as long as the TTL.
type Grant struct {
	Addr  netip.Addr
	Ports Ports
	TTL   time.Duration
}
