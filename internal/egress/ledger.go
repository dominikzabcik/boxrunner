package egress

import (
	"slices"
	"sync"
	"time"
)

// Kind says what was refused.
type Kind string

const (
	// Packet is a packet the table dropped.
	Packet Kind = "packet"
	// Lookup is a name the forwarder refused.
	Lookup Kind = "lookup"
)

// Block is one thing a class's jobs were refused, as the box's owner reads it.
type Block struct {
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	// Runner and Repo name the job, when the agent could tell which one asked.
	Runner string `json:"runner,omitempty"`
	Repo   string `json:"repo,omitempty"`
	// Dest is the address and port of a dropped packet, or the name of a
	// refused lookup. Name is the allowed name that last resolved to Dest, when
	// there is one.
	Dest string `json:"dest"`
	Name string `json:"name,omitempty"`
}

// String says what was refused, such as "packet to 203.0.113.9:443
// (api.example.com)" or "lookup of data.evil.example".
func (b Block) String() string {
	text := "packet to " + b.Dest
	if b.Kind == Lookup {
		text = "lookup of " + b.Dest
	}
	if b.Name != "" {
		text += " (" + b.Name + ")"
	}
	return text
}

const (
	// recent is how many blocks a class's ledger keeps.
	recent = 20
	// every is how often the same refusal is recorded, and perMinute how many
	// distinct ones a class records: a job that tries a host in a loop, or a
	// scan, fills neither the log nor the ring.
	every     = time.Minute
	perMinute = 30
)

// Ledger counts what each class's jobs were refused and keeps the latest.
type Ledger struct {
	Now func() time.Time

	mu      sync.Mutex
	classes map[string]*classBlocks
}

type classBlocks struct {
	packets, lookups uint64
	ring             []Block
	last             map[string]time.Time
	windowStart      time.Time
	// inWindow counts the refusals admitted in the minute that began at
	// windowStart, by kind, so one kind cannot use up another's share.
	inWindow map[Kind]int
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Ledger) class(name string) *classBlocks {
	if l.classes == nil {
		l.classes = map[string]*classBlocks{}
	}
	c := l.classes[name]
	if c == nil {
		c = &classBlocks{last: map[string]time.Time{}, inWindow: map[Kind]int{}}
		l.classes[name] = c
	}
	return c
}

// CountLookup counts a refused lookup.
func (l *Ledger) CountLookup(class string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.class(class).lookups++
}

// SetPackets sets how many packets the table has dropped for the class since
// the agent started.
func (l *Ledger) SetPackets(class string, total uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.class(class).packets = total
}

// Admit reports whether a refusal is worth recording and logging: false when
// the class refused the same thing within the last minute, or has recorded as
// many refusals of that kind as it records a minute. Looking up who asked is worth doing
// only for the ones it admits.
func (l *Ledger) Admit(class string, kind Kind, dest string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.class(class)
	now := l.now()
	key := string(kind) + " " + dest
	if at, ok := c.last[key]; ok && now.Sub(at) < every {
		return false
	}
	if now.Sub(c.windowStart) >= time.Minute {
		c.windowStart = now
		clear(c.inWindow)
	}
	if c.inWindow[kind] >= perMinute {
		return false
	}
	c.inWindow[kind]++
	if len(c.last) > 4*perMinute {
		for k, at := range c.last {
			if now.Sub(at) >= every {
				delete(c.last, k)
			}
		}
	}
	c.last[key] = now
	return true
}

// Keep records a refusal that Admit let through.
func (l *Ledger) Keep(class string, b Block) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.class(class)
	b.Time = l.now()
	c.ring = append(c.ring, b)
	if len(c.ring) > recent {
		c.ring = slices.Delete(c.ring, 0, len(c.ring)-recent)
	}
}

// Counts are what a class was refused since the agent started.
type Counts struct {
	Packets, Lookups uint64
	// Recent are the latest recorded blocks, oldest first.
	Recent []Block
}

// Counts reads a class's ledger.
func (l *Ledger) Counts(class string) Counts {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.class(class)
	return Counts{Packets: c.packets, Lookups: c.lookups, Recent: slices.Clone(c.ring)}
}
