package tui

import (
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"
)

// TestPopupConfirmKeysNotGloballyBound (a structural pin for
// risk #6 from the impl plan) asserts that the confirm-popup chords
// (y / n / Y / N) are NOT bound at the empty-view (global) level in
// the production keymap. gocui's dispatcher routes view-scoped
// bindings before globals, so a popup-active 'q' still triggers the
// global quit — that integration behaviour is intentionally outside
// the scope of this test (it'd require running a real headless
// MainLoop and is on the followup list as the proper risk-#6 test).
//
// What this test DOES pin: nobody accidentally adds e.g. a global
// 'y' binding (which would override the popup's per-view 'y' as a
// quit shortcut). The previous version of this test attempted to
// detect the binding via SetKeybinding, which always succeeds (gocui
// silently appends duplicates) and so pinned nothing at all. This
// version uses DeleteKeybinding, which returns errors.New("keybinding
// not found") iff no matching binding exists — a real, falsifiable
// assertion.
//
// To prove the test bites: add e.g. `{"", 'y', gocui.ModNone, gu.quit}`
// to bindKeys and the assertion below fails because DeleteKeybinding
// returns nil (a binding existed and was removed).
func TestPopupConfirmKeysNotGloballyBound(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      80,
		Height:     24,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}
	if err := gu.bindKeys(); err != nil {
		t.Fatalf("bindKeys: %v", err)
	}

	// DeleteKeybinding(view, key, mod) returns nil iff a matching
	// binding was found AND removed. If it returns "keybinding not
	// found" we know the production keymap doesn't register that
	// chord. The popup confirm chords must NOT be globally bound —
	// otherwise a popup-active 'y' would conflict with a global
	// handler (gocui appends duplicates silently).
	for _, key := range []any{'y', 'n', 'Y', 'N'} {
		err := g.DeleteKeybinding("", key, gocui.ModNone)
		if err == nil {
			t.Errorf("chord %q IS globally bound; production keymap must NOT register confirm chords at view \"\" (popup leak)", key)
			continue
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("DeleteKeybinding(%q) returned unexpected error: %v (want 'not found')", key, err)
		}
	}

	// Sanity-check the same assertion against a chord we KNOW is
	// globally bound: 'q' should be a global quit. Deleting it must
	// succeed (returning nil). If this branch fails, the test
	// infrastructure itself is broken (e.g. bindKeys never ran).
	if err := g.DeleteKeybinding("", 'q', gocui.ModNone); err != nil {
		t.Fatalf("sanity check: 'q' must be globally bound (quit); DeleteKeybinding returned %v", err)
	}
}
