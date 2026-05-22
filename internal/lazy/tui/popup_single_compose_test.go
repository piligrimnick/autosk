package tui

import (
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"
)

// TestOpenSingleComposeState verifies openSingleCompose records the
// expected popup-state shape so the layout pass and key handlers
// can tell the popup apart from the one-line prompt that lives in
// the same Input slot.
func TestOpenSingleComposeState(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.openSingleCompose("Title", "JSON object", "seed text", func(string) error { return nil })

	if k := gu.st.popup.Kind; k != popupSingleCompose {
		t.Fatalf("popup kind = %v, want popupSingleCompose", k)
	}
	if gu.st.popup.Title != "Title" {
		t.Errorf("title = %q, want %q", gu.st.popup.Title, "Title")
	}
	if gu.st.popup.Hint != "JSON object" {
		t.Errorf("hint = %q, want %q", gu.st.popup.Hint, "JSON object")
	}
	if gu.st.popup.Input != "seed text" {
		t.Errorf("input seed = %q, want %q", gu.st.popup.Input, "seed text")
	}
	if gu.st.popup.OnAccept == nil {
		t.Errorf("OnAccept not recorded")
	}
}

// TestSingleComposeConfirmReadsTextArea drives a real headless
// gocui, opens the single-compose popup, types into the textarea,
// and verifies singleComposeConfirm clears popup state and fires
// OnAccept with the trimmed content (matching taskComposeConfirm's
// behaviour: trailing newlines are stripped).
func TestSingleComposeConfirmReadsTextArea(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	gu := &Gui{g: g, st: newState()}
	var got string
	var fired int
	gu.openSingleCompose("Comment", "markdown ok", "", func(text string) error {
		fired++
		got = text
		return nil
	})
	gu.layoutPopup(g, 120, 40)

	v, err := g.View(winSingleCompose)
	if err != nil {
		t.Fatalf("view missing after layout: %v", err)
	}
	if !v.Editable {
		t.Fatalf("single-compose view must be Editable")
	}
	v.TextArea.TypeString("line one\nline two")

	if err := gu.singleComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if fired != 1 {
		t.Fatalf("OnAccept fired %d times, want 1", fired)
	}
	if got != "line one\nline two" {
		t.Errorf("got %q, want %q", got, "line one\nline two")
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared after confirm: kind=%v", k)
	}
}

// TestPopupCloseOnSingleComposeSkipsOnAccept pins the cancel
// contract: Esc → popupClose clears state without invoking OnAccept.
func TestPopupCloseOnSingleComposeSkipsOnAccept(t *testing.T) {
	gu := &Gui{st: newState()}
	var fired int
	gu.openSingleCompose("", "", "seed", func(string) error {
		fired++
		return nil
	})
	if err := gu.popupClose(nil, nil); err != nil {
		t.Fatalf("popupClose: %v", err)
	}
	if fired != 0 {
		t.Errorf("OnAccept fired %d times on cancel, want 0", fired)
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared: kind=%v", k)
	}
}

// TestComposeSingleSubtitleAlwaysShowsSubmitHint pins the format of
// the bottom-right hint string: the submit / cancel chords MUST be
// rendered every time so the user can discover them without trial
// and error. An optional caller-supplied label (e.g. "JSON object")
// is appended on the left.
func TestComposeSingleSubtitleAlwaysShowsSubmitHint(t *testing.T) {
	cases := []struct {
		hint string
		want []string // substrings that must be present
	}{
		{"", []string{"<c-s>", "<a-enter>", "submit", "<esc>", "cancel"}},
		{"markdown ok", []string{"<c-s>", "<a-enter>", "submit", "<esc>", "cancel", "markdown ok"}},
		{"JSON object", []string{"<c-s>", "<a-enter>", "submit", "<esc>", "cancel", "JSON object"}},
	}
	for _, tc := range cases {
		got := composeSingleSubtitle(tc.hint)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("composeSingleSubtitle(%q) missing %q: got %q", tc.hint, want, got)
			}
		}
	}
}

// TestLayoutSingleComposePersistsTypedText pins the load-bearing
// view-lifetime invariant: layoutPopup must NOT delete + re-create
// the single-compose view on every pass, or gocui's NewView
// constructor reinstalls a fresh TextArea and the user's typed text
// disappears on the next keystroke (the same regression that the
// prompt and the task-compose tests pin).
func TestLayoutSingleComposePersistsTypedText(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	gu := &Gui{g: g, st: newState()}
	gu.openSingleCompose("Comment", "markdown ok", "", func(string) error { return nil })
	gu.layoutPopup(g, 120, 40)
	v, err := g.View(winSingleCompose)
	if err != nil {
		t.Fatalf("view missing: %v", err)
	}
	v.TextArea.TypeString("hi")
	v.RenderTextArea()

	gu.layoutPopup(g, 120, 40)
	v2, _ := g.View(winSingleCompose)
	if v2 != v {
		t.Errorf("single-compose view re-created across layout passes — typed text would be lost")
	}
	if got := v2.TextArea.GetContent(); got != "hi" {
		t.Errorf("content lost across layout pass: %q", got)
	}
}

// TestLayoutSingleComposeSeedsInitial pins the first-pass seeding:
// when popup.Input carries an initial value (e.g. pretty-printed
// metadata or the previous-attempt's typed text for re-open) the
// view's TextArea is populated from it on the first layout pass.
func TestLayoutSingleComposeSeedsInitial(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()

	gu := &Gui{g: g, st: newState()}
	const seed = "{\n  \"foo\": \"bar\"\n}"
	gu.openSingleCompose("Metadata", "JSON object", seed, func(string) error { return nil })
	gu.layoutPopup(g, 120, 40)
	v, err := g.View(winSingleCompose)
	if err != nil {
		t.Fatalf("view missing: %v", err)
	}
	if got := strings.TrimRight(v.TextArea.GetContent(), "\n"); got != seed {
		t.Errorf("seeded content = %q, want %q", got, seed)
	}
}

// TestPopupIsolationSingleCompose pins risk #6 for the new popup:
// while popupSingleCompose is active, no other popup view (menu,
// confirm, prompt, task compose) is left on screen.
func TestPopupIsolationSingleCompose(t *testing.T) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      120,
		Height:     40,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	defer g.Close()
	gu := &Gui{g: g, st: newState()}

	// Open the menu first so a menu view exists, then switch to the
	// single compose: the menu view must be cleaned up by the layout
	// pass.
	gu.openMenu("pick", []string{"a", "b"}, func(int) error { return nil })
	gu.layoutPopup(g, 120, 40)
	if _, err := g.View(winPopupMenu); err != nil {
		t.Fatalf("menu view missing after first layout: %v", err)
	}

	gu.st.popup = popupState{}
	gu.openSingleCompose("Comment", "", "", func(string) error { return nil })
	gu.layoutPopup(g, 120, 40)

	for _, name := range []string{
		winPopupMenu, winPopupConfirm, winPopupPrompt,
		winTaskComposeSummary, winTaskComposeDescription,
	} {
		if _, err := g.View(name); err == nil {
			t.Errorf("popup view %q must be deleted while popupSingleCompose is active", name)
		}
	}
	if _, err := g.View(winSingleCompose); err != nil {
		t.Errorf("single-compose view should exist: %v", err)
	}
}

// TestArrangementHasSingleCompose pins that the new winSingleCompose
// is registered in allPopupWindows so layoutPopup's garbage-collect
// pass can clean it up when the popup closes.
func TestArrangementHasSingleCompose(t *testing.T) {
	found := false
	for _, name := range allPopupWindows {
		if name == winSingleCompose {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("winSingleCompose missing from allPopupWindows: %v", allPopupWindows)
	}
}
