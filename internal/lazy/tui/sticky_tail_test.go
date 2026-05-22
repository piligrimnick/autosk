package tui

import (
	"strings"
	"testing"
)

// stickyTailFixture wires a small headless gui + a winDetail view
// sized to a known height. The caller writes a first body to seed
// the buffer, then writes a second body to exercise sticky-tail.
func stickyTailFixture(t *testing.T, w, h int) (*Gui, string) {
	t.Helper()
	gu := newHeadlessGui(t, w, h)
	// Create winDetail at a known size. The view height of 10 + frame
	// gives us inner height = 8 lines so the math is easy to reason
	// about.
	if _, err := gu.g.SetView(winDetail, 0, 0, w-1, 10, 0); err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}
	return gu, winDetail
}

// TestStickyTail_AtBottom_FollowsNewContent: when the view is at the
// bottom and a longer body arrives, the origin moves so the tail
// stays visible.
func TestStickyTail_AtBottom_FollowsNewContent(t *testing.T) {
	gu, name := stickyTailFixture(t, 80, 24)
	v, _ := gu.g.View(name)
	if v == nil {
		t.Fatalf("view %q nil after SetView", name)
	}

	// First body: 5 lines (fits in the view, oy=0).
	first := strings.Repeat("line\n", 5)
	gu.writeViewSticky(name, "", first)
	if _, oy := v.Origin(); oy != 0 {
		t.Fatalf("first frame origin should be 0; got %d", oy)
	}

	// Second body: 100 lines. The user was at the bottom (since the
	// first body fit in the viewport), so the origin must advance.
	second := strings.Repeat("line\n", 100)
	gu.writeViewSticky(name, "", second)
	_, oy := v.Origin()
	if oy == 0 {
		t.Errorf("sticky-tail did not advance origin after body grew; oy=%d", oy)
	}
	// The bottom of the viewport must be at or past the body's last
	// line. Inner height is the writable area inside the frame.
	_, h := v.InnerSize()
	newLines := strings.Count(v.Buffer(), "\n")
	t.Logf("after second write: oy=%d innerH=%d bufferLines=%d", oy, h, newLines)
	bottom := oy + h
	if bottom < newLines {
		t.Errorf("sticky tail viewport bottom %d below body length %d", bottom, newLines)
	}
}

// TestStickyTail_ScrolledUp_OriginPreserved: when the user has
// scrolled up, a follow-up writeViewSticky must NOT yank the
// viewport back to the bottom.
func TestStickyTail_ScrolledUp_OriginPreserved(t *testing.T) {
	gu, name := stickyTailFixture(t, 80, 24)
	v, _ := gu.g.View(name)

	first := strings.Repeat("line\n", 100)
	gu.writeViewSticky(name, "", first)
	// Manually scroll up by setting origin to 5.
	ox, _ := v.Origin()
	v.SetOrigin(ox, 5)

	// New body lands; sticky-tail must respect the scrolled-up origin.
	second := strings.Repeat("line\n", 200)
	gu.writeViewSticky(name, "", second)
	_, oy := v.Origin()
	if oy != 5 {
		t.Errorf("sticky-tail clobbered scrolled-up origin; oy=%d want 5", oy)
	}
}

// TestStickyTail_FirstFrame_StartsAtBottom: an empty view receiving
// its first body must start anchored at the bottom (so live events
// arriving on first frame are immediately visible).
func TestStickyTail_FirstFrame_StartsAtBottom(t *testing.T) {
	gu, name := stickyTailFixture(t, 80, 24)
	v, _ := gu.g.View(name)

	body := strings.Repeat("line\n", 50)
	gu.writeViewSticky(name, "", body)
	_, oy := v.Origin()
	if oy == 0 {
		t.Errorf("first-frame sticky tail did not advance origin; oy=%d (body has 50 lines, view height ~10)", oy)
	}
}
