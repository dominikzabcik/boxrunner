// Package seen records where jobs connect. The agent logs the start of every
// new connection that leaves a job network and notes it here against the
// repository whose runner made it, in a set that is capped in size.
package seen

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	// MaxRepos is how many repositories the book keeps destinations for. A new
	// one beyond that replaces the repository that connected least recently.
	MaxRepos = 64
	// MaxPerRepo is how many destinations the book keeps for a repository. A
	// new one beyond that replaces the destination used least recently.
	MaxPerRepo = 100
	// Keep is how long a destination nobody used stays in the book.
	Keep = 30 * 24 * time.Hour
)

// Destination is a place a repository's jobs connected to.
type Destination struct {
	// Addr is the IP address, Port the destination port (0 for a protocol
	// without ports) and Proto the protocol: tcp, udp, icmp, or its number.
	Addr  string `json:"addr"`
	Port  uint16 `json:"port"`
	Proto string `json:"proto"`
	// Name is the name the class's DNS forwarder last opened Addr for, when
	// it did; only a class with an egress allowlist has such a forwarder.
	Name string `json:"name,omitempty"`
	// First and Last are when the agent saw the first and the latest new
	// connection to it, and Count how many it saw. The kernel logs at most 100
	// new connections a second, so Count is at least that many.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	Count uint64    `json:"count"`
}

func (d Destination) key() string { return d.Addr + " " + strconv.Itoa(int(d.Port)) + " " + d.Proto }

// String is the destination as people read it, such as "203.0.113.9:443
// (api.example.com)".
func (d Destination) String() string {
	text := d.Addr
	if a, err := netip.ParseAddr(d.Addr); err == nil && d.Port != 0 {
		text = netip.AddrPortFrom(a, d.Port).String()
	}
	if d.Proto != "tcp" {
		text += " " + d.Proto
	}
	if d.Name != "" {
		text += " (" + d.Name + ")"
	}
	return text
}

// Repo is the destinations of one repository's jobs, newest first.
type Repo struct {
	Repo         string        `json:"repo"`
	Destinations []Destination `json:"destinations"`
}

// ProtoName is the name of an IP protocol number.
func ProtoName(proto uint8) string {
	switch proto {
	case 1:
		return "icmp"
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 58:
		return "icmpv6"
	default:
		return strconv.Itoa(int(proto))
	}
}

// Book is the destinations of every repository, bounded by MaxRepos and
// MaxPerRepo. It is safe for concurrent use.
type Book struct {
	Now func() time.Time

	mu    sync.Mutex
	repos map[string]map[string]*Destination
	// changed is set by Note and cleared by Save.
	changed bool
}

func (b *Book) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Note records a new connection of a repository's job to addr. It returns the
// destination as the book holds it now, and whether the repository had not
// connected there before.
func (b *Book) Note(repo string, addr netip.Addr, port uint16, proto uint8, name string) (Destination, bool) {
	d := Destination{Addr: addr.Unmap().String(), Port: port, Proto: ProtoName(proto)}
	if proto != 6 && proto != 17 {
		d.Port = 0
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.changed = true
	set := b.repos[repo]
	if set == nil {
		if b.repos == nil {
			b.repos = map[string]map[string]*Destination{}
		}
		if len(b.repos) >= MaxRepos {
			delete(b.repos, quietest(b.repos))
		}
		set = map[string]*Destination{}
		b.repos[repo] = set
	}
	if old, ok := set[d.key()]; ok {
		old.Last, old.Count = now, old.Count+1
		if name != "" {
			old.Name = name
		}
		return *old, false
	}
	if len(set) >= MaxPerRepo {
		delete(set, idlest(set))
	}
	d.Name, d.First, d.Last, d.Count = name, now, now, 1
	set[d.key()] = &d
	return d, true
}

// idlest is the key of the destination used least recently.
func idlest(set map[string]*Destination) string {
	var key string
	var at time.Time
	for k, d := range set {
		if key == "" || d.Last.Before(at) || (d.Last.Equal(at) && k < key) {
			key, at = k, d.Last
		}
	}
	return key
}

// quietest is the repository that connected least recently.
func quietest(repos map[string]map[string]*Destination) string {
	var name string
	var at time.Time
	for r, set := range repos {
		last := latest(set)
		if name == "" || last.Before(at) || (last.Equal(at) && r < name) {
			name, at = r, last
		}
	}
	return name
}

func latest(set map[string]*Destination) time.Time {
	var last time.Time
	for _, d := range set {
		if d.Last.After(last) {
			last = d.Last
		}
	}
	return last
}

// Repos lists the repositories by name, each with at most perRepo of its
// newest destinations (all when perRepo is 0 or less).
func (b *Book) Repos(perRepo int) []Repo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Repo, 0, len(b.repos))
	for name, set := range b.repos {
		r := Repo{Repo: name}
		for _, d := range set {
			r.Destinations = append(r.Destinations, *d)
		}
		slices.SortFunc(r.Destinations, func(x, y Destination) int {
			return cmp.Or(y.First.Compare(x.First), cmp.Compare(x.key(), y.key()))
		})
		if perRepo > 0 && len(r.Destinations) > perRepo {
			r.Destinations = r.Destinations[:perRepo]
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(x, y Repo) int { return cmp.Compare(x.Repo, y.Repo) })
	return out
}

// file is what Save writes.
type file struct {
	Repos []Repo `json:"repos"`
}

// Save writes the book to path when it changed since the last Save.
func (b *Book) Save(path string) error {
	b.mu.Lock()
	changed := b.changed
	b.changed = false
	b.mu.Unlock()
	if !changed {
		return nil
	}
	body, err := json.Marshal(file{Repos: b.Repos(0)})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, body, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		b.mu.Lock()
		b.changed = true
		b.mu.Unlock()
	}
	return err
}

// Load reads what Save wrote. It leaves out destinations unused for Keep and
// whatever exceeds the caps, and a missing file is an empty book.
func (b *Book) Load(path string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(body, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cutoff := b.now().Add(-Keep)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.repos = map[string]map[string]*Destination{}
	for _, r := range f.Repos {
		set := map[string]*Destination{}
		for _, d := range r.Destinations {
			if _, err := netip.ParseAddr(d.Addr); err != nil || d.Last.Before(cutoff) {
				continue
			}
			set[d.key()] = &d
			if len(set) > MaxPerRepo {
				delete(set, idlest(set))
			}
		}
		if len(set) == 0 {
			continue
		}
		b.repos[r.Repo] = set
		if len(b.repos) > MaxRepos {
			delete(b.repos, quietest(b.repos))
		}
	}
	return nil
}
