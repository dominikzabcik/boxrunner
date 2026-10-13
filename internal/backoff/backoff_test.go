package backoff

import (
	"testing"
	"time"
)

func TestDelay(t *testing.T) {
	cases := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{0, 0, 500 * time.Millisecond},
		{0, 0.5, 750 * time.Millisecond},
		{1, 0, time.Second},
		{3, 0.999, 8*time.Second - 4*time.Millisecond},
		{8, 0, 128 * time.Second},
		{9, 0, 150 * time.Second},
		{9, 0.999, 5*time.Minute - 150*time.Millisecond},
		{64, 0, 150 * time.Second},
	}
	for _, c := range cases {
		if got := Delay(c.attempt, c.jitter); got != c.want {
			t.Errorf("Delay(%d, %v) = %v; want %v", c.attempt, c.jitter, got, c.want)
		}
	}
}
