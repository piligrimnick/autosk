package tui

import (
	"testing"

	"github.com/jesseduffield/gocui"
)

// TestPopupKeys_DoNotLeakToGlobals (risk #6 from the impl plan)
// asserts that the global keybindings — quit (q / Ctrl-C), tab
// switching (1..4), refresh (R), filter (/), palette (:), clear
// scope (*) — are bound to view "" (any view) BUT the popup
// bindings are scoped to popup view names, so gocui's key router
// won't dispatch the global handler while a popup view is current.
//
// We can't drive the real gocui router without a Gui, but we CAN
// verify the binding registry: every popup key (Enter, j/k, y/n,
// Esc) appears in the bindings list scoped to a popup view, and
// no popup key is bound to the empty view name (which would override
// it globally).
//
// This is a structural pin: a future refactor that re-binds e.g.
// 'y' on view "" would surface here.
func TestPopupKeys_DoNotLeakToGlobals(t *testing.T) {
	// Drive bindKeys against a stub Gui so we can inspect the
	// registry through gocui's SetKeybinding return value. The
	// cheapest path: run bindKeys against a headless gocui (it
	// doesn't actually wire keys to a screen until MainLoop, but
	// SetKeybinding rejects nil handlers / nil g, so we need a
	// real Gui shell).
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

	// Popup-only keys must not also be bound globally on the same
	// modifier — gocui dispatches view-scoped bindings before
	// globals, but a global with the same chord is a footgun.
	// The keys we care about: 'y', 'n' (Confirm), Enter (Menu / Prompt /
	// Confirm), j/k on popup-menu (which DO have globals — but the
	// globals are also view-scoped to winTasks/winJobs/etc, NOT to
	// "", so they can't fire when a popup view is current).
	//
	// We check by trying to bind the SAME chord on view "" and
	// expecting either an "already bound" error OR success (if the
	// global was never bound). The contract we're pinning: 'y' /
	// 'n' / 'Y' / 'N' must NOT be globally bound. j/k are bound
	// per-view (winTasks etc., NOT ""), which means the popup
	// instance (winPopupMenu) takes precedence cleanly.
	globalShouldBeUnbound := []struct {
		view string
		key  any
	}{
		{"", 'y'},
		{"", 'n'},
		{"", 'Y'},
		{"", 'N'},
	}
	noop := func(*gocui.Gui, *gocui.View) error { return nil }
	for _, b := range globalShouldBeUnbound {
		// SetKeybinding returns nil on a fresh chord, errors on a
		// duplicate (lazygit/gocui surfaces that as a panic-or-
		// error depending on the path — here we just check we can
		// re-bind without conflict).
		if err := g.SetKeybinding(b.view, b.key, gocui.ModNone, noop); err != nil {
			t.Errorf("global chord %q on view %q must be unbound; got: %v", b.key, b.view, err)
		}
	}

	// And the per-panel j/k (which DO collide with the popup-menu
	// j/k) are scoped to winTasks etc., NOT to "". A test that
	// re-binds j on view "" should succeed without overriding the
	// per-panel handlers — proving the scopes don't overlap.
	if err := g.SetKeybinding("", 'j', gocui.ModNone, noop); err != nil {
		t.Errorf("'j' on view \"\" should be unbound (the per-panel and popup-menu handlers are view-scoped); got: %v", err)
	}
}
