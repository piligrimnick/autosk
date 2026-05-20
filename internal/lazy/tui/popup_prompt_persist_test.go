package tui

import (
	"testing"

	"github.com/jesseduffield/gocui"
)

// TestLayoutPopupPromptPreservesTypedText pins the fix for the
// "can't type in the new-task title popup" bug.
//
// Before the fix, layoutPopup unconditionally deleted all three popup
// views at the top of every layout pass and re-created the active
// one via SetView. Because gocui's NewView constructor installs a
// fresh TextArea{} on every new view, every keystroke (which forces
// a flush → layout cycle) wiped the character the editor had just
// inserted. Result: the prompt looked broken — typing did nothing.
//
// This test drives a real headless gocui, opens a prompt, writes a
// character through the same code path SimpleEditor uses (TextArea
// .TypeCharacter), then asks layoutPopup to run again. The typed
// character must still be in the view's buffer afterwards. If a
// future refactor reintroduces the delete-every-pass behaviour, the
// buffer will come back empty and this assertion fails.
//
// The same view-lifetime invariant is what allows ALL prompt-driven
// panels to accept text — task new, filter, enroll, resume, block,
// unblock, comment, workflow file path. Fixing it in one place
// fixes them all.
func TestLayoutPopupPromptPreservesTypedText(t *testing.T) {
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

	// Open a prompt and run a layout pass so the view exists.
	gu.openPrompt("new task title:", "", func(string) error { return nil })
	gu.layoutPopup(g, 80, 24)

	v, err := g.View(winPopupPrompt)
	if err != nil {
		t.Fatalf("popup view missing after first layout: %v", err)
	}
	if !v.Editable {
		t.Fatalf("prompt view should be Editable")
	}

	// Simulate the editor inserting a character — this is what
	// SimpleEditor does for every printable rune typed by the user.
	v.TextArea.TypeCharacter("h")
	v.TextArea.TypeCharacter("i")
	v.RenderTextArea()

	if got := v.TextArea.GetContent(); got != "hi" {
		t.Fatalf("after typing 'hi': TextArea content = %q want %q", got, "hi")
	}

	// Now run another layout pass — exactly what gocui's flush()
	// triggers after every key event. The fix preserves the active
	// popup view; the regression would delete + recreate it and the
	// TextArea would reset to empty.
	gu.layoutPopup(g, 80, 24)

	v2, err := g.View(winPopupPrompt)
	if err != nil {
		t.Fatalf("popup view missing after second layout: %v", err)
	}
	if v2 != v {
		t.Fatalf("layoutPopup re-created the prompt view across passes (got %p, want %p) — typed text would be lost", v2, v)
	}
	if got := v2.TextArea.GetContent(); got != "hi" {
		t.Fatalf("typed text lost across layout passes: TextArea content = %q want %q", got, "hi")
	}
}

// TestLayoutPopupSwitchingKindRecreates pins the other half of the
// invariant: when the active popup KIND changes (e.g. user closes a
// prompt and opens a menu, or vice-versa), the previously-active
// popup view must be cleaned up so a stale frame doesn't linger.
func TestLayoutPopupSwitchingKindRecreates(t *testing.T) {
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

	// Prompt → menu: the prompt view must disappear.
	gu.openPrompt("title?", "", func(string) error { return nil })
	gu.layoutPopup(g, 80, 24)
	if _, err := g.View(winPopupPrompt); err != nil {
		t.Fatalf("prompt view should exist after open+layout: %v", err)
	}

	gu.st.withLock(func() { gu.st.popup = popupState{} })
	gu.openMenu("pick", []string{"a", "b"}, func(int) error { return nil })
	gu.layoutPopup(g, 80, 24)

	if _, err := g.View(winPopupPrompt); err == nil {
		t.Fatalf("prompt view should have been deleted when switching to menu")
	}
	if _, err := g.View(winPopupMenu); err != nil {
		t.Fatalf("menu view should exist after switching: %v", err)
	}

	// Close popup entirely: all three popup views must be gone.
	gu.st.withLock(func() { gu.st.popup = popupState{} })
	gu.layoutPopup(g, 80, 24)
	for _, name := range []string{winPopupMenu, winPopupConfirm, winPopupPrompt} {
		if _, err := g.View(name); err == nil {
			t.Fatalf("popup view %q should be deleted when no popup is active", name)
		}
	}
}
