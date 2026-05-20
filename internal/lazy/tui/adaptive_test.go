package tui

import (
	"testing"
	"time"
)

// TestAdaptiveDelay_BoundsAndBackoff: pin the rule so it doesn't
// drift silently.
//
// The shape of the function is the contract: if last refresh was
// fast we stay at the base cadence, otherwise we back off to
// max(base*2, elapsed*2) capped at 30s. The cap is what stops a
// wedged doltlite from melting the CPU; the elapsed*2 floor stops
// us from queuing back-to-back refreshes against a slow datasource.
func TestAdaptiveDelay_BoundsAndBackoff(t *testing.T) {
	const base = 2 * time.Second
	cases := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{"fresh DB / never fetched", 0, base},
		{"healthy read", 100 * time.Millisecond, base},
		{"right at half budget", base / 2, base},
		{"just over half budget", base/2 + time.Millisecond, base * 2},
		{"3s read (>base, <cap)", 3 * time.Second, 6 * time.Second},
		{"10s read (well over cap floor)", 10 * time.Second, 20 * time.Second},
		{"20s read (caps at 30s)", 20 * time.Second, maxAdaptiveDelay},
		{"1m read (caps at 30s)", time.Minute, maxAdaptiveDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adaptiveDelay(base, tc.elapsed)
			if got != tc.want {
				t.Fatalf("adaptiveDelay(%s, %s) = %s; want %s",
					base, tc.elapsed, got, tc.want)
			}
		})
	}
}

// TestAdaptiveDelay_BaseZeroDefaults: the loop seeds base from
// opts.Refresh, but opts.Refresh could be zero in early-init paths.
// We must not divide by zero — the default fallback (2s) is the same
// constant the gocui Run already applies.
func TestAdaptiveDelay_BaseZeroDefaults(t *testing.T) {
	if got := adaptiveDelay(0, 0); got != 2*time.Second {
		t.Fatalf("adaptiveDelay(0, 0) = %s; want 2s", got)
	}
}
