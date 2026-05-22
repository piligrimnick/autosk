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

// TestDetailScrollTo_BottomUsesInnerSize pins the regression review
// flagged: detailScrollTo / detailScrollPage / scrollViewByLines
// must clamp against InnerSize (frame-excluded) not Size, otherwise
// G lands 2 lines short of the actual bottom and the last 2 lines
// of the transcript are permanently invisible via j or wheel-down.
//
// gocui collapses the trailing newline in Buffer(), so we anchor
// the expectation on what strings.Count(v.Buffer(), "\n") actually
// returns rather than the writer-side newline count.
func TestDetailScrollTo_BottomUsesInnerSize(t *testing.T) {
	gu := newHeadlessGui(t, 80, 40)
	// Outer view height 31, inner 29 (frame eats 2 cells).
	v, err := gu.g.SetView(winDetail, 0, 0, 79, 30, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}
	if v == nil {
		t.Fatal("SetView returned nil")
	}
	v.Frame = true
	body := strings.Repeat("line\n", 100)
	v.Clear()
	if _, err := v.Write([]byte(body)); err != nil {
		t.Fatalf("v.Write: %v", err)
	}
	_, innerH := v.InnerSize()
	if innerH < 1 {
		t.Fatalf("innerH=%d; fixture broken", innerH)
	}
	bufLines := strings.Count(v.Buffer(), "\n")
	want := bufLines - innerH

	// G → bottom.
	if err := gu.detailScrollTo(true)(nil, nil); err != nil {
		t.Fatalf("detailScrollTo: %v", err)
	}
	_, oy := v.Origin()
	if oy != want {
		t.Errorf("detailScrollTo(true) origin oy=%d want %d (innerH=%d, bufLines=%d)", oy, want, innerH, bufLines)
	}
	// Sanity: the buggy Size()-based math would have landed at oy =
	// bufLines - outerH = bufLines - (innerH+2). Make sure we're
	// not there.
	bug := bufLines - (innerH + 2)
	if oy == bug {
		t.Errorf("detailScrollTo(true) landed at the buggy Size() offset oy=%d (would be %d short of bottom)", oy, want-oy)
	}

	// Verify scrollViewByLines also uses InnerSize: from oy=0 a
	// massive forward step must clamp at the same lines-innerH.
	v.SetOrigin(0, 0)
	if err := gu.scrollViewByLines(winDetail, 9999); err != nil {
		t.Fatalf("scrollViewByLines: %v", err)
	}
	_, oy = v.Origin()
	if oy != want {
		t.Errorf("scrollViewByLines(9999) origin oy=%d want %d", oy, want)
	}
}
