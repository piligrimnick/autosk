package tui

import "testing"

// TestScrollOffOrigin_CenteringFallback pins the new behaviour added
// to support "wheel scrolls the viewport without moving the cursor":
// when the user wheel-scrolls the selection out of view and then
// presses j/k, the next render must bring the cursor back into the
// viewport. scrollOffDelta alone doesn't do this (its `before must be
// visible` guard makes it return 0 when the cursor was off-screen),
// so scrollOffOrigin layers a centre-on-cursor pass on top.
func TestScrollOffOrigin_CenteringFallback(t *testing.T) {
	// vp = [0..10), cursor at abs 30 (off-screen, e.g. after wheel
	// scrolled the viewport away from it). j moves cursor 30 → 31
	// (still off-screen). scrollOffDelta returns 0 (before not in vp),
	// scrollOffOrigin centres: oy = 31 - 10/2 = 26.
	if got := scrollOffOrigin(30, 31, 0, 10, 2); got != 26 {
		t.Fatalf("centring fallback: got oy=%d want 26", got)
	}
	// k moves 30 → 29 — same centring: oy = 29 - 5 = 24.
	if got := scrollOffOrigin(30, 29, 0, 10, 2); got != 24 {
		t.Fatalf("centring fallback (up): got oy=%d want 24", got)
	}
	// Normal in-viewport move still uses the margin algorithm:
	// oy=0, vpH=10, before=7 (visible), after=8 enters bottom margin.
	// scrollOffDelta=+1, after the +1 cursor is at 8 still inside
	// [1..11) — no further centring.
	if got := scrollOffOrigin(7, 8, 0, 10, 2); got != 1 {
		t.Fatalf("margin path: got oy=%d want 1", got)
	}
	// Negative-clamping: cursor near top, vp already at 0 — oy stays 0.
	if got := scrollOffOrigin(0, 0, 0, 10, 2); got != 0 {
		t.Fatalf("no-op at top: got oy=%d want 0", got)
	}
}

// TestScrollOffDelta_NoOpWhenInsideViewport pins the baseline: a
// move that lands well within the viewport (neither edge crossed)
// doesn't scroll. Without this guard the algorithm would scroll on
// every keystroke, which is the opposite of what the user wants.
func TestScrollOffDelta_NoOpWhenInsideViewport(t *testing.T) {
	// vp = [0..10), cursor 4 → 5, margin 2 → still inside [2..7]
	if got := scrollOffDelta(4, 5, 0, 10, 2); got != 0 {
		t.Fatalf("center→center: got delta=%d want 0", got)
	}
	// cursor 5 → 4 same idea
	if got := scrollOffDelta(5, 4, 0, 10, 2); got != 0 {
		t.Fatalf("center↑: got delta=%d want 0", got)
	}
}

// TestScrollOffDelta_DownEntersBottomMargin pins lazygit's "next
// keystroke after entering the bottom 2-row margin advances the
// origin by exactly one row". The user's reported regression was
// that this was returning 0 — viewport stayed stuck while the cursor
// kept moving off-screen.
func TestScrollOffDelta_DownEntersBottomMargin(t *testing.T) {
	// vp height 10, oy=0, margin 2.
	// marginStart = 0 + 10 - 2 - 1 = 7. After=8 crosses it.
	// delta = 8 - 7 = 1.
	if got := scrollOffDelta(7, 8, 0, 10, 2); got != 1 {
		t.Fatalf("entering bottom margin: got delta=%d want 1", got)
	}
	// After=9 (last visible) → delta=2 (push origin down 2 so cursor
	// sits at marginStart=9).
	if got := scrollOffDelta(7, 9, 0, 10, 2); got != 2 {
		t.Fatalf("two-step jump into bottom margin: got delta=%d want 2", got)
	}
}

// TestScrollOffDelta_UpEntersTopMargin: symmetric to the down case.
func TestScrollOffDelta_UpEntersTopMargin(t *testing.T) {
	// vp=[20..30), oy=20, margin 2. marginEnd = 20+2 = 22.
	// before=22 (visible), after=21 (inside margin) → delta = 21-22 = -1.
	if got := scrollOffDelta(22, 21, 20, 10, 2); got != -1 {
		t.Fatalf("entering top margin: got delta=%d want -1", got)
	}
	// after=20 (top edge) → delta = -2.
	if got := scrollOffDelta(22, 20, 20, 10, 2); got != -2 {
		t.Fatalf("two-step jump into top margin: got delta=%d want -2", got)
	}
}

// TestScrollOffDelta_BeforeOutOfView: when the keystroke "before"
// position wasn't visible (mouse-scroll moved the selection off
// the viewport without firing the scroll-off path), we deliberately
// do NOT auto-scroll. The applyRowHighlight fallback snaps the
// cursor back into view in that case.
//
// This guard prevents a feedback loop: scroll wheel hides the
// cursor, j keypress would otherwise yank the viewport right back
// where it was, defeating the scroll wheel's intent.
func TestScrollOffDelta_BeforeOutOfView(t *testing.T) {
	// vp=[100..110), but cursor before=5 (off-screen above).
	if got := scrollOffDelta(5, 6, 100, 10, 2); got != 0 {
		t.Fatalf("before above viewport: got delta=%d want 0", got)
	}
	// cursor before=200 (off-screen below).
	if got := scrollOffDelta(200, 199, 100, 10, 2); got != 0 {
		t.Fatalf("before below viewport: got delta=%d want 0", got)
	}
}

// TestScrollOffDelta_CapsAtHalfHeight: on a 3-row viewport, a
// scrollOffMargin of 2 would make the cursor stuck in place
// (margin+margin > height). Lazygit caps to half height so motion
// still happens. Pin the formulas:
//
//	down: cap = (vpH-1)/2
//	up:   cap = (vpH+1)/2
//
// so even-height viewports get an asymmetric +1 bias toward the
// top (matches lazygit's comment in its scroll_off_margin.go).
func TestScrollOffDelta_CapsAtHalfHeight(t *testing.T) {
	// vpH=3, margin=2.
	// down: cap = (3-1)/2 = 1. marginStart = 0+3-1-1 = 1.
	// before=1 (visible), after=2 → delta = 2-1 = 1.
	if got := scrollOffDelta(1, 2, 0, 3, 2); got != 1 {
		t.Fatalf("small vp down: got delta=%d want 1", got)
	}
	// up: cap = (3+1)/2 = 2 → but vp only has 3 rows, full coverage.
	// before=1 (visible), after=0 → marginEnd = 0+2 = 2; after=0 < 2 → delta = -2.
	if got := scrollOffDelta(1, 0, 0, 3, 2); got != -2 {
		t.Fatalf("small vp up: got delta=%d want -2", got)
	}
}
