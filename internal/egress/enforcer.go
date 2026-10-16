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

// ErrFull means a class has MaxLive elements open already.
var ErrFull = errors.New("egress: the class has as many addresses and ports open as it may")

// Enforcer keeps the nftables table and opens addresses in it.
type Enforcer struct {
	Run firewall.Run
	Now func() time.Time

	// exec orders what changes the table, and mu guards open. An address is
	// open in the table before Open returns, so a lookup's answer never
	// outruns it.
	exec sync.Mutex
	mu   sync.Mutex
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

// Apply makes the table what r says. When the table has a set of another
// shape, such as one an older release made, it replaces the table in one
// transaction: the addresses open in it close, and no packet sees it empty.
func (e *Enforcer) Apply(ctx context.Context, r Ruleset) error {
	e.exec.Lock()
	defer e.exec.Unlock()
	listing := e.listing(ctx)
	out, err := e.Run(ctx, r.Script(listing), "nft", "-f", "-")
	if err != nil && listing != "" {
		out, err = e.Run(ctx, r.Replace(), "nft", "-f", "-")
	}
	if err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.open = nil
	return nil
}

// Verify applies r again when the table is not what the last Apply left, such
// as after someone flushed the ruleset. It reports whether the table was
// whole before.
func (e *Enforcer) Verify(ctx context.Context, r Ruleset) (bool, error) {
	if r.Intact(e.listing(ctx)) {
		return true, nil
	}
	return false, e.Apply(ctx, r)
}

// listing is what nft prints of the table, "" when there is none.
func (e *Enforcer) listing(ctx context.Context) string {
	out, err := e.Run(ctx, "", "nft", "list", "table", "inet", Table)
	if err != nil {
		return ""
	}
	return out
}

// Open opens addresses for a class and returns how long the grants stay open:
// the time the first of their elements ends. An element that has most of its
// time left is not touched, so a job that resolves a name again and again
// costs nothing, and the time returned may then be shorter than a grant's TTL.
// One transaction sets the others, whether or not they are open already.
func (e *Enforcer) Open(ctx context.Context, c Class, grants []Grant) (time.Duration, error) {
	e.exec.Lock()
	defer e.exec.Unlock()
	now := e.now()
	var todo []Element
	fresh := 0
	e.mu.Lock()
	if e.open == nil {
		e.open = map[string]map[string]time.Time{}
	}
	live := e.open[c.Net.Class]
	if live == nil {
		live = map[string]time.Time{}
		e.open[c.Net.Class] = live
	}
	for k, end := range live {
		if !now.Before(end) {
			delete(live, k)
		}
	}
	for _, g := range grants {
		for _, el := range g.Elements() {
			end, open := live[el.Key()]
			switch {
			case open && end.Sub(now) > el.TTL/2:
			case open:
				todo = append(todo, el)
			default:
				todo = append(todo, el)
				fresh++
			}
		}
	}
	full := len(live)+fresh > MaxLive
	e.mu.Unlock()
	if full {
		return 0, ErrFull
	}
	if len(todo) > 0 {
		if out, err := e.Run(ctx, ElementScript(c, todo), "nft", "-f", "-"); err != nil {
			return 0, fmt.Errorf("nft add element: %w: %s", err, strings.TrimSpace(out))
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, el := range todo {
		live[el.Key()] = now.Add(el.TTL)
	}
	var left time.Duration
	first := true
	for _, g := range grants {
		for _, el := range g.Elements() {
			if d := live[el.Key()].Sub(now); first || d < left {
				left, first = d, false
			}
		}
	}
	return left, nil
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
