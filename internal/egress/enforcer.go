package egress

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/firewall"
)

// ErrFull means a class has MaxLive addresses open already.
var ErrFull = errors.New("egress: the class has as many addresses open as it may")

// Enforcer keeps the nftables table and opens addresses in it.
type Enforcer struct {
	Run firewall.Run
	Now func() time.Time

	mu sync.Mutex
	// open is when each address that was opened ends, by class and address
	// with its ports.
	open map[string]map[string]time.Time
}

func (e *Enforcer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Apply makes the table what r says. When the table holds a set of another
// shape, such as one an older release made, it replaces the table, and the
// addresses open in it close.
func (e *Enforcer) Apply(ctx context.Context, r Ruleset) error {
	listing, _ := e.Run(ctx, "", "nft", "list", "table", "inet", Table)
	out, err := e.Run(ctx, r.Script(listing), "nft", "-f", "-")
	if err != nil && listing != "" {
		if out, derr := e.Run(ctx, "", "nft", "delete", "table", "inet", Table); derr != nil {
			return fmt.Errorf("nft delete table: %w: %s", derr, out)
		}
		out, err = e.Run(ctx, r.Script(""), "nft", "-f", "-")
	}
	if err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.open = nil
	return nil
}

// Open opens addresses for a class. An address that has most of its time left
// is not touched, so a job that resolves a name again and again costs nothing.
func (e *Enforcer) Open(ctx context.Context, n Net, grants []Grant) error {
	now := e.now()
	e.mu.Lock()
	if e.open == nil {
		e.open = map[string]map[string]time.Time{}
	}
	live := e.open[n.Class]
	if live == nil {
		live = map[string]time.Time{}
		e.open[n.Class] = live
	}
	for k, end := range live {
		if !now.Before(end) {
			delete(live, k)
		}
	}
	var todo []Grant
	var keys []string
	fresh := 0
	for _, g := range grants {
		key := g.Addr.String() + " " + g.Ports.String()
		if end, ok := live[key]; ok && end.Sub(now) > g.TTL/2 {
			continue
		} else if !ok {
			fresh++
		}
		todo = append(todo, g)
		keys = append(keys, key)
	}
	if len(live)+fresh > MaxLive {
		e.mu.Unlock()
		return ErrFull
	}
	e.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	if out, err := e.Run(ctx, ElementScript(n, todo), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("nft add element: %w: %s", err, strings.TrimSpace(out))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, g := range todo {
		e.open[n.Class][keys[i]] = now.Add(g.TTL)
	}
	return nil
}

var counterBlock = regexp.MustCompile(`(?s)counter (\S+) \{\s*packets (\d+) bytes (\d+)`)

// Blocked counts the packets the table dropped, by class id.
func (e *Enforcer) Blocked(ctx context.Context) (map[string]uint64, error) {
	out, err := e.Run(ctx, "", "nft", "list", "counters", "table", "inet", Table)
	if err != nil {
		return nil, fmt.Errorf("nft list counters: %w: %s", err, strings.TrimSpace(out))
	}
	counts := map[string]uint64{}
	for _, m := range counterBlock.FindAllStringSubmatch(out, -1) {
		if id, ok := strings.CutSuffix(strings.Trim(m[1], `"`), "_blocked"); ok {
			counts[id], _ = strconv.ParseUint(m[2], 10, 64)
		}
	}
	return counts, nil
}

// Present reports whether the table exists.
func Present(ctx context.Context, run firewall.Run) bool {
	_, err := run(ctx, "", "nft", "list", "table", "inet", Table)
	return err == nil
}

// Remove deletes the table with everything in it. A table that is not there
// counts as removed.
func Remove(ctx context.Context, run firewall.Run) error {
	if !Present(ctx, run) {
		return nil
	}
	if out, err := run(ctx, "", "nft", "delete", "table", "inet", Table); err != nil {
		return fmt.Errorf("nft delete table: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}
