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

// Open opens addresses for a class. An address that has most of its time left
// is not touched, so a job that resolves a name again and again costs nothing.
func (e *Enforcer) Open(ctx context.Context, c Class, grants []Grant) error {
	e.exec.Lock()
	defer e.exec.Unlock()
	now := e.now()
	type change struct {
		key   string
		grant Grant
		renew bool
	}
	var todo []change
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
		if !g.Addr.Is4() {
			continue
		}
		key := g.Addr.String() + " " + g.Ports.String()
		end, open := live[key]
		switch {
		case open && end.Sub(now) > g.TTL/2:
		case open:
			todo = append(todo, change{key, g, true})
		default:
			todo = append(todo, change{key, g, false})
			fresh++
		}
	}
	full := len(live)+fresh > MaxLive
	e.mu.Unlock()
	if full {
		return ErrFull
	}
	if len(todo) == 0 {
		return nil
	}
	var renew, all []Grant
	for _, ch := range todo {
		all = append(all, ch.grant)
		if ch.renew {
			renew = append(renew, ch.grant)
		}
	}
	if len(renew) > 0 {
		// An address that expired meanwhile is not there to remove, which does not matter.
		e.Run(ctx, ElementScript(c, renew, true), "nft", "-f", "-")
	}
	if out, err := e.Run(ctx, ElementScript(c, all, false), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("nft add element: %w: %s", err, strings.TrimSpace(out))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range todo {
		live[ch.key] = now.Add(ch.grant.TTL)
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
