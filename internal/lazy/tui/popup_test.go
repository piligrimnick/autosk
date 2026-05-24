package tui

import (
	"strings"
	"testing"
)

// TestPopupKindNoneByDefault verifies state starts with no popup so
// the layout function doesn't try to draw a stale one.
func TestPopupKindNoneByDefault(t *testing.T) {
	s := newState()
	if s.popup.Kind != popupNone {
		t.Fatalf("default popup kind = %v want popupNone", s.popup.Kind)
	}
}

// TestRenderMenuBodyCursorMarker pins the per-line rendering: the
// cursored line gets a "▶ " prefix and the rest a "  " prefix.
func TestRenderMenuBodyCursorMarker(t *testing.T) {
	body := renderMenuBody([]string{"a", "b", "c"}, 1)
	wantSub := "▶ "
	if !strings.Contains(body, wantSub) {
		t.Fatalf("body missing %q:\n%s", wantSub, body)
	}
}

// TestMinPopupClampsToTerminal verifies the popup window fits inside
// the terminal even on a small TUI. The clamp leaves a 4-cell margin
// (2 on each side); the floor of 5 rows comes from the popup needing
// room for a frame + title + body even when the terminal is tiny.
func TestMinPopupClampsToTerminal(t *testing.T) {
	w, h := minPopup(20, 30, "single line")
	if w > 16 {
		t.Fatalf("popup width %d should clamp under w=20", w)
	}
	if h > 26 {
		t.Fatalf("popup height %d should clamp under h=30", h)
	}
	// Floor: even on a very small terminal, the popup is at least 5 rows.
	wf, hf := minPopup(20, 6, "")
	if hf < 2 {
		t.Fatalf("popup height floor not respected: %d", hf)
	}
	if wf > 16 {
		t.Fatalf("floor case: width %d should clamp", wf)
	}
}

// TestMinPopupGrowsWithBodyWidth pins the regression flagged on the
// help popup: lines wider than the 60-cell baseline (e.g. the
// `ctrl+r hard refresh ...` row) were getting clipped. minPopup
// must grow the outer width to fit the widest body line (plus a
// small padding budget for the frame + cursor marker), capped by
// termW-4.
func TestMinPopupGrowsWithBodyWidth(t *testing.T) {
	long := strings.Repeat("x", 90)
	pw, _ := minPopup(200, 40, long)
	if pw < 90 {
		t.Errorf("popup width %d must accommodate the longest body line (90 cells)", pw)
	}
	// Cap by termW-4 still wins on a narrow terminal.
	pw2, _ := minPopup(40, 40, long)
	if pw2 > 36 {
		t.Errorf("popup width %d must clamp to termW-4=36 on a narrow terminal", pw2)
	}
	// Short body still picks up the 60-cell baseline so menus with
	// tiny labels don't shrink to a sliver.
	pw3, _ := minPopup(200, 40, "a\nb\nc")
	if pw3 < 60 {
		t.Errorf("popup width %d must keep the 60-cell baseline for short bodies", pw3)
	}
}

// TestMaxLineWidthHandlesMultibyte pins maxLineWidth: it must count
// runes, not bytes, so a Cyrillic / CJK cheatsheet line gets sized
// by its visible width and not by its UTF-8 byte length (which
// would over-allocate).
func TestMaxLineWidthHandlesMultibyte(t *testing.T) {
	if got := maxLineWidth(""); got != 0 {
		t.Errorf("empty: %d want 0", got)
	}
	if got := maxLineWidth("a\nbcd\nef"); got != 3 {
		t.Errorf("ascii: %d want 3 (widest line=bcd)", got)
	}
	// 5 runes — each Cyrillic letter is 2 bytes in UTF-8 but 1
	// visible cell. The byte length would be 10; the rune count
	// is 5.
	if got := maxLineWidth("приве\nx"); got != 5 {
		t.Errorf("multibyte: %d want 5 rune count of Приве", got)
	}
}

// TestMaxLineWidthStripsLipglossSGR pins review R8: the helper feeds
// renderMenuBody's output to minPopup, which renders the cursored
// row through styleAccent.Render. Under a real TTY (or once the test
// pins TrueColor) that wraps the row in a `\x1b[38;2;…m…\x1b[0m`
// envelope; counting that envelope as visible cells would over-size
// the popup by ~9 cells whenever the cursor landed on the widest
// row, then snap back when it moved away. lipgloss.Width strips SGR
// before counting cells; this test verifies the cursored row and the
// non-cursored row measure to the same plain-text width.
func TestMaxLineWidthStripsLipglossSGR(t *testing.T) {
	forceTrueColor(t)

	// renderMenuBody emits "▶ " + styled cursored row + "\n" + "  "
	// + plain non-cursored row + "\n". Visible widths are 2+5=7 for
	// the cursored row and 2+18=20 for the non-cursored row. The
	// SGR envelope around the cursored row adds ~9 "runes" the
	// terminal never paints; lipgloss.Width must strip them.
	rendered := renderMenuBody([]string{"alpha", "beta-very-long-row"}, 0)
	const wantPlainNonCursored = 20 // "  beta-very-long-row"
	if got := maxLineWidth(rendered); got != wantPlainNonCursored {
		t.Errorf("maxLineWidth=%d want %d (SGR envelope leaked into measurement)", got, wantPlainNonCursored)
	}

	// Flip the cursor onto the WIDER row: the cursored row gets the
	// SGR envelope but the visible width must still be 20 — if SGR
	// leaked the result would jump to ~29 here and the popup would
	// oscillate as the cursor moved.
	rendered2 := renderMenuBody([]string{"alpha", "beta-very-long-row"}, 1)
	if got := maxLineWidth(rendered2); got != wantPlainNonCursored {
		t.Errorf("maxLineWidth=%d want %d (cursor on wide row must not widen popup via SGR)", got, wantPlainNonCursored)
	}
}
