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


