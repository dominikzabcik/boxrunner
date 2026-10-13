// Package backoff spaces out retries, so a loop on the box can never spend the
// Worker's daily request allowance or hammer GitHub.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	First = time.Second
	Cap   = 5 * time.Minute
)

// Delay is the wait before retry number attempt (0 for the first retry):
// First doubled each attempt up to Cap, then scaled into [d/2, d) by jitter,
// a number in [0, 1).
func Delay(attempt int, jitter float64) time.Duration {
	d := Cap
	if attempt < 20 {
		d = min(First<<attempt, Cap)
	}
	return d/2 + time.Duration(jitter*float64(d/2))
}

// Policy waits between attempts. The zero value waits for real with random
// jitter; tests replace Sleep and Jitter.
type Policy struct {
	Sleep  func(context.Context, time.Duration) error
	Jitter func() float64
}

// Wait sleeps before retry number attempt, or returns early with ctx's error.
func (p Policy) Wait(ctx context.Context, attempt int) error {
	jitter := rand.Float64
	if p.Jitter != nil {
		jitter = p.Jitter
	}
	sleep := Sleep
	if p.Sleep != nil {
		sleep = p.Sleep
	}
	return sleep(ctx, Delay(attempt, jitter()))
}

func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
