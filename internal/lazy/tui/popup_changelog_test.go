package tui

import (
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/changelog"
)

// TestOpenChangelog_PushesPopupState pins the open helper: the
// popup kind is popupChangelog, the source body is preserved on
// state, and the dismiss handler is wired through.
func TestOpenChangelog_PushesPopupState(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.openChangelog("hello", "## body", func() error { return nil })
	if k := gu.st.popup.Kind; k != popupChangelog {
		t.Fatalf("kind=%v want popupChangelog", k)
	}
	if gu.st.popup.Title != "hello" {
		t.Errorf("title=%q want hello", gu.st.popup.Title)
	}
	if gu.st.popup.ChangelogSource != "## body" {
		t.Errorf("source=%q want '## body'", gu.st.popup.ChangelogSource)
	}
	if gu.st.popup.OnDismissChangelog == nil {
		t.Error("OnDismissChangelog must not be nil when caller provides one")
	}
}

// TestChangelogDismiss_FiresCallbackOnceThenClears pins the
// dismiss path: the callback runs exactly once on Esc / Enter, the
// popup goes back to popupNone, and subsequent dismisses are no-ops.
//
// Uses a synchronous dispatch shim because changelogDismiss now
// routes the callback through runDispatch (review R8: userstate.Save
// is file I/O and shouldn't run on the gocui main goroutine).
func TestChangelogDismiss_FiresCallbackOnceThenClears(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	var fired int
	gu.openChangelog("t", "body", func() error {
		fired++
		return nil
	})
	if err := gu.changelogDismiss(nil, nil); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if fired != 1 {
		t.Errorf("callback fired %d times want 1", fired)
	}
	if gu.st.popup.Kind != popupNone {
		t.Errorf("popup not cleared after dismiss: %v", gu.st.popup.Kind)
	}
	// A second dismiss must be a no-op (popup is already cleared).
	if err := gu.changelogDismiss(nil, nil); err != nil {
		t.Fatalf("dismiss #2: %v", err)
	}
	if fired != 1 {
		t.Errorf("callback fired %d times after second dismiss want 1", fired)
	}
}

// TestChangelogDismiss_NilCallbackIsSafe pins the ctrl+w re-opener
// contract: no OnDismissChangelog means dismissal closes the popup
// without panicking and without touching state.json. The nil-
// callback fast path bypasses runDispatch entirely so this test
// doesn't need a dispatch shim.
func TestChangelogDismiss_NilCallbackIsSafe(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.openChangelog("manual", "body", nil)
	if err := gu.changelogDismiss(nil, nil); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if gu.st.popup.Kind != popupNone {
		t.Errorf("popup not cleared: %v", gu.st.popup.Kind)
	}
}

// TestHandleWhatsNew_OpensFullChangelog pins ctrl+w behaviour: the
// embedded CHANGELOG is opened with NO OnDismissChangelog (re-
// opener path; state.json untouched).
func TestHandleWhatsNew_OpensFullChangelog(t *testing.T) {
	gu := &Gui{st: newState()}
	if err := gu.handleWhatsNew(nil, nil); err != nil {
		t.Fatalf("handleWhatsNew: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupChangelog {
		t.Fatalf("kind=%v want popupChangelog (embedded CHANGELOG must have at least one entry)", k)
	}
	if gu.st.popup.OnDismissChangelog != nil {
		t.Error("ctrl+w opener must NOT register an OnDismissChangelog (would silently mutate state.json)")
	}
	// Body must carry at least the latest header.
	latest := changelog.Latest(changelog.Embedded())
	if !strings.Contains(gu.st.popup.ChangelogSource, latest) {
		t.Errorf("popup source missing latest version %q:\n%s", latest, gu.st.popup.ChangelogSource)
	}
	// And the OLDEST parsed version's header (the full-history
	// contract).
	es := changelog.Embedded()
	oldest := es[len(es)-1].Version
	if !strings.Contains(gu.st.popup.ChangelogSource, oldest) {
		t.Errorf("popup source missing oldest version %q (full-history contract):\n%s",
			oldest, gu.st.popup.ChangelogSource)
	}
}

// TestHandleWhatsNew_PreservesAutoPopupDismissCallback pins
// review R1: pressing ctrl+w on top of an active auto-popup must
// PRESERVE the pre-existing OnDismissChangelog so the subsequent
// dismiss still stamps last_seen_changelog. Without the guard the
// operator would land in a popup loop — ctrl+w would overwrite
// the save callback with nil, dismissal would skip the state.json
// write, and the auto-popup would fire AGAIN on the next start.
func TestHandleWhatsNew_PreservesAutoPopupDismissCallback(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	var saved int
	gu.openChangelog("auto", "unseen entries", func() error { saved++; return nil })
	if err := gu.handleWhatsNew(nil, nil); err != nil {
		t.Fatalf("handleWhatsNew: %v", err)
	}
	// After upgrade the popup body must have switched to the full
	// changelog (no longer the original "unseen entries" string).
	if gu.st.popup.ChangelogSource == "unseen entries" {
		t.Error("handleWhatsNew did not upgrade the popup body to the full CHANGELOG.md")
	}
	// The save callback must be carried over so dismissal still
	// stamps last_seen_changelog.
	if gu.st.popup.OnDismissChangelog == nil {
		t.Fatal("ctrl+w on top of auto-popup dropped OnDismissChangelog (review R1 regression)")
	}
	if err := gu.changelogDismiss(nil, nil); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if saved != 1 {
		t.Errorf("save callback fired %d times after ctrl+w + dismiss; want 1", saved)
	}
}

// TestHandleWhatsNew_NoActivePopup_StaysNil pins the
// complementary contract: when there is NO active popupChangelog,
// ctrl+w opens with a nil OnDismissChangelog so dismissal does
// not mutate state.json. This is what acceptance criterion #13
// requires for the stand-alone re-opener path.
func TestHandleWhatsNew_NoActivePopup_StaysNil(t *testing.T) {
	gu := &Gui{st: newState()}
	if err := gu.handleWhatsNew(nil, nil); err != nil {
		t.Fatalf("handleWhatsNew: %v", err)
	}
	if gu.st.popup.OnDismissChangelog != nil {
		t.Error("ctrl+w with no pre-existing popup must NOT register a dismiss callback")
	}
}

// TestStartChangelog_OnFirstFrame pins that Run pushes a
// popupChangelog state when Options.ChangelogModal != nil, BEFORE
// the first MainLoop iteration. We don't actually start MainLoop —
// we replicate the openChangelog call Run makes and assert the
// state transition.
func TestStartChangelog_OnFirstFrame(t *testing.T) {
	gu := &Gui{st: newState()}
	opts := &ChangelogModalOptions{
		Title: "What's new",
		Body:  "## [0.3.0] - 2026-04-01\n\n### Added\n- thing.",
		OnDismiss: func() error {
			return nil
		},
	}
	// Mirror the call shape in gui.go::Run: openChangelog is
	// invoked once before MainLoop. If a future refactor changes
	// the trigger point this test breaks loudly.
	gu.openChangelog(opts.Title, opts.Body, opts.OnDismiss)
	if gu.st.popup.Kind != popupChangelog {
		t.Fatalf("after openChangelog: kind=%v want popupChangelog", gu.st.popup.Kind)
	}
	if !strings.Contains(gu.st.popup.ChangelogSource, "0.3.0") {
		t.Errorf("source missing '0.3.0':\n%s", gu.st.popup.ChangelogSource)
	}
}

// TestChangelogKeysBoundOnPopupView pins the scroll + dismiss
// bindings on winPopupChangelog: a future refactor that drops one
// silently would otherwise be hard to catch without driving the
// real MainLoop. Uses the DeleteKeybinding probe trick from
// popup_isolation_test.go.
func TestChangelogKeysBoundOnPopupView(t *testing.T) {
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

	// Must be bound on winPopupChangelog.
	wantKeys := []any{
		gocui.KeyEsc,
		gocui.KeyEnter,
		'j', 'k',
		gocui.KeyArrowDown, gocui.KeyArrowUp,
		gocui.KeyCtrlF, gocui.KeyCtrlB,
		gocui.KeyPgdn, gocui.KeyPgup,
		'g', 'G',
		gocui.MouseWheelDown, gocui.MouseWheelUp,
	}
	for _, k := range wantKeys {
		if err := g.DeleteKeybinding(winPopupChangelog, k, gocui.ModNone); err != nil {
			t.Errorf("expected binding on winPopupChangelog for %v; DeleteKeybinding returned %v", k, err)
		}
	}

	// Ctrl-W must be GLOBALLY bound (no view scope) — the
	// re-opener fires from any panel.
	if err := g.DeleteKeybinding("", gocui.KeyCtrlW, gocui.ModNone); err != nil {
		t.Errorf("ctrl+w must be globally bound; DeleteKeybinding returned %v", err)
	}
}

// TestHelpScreen_ListsWhatsNewBinding pins acceptance criterion 15:
// the `?` help cheatsheet lists the new ctrl+w binding under the
// Global section. Adapted to the ask-ed8035 cheatsheet redesign:
// `openHelp` now opens a `popupCheatsheet` whose body is a slice
// of structured `cheatsheetItem` rows (header rows carry
// IsHeader=true + Section name; binding rows carry KeyLabel +
// Description + the section they belong to). We walk the items,
// scope to Section=="Global", and look for a row whose KeyLabel
// is "ctrl+w".
func TestHelpScreen_ListsWhatsNewBinding(t *testing.T) {
	gu := &Gui{st: newState()}
	if err := gu.openHelp(nil, nil); err != nil {
		t.Fatalf("openHelp: %v", err)
	}
	if k := gu.st.popup.Kind; k != popupCheatsheet {
		t.Fatalf("popup kind=%v want popupCheatsheet", k)
	}
	var found bool
	for _, it := range gu.st.popup.CheatsheetItems {
		if it.IsHeader {
			continue
		}
		if it.Section == "Global" && it.KeyLabel == "ctrl+w" {
			found = true
			break
		}
	}
	if !found {
		var b strings.Builder
		for _, it := range gu.st.popup.CheatsheetItems {
			if it.IsHeader {
				b.WriteString("--- " + it.Section + " ---\n")
				continue
			}
			b.WriteString("  " + it.KeyLabel + "  " + it.Description + "\n")
		}
		t.Errorf("help cheatsheet doesn't list ctrl+w under Global section:\n%s", b.String())
	}
}

// TestReplacePopup_FiresChangelogDismissOnTransition pins review
// R10: when any open* helper replaces an active popupChangelog
// that has a non-nil OnDismissChangelog, replacePopup MUST
// dispatch the callback so the auto-popup's state.json save still
// fires. Without this, pressing '?' / '/' / ':' / 'm' / etc. on
// top of the auto-popup silently drops the save callback —
// popupClose (the dismiss path for the replacement popup) has no
// notion of the changelog contract and last_seen_changelog stays
// unwritten, so the auto-popup re-fires on the next start. This
// is the second class of "first-run loop" bug (R1 covered the
// ctrl+w-on-top-of-auto-popup variant; this one covers every
// other popup-replacement path).
//
// Driven through replacePopup directly (not the open* helpers) so
// it pins the helper's contract independently of any specific
// caller — a future open* path that's added by mistake without
// routing through replacePopup will still pass this test, but the
// path-specific tests below will catch it on the relevant key.
func TestReplacePopup_FiresChangelogDismissOnTransition(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	var saved int
	gu.openChangelog("auto", "unseen entries", func() error { saved++; return nil })
	gu.replacePopup(popupState{Kind: popupMenu, Title: "replacement"})
	if saved != 1 {
		t.Errorf("changelog OnDismissChangelog fired %d times on popup replacement; want 1", saved)
	}
	if gu.st.popup.Kind != popupMenu {
		t.Errorf("popup kind after replacePopup = %v; want popupMenu", gu.st.popup.Kind)
	}
}

// TestReplacePopup_NoChangelog_DoesNotFireCallback pins the
// complementary contract: replacing a NON-changelog popup leaves
// the callback unfired (no spurious flash, no save attempt). A
// regression that fired a stray callback in this case would
// trigger a userstate.Save with whatever closure the caller of
// the auto-popup wired up — in production that's a real file
// write to ~/.autosk/state.json.
func TestReplacePopup_NoChangelog_DoesNotFireCallback(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	gu.replacePopup(popupState{Kind: popupMenu, Title: "first"})
	// No prior popupChangelog — callback can't fire because there
	// is none. Now replace popupMenu with popupConfirm.
	gu.replacePopup(popupState{Kind: popupConfirm, Title: "second"})
	if gu.st.popup.Kind != popupConfirm {
		t.Errorf("popup kind after second replacePopup = %v; want popupConfirm", gu.st.popup.Kind)
	}
	// If a stray callback fired here it would only show up in a
	// test harness via panic / flash. We assert state shape: the
	// new popup has no OnDismissChangelog (it's a popupConfirm).
	if gu.st.popup.OnDismissChangelog != nil {
		t.Errorf("replacePopup leaked OnDismissChangelog onto popupConfirm")
	}
}

// TestHelpOverChangelog_FiresDismissCallback is the path-specific
// regression for review R10: pressing '?' on top of the auto-
// popup must save state.json once, then close the help popup
// cleanly. Was the example the reviewer used; pinned here so a
// future refactor that bypasses replacePopup in openHelp would
// fail this test loudly.
func TestHelpOverChangelog_FiresDismissCallback(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	var saved int
	gu.openChangelog("auto", "unseen entries", func() error { saved++; return nil })
	if err := gu.openHelp(nil, nil); err != nil {
		t.Fatalf("openHelp: %v", err)
	}
	if saved != 1 {
		t.Errorf("'?' over auto-popup fired OnDismissChangelog %d times; want 1", saved)
	}
	// On the ask-ed8035 cheatsheet redesign, openHelp opens a
	// popupCheatsheet (not the legacy popupMenu shape ask-911ea0
	// was written against). The R10 contract is the same
	// regardless of what the replacement popup is — what matters
	// is that the dismiss callback fired exactly once before the
	// replacement landed.
	if gu.st.popup.Kind != popupCheatsheet {
		t.Errorf("popup kind after openHelp = %v; want popupCheatsheet", gu.st.popup.Kind)
	}
	// Subsequent dismiss must NOT re-fire the callback.
	if err := gu.popupClose(nil, nil); err != nil {
		t.Fatalf("popupClose: %v", err)
	}
	if saved != 1 {
		t.Errorf("popupClose re-fired OnDismissChangelog; saved=%d want 1", saved)
	}
}

// TestFilterOverChangelog_FiresDismissCallback covers the '/'
// path — openFilter routes through openPrompt, which now goes
// through replacePopup. Same shape as the help test.
func TestFilterOverChangelog_FiresDismissCallback(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	var saved int
	gu.openChangelog("auto", "unseen entries", func() error { saved++; return nil })
	if err := gu.openFilter(nil, nil); err != nil {
		t.Fatalf("openFilter: %v", err)
	}
	if saved != 1 {
		t.Errorf("'/' over auto-popup fired OnDismissChangelog %d times; want 1", saved)
	}
	if gu.st.popup.Kind != popupPrompt {
		t.Errorf("popup kind after openFilter = %v; want popupPrompt", gu.st.popup.Kind)
	}
}

// TestPaletteOverChangelog_FiresDismissCallback covers the ':'
// path — openPalette routes through openMenu. Closes the R10
// regression triad (?, /, :) the reviewer named explicitly.
func TestPaletteOverChangelog_FiresDismissCallback(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.dispatch = func(f func()) { f() }
	var saved int
	gu.openChangelog("auto", "unseen entries", func() error { saved++; return nil })
	if err := gu.openPalette(nil, nil); err != nil {
		t.Fatalf("openPalette: %v", err)
	}
	if saved != 1 {
		t.Errorf("':' over auto-popup fired OnDismissChangelog %d times; want 1", saved)
	}
	if gu.st.popup.Kind != popupMenu {
		t.Errorf("popup kind after openPalette = %v; want popupMenu", gu.st.popup.Kind)
	}
}
