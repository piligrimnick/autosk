package tui

import (
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"
)

// TestComposePanelWidth pins the lazygit-parity width formula:
//
//	width = min(4*termWidth/7, 100), floored to min(termWidth-2, 80).
//
// The floor handles narrow terminals: on a 60-wide terminal the
// 4/7 result is 34, which is too thin to type a sensible title; we
// pin to 80 instead, then clamp to termWidth-2 if that overshoots.
// Mirrors lazygit's confirmation_helper.go::getPopupPanelWidth.
func TestComposePanelWidth(t *testing.T) {
	cases := []struct {
		termW int
		want  int
	}{
		// Wide terminals: capped at 100.
		{200, 100},
		{180, 100},
		// "Medium" terminals: 4/7 rule wins.
		{160, 91},
		{140, 80}, // 4*140/7 = 80
		// Narrow: the 80-floor kicks in until termW-2 < 80.
		{100, 80},
		{90, 80},
		{82, 80},
		// Very narrow: clamp to termW-2.
		{70, 68},
		{50, 48},
	}
	for _, tc := range cases {
		if got := composePanelWidth(tc.termW); got != tc.want {
			t.Errorf("composePanelWidth(%d) = %d, want %d", tc.termW, got, tc.want)
		}
	}
}

// TestComposeContentHeight pins the description-sizing rule: at
// least composeMinDescriptionContent rows, otherwise one row per \n.
func TestComposeContentHeight(t *testing.T) {
	cases := []struct {
		desc string
		want int
	}{
		{"", composeMinDescriptionContent},
		{"one line", composeMinDescriptionContent},
		{"three\nshort\nlines", composeMinDescriptionContent},
		// Once typed content exceeds the minimum, the panel grows.
		{strings.Repeat("x\n", composeMinDescriptionContent), composeMinDescriptionContent + 1},
		{strings.Repeat("x\n", 20), 21},
	}
	for _, tc := range cases {
		if got := composeContentHeight(tc.desc); got != tc.want {
			t.Errorf("composeContentHeight(%q) = %d, want %d", tc.desc, got, tc.want)
		}
	}
}

// TestComposeRects pins the geometry: summary sits at the top of
// the centred panel, description starts at summary.bottom+1 and
// extends past the centred box by composeSummaryViewHeight rows
// (lazygit's offset trick \u2014 see ResizeCommitMessagePanels).
func TestComposeRects(t *testing.T) {
	const termW, termH = 200, 60
	_, _, sx0, sy0, sx1, sy1, dx0, dy0, dx1, dy1 := composeRects(termW, termH, "")

	// Summary outer height must be exactly composeSummaryViewHeight.
	if h := sy1 - sy0 + 1; h != composeSummaryViewHeight {
		t.Errorf("summary outer height = %d, want %d", h, composeSummaryViewHeight)
	}
	// Description starts on the row right after summary's bottom frame.
	if dy0 != sy1+1 {
		t.Errorf("description y0 = %d, want %d (= summary y1+1)", dy0, sy1+1)
	}
	// Summary and description share x-extents (stacked, not side-by-side).
	if sx0 != dx0 || sx1 != dx1 {
		t.Errorf("summary x-extent (%d,%d) must match description (%d,%d)", sx0, sx1, dx0, dx1)
	}
	// Panel is roughly centred horizontally.
	mid := (sx0 + sx1) / 2
	if abs(mid-termW/2) > 2 {
		t.Errorf("panel midpoint x=%d should be near termW/2=%d", mid, termW/2)
	}
	// Panel sits above the bottom edge.
	if dy1 >= termH {
		t.Errorf("description y1 = %d overflows termH=%d", dy1, termH)
	}
}

// TestComposeSummarySubtitle pins the lazygit-style counter format.
// Spaces around the number are intentional \u2014 lazygit's
// getBufferLength returns " %d ", so the subtitle reads e.g. " 12 "
// inside the frame edge.
func TestComposeSummarySubtitle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", " 0 "},
		{"abc", " 3 "},
		// Counts runes, not bytes (\u044f is 2 bytes in UTF-8).
		{"\u044f\u0431\u043b\u043e\u043a\u043e", " 6 "},
	}
	for _, tc := range cases {
		if got := composeSummarySubtitle(tc.in); got != tc.want {
			t.Errorf("composeSummarySubtitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOpenTaskComposeState verifies the open helper records the
// expected popup-state shape.
func TestOpenTaskComposeState(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.openTaskCompose("Hello", "initial summary", "initial desc", func(string, string) error { return nil })
	if k := gu.st.popup.Kind; k != popupTaskCompose {
		t.Fatalf("popup kind = %v, want popupTaskCompose", k)
	}
	if gu.st.popup.Title != "Hello" {
		t.Errorf("title = %q, want %q", gu.st.popup.Title, "Hello")
	}
	if gu.st.popup.Summary != "initial summary" {
		t.Errorf("summary seed wrong: %q", gu.st.popup.Summary)
	}
	if gu.st.popup.Description != "initial desc" {
		t.Errorf("description seed wrong: %q", gu.st.popup.Description)
	}
	if gu.st.popup.ComposeFocus != composeSummary {
		t.Errorf("initial focus = %v, want composeSummary", gu.st.popup.ComposeFocus)
	}
	if gu.st.popup.OnComposeAccept == nil {
		t.Errorf("OnComposeAccept not recorded")
	}
}

// TestTaskComposeToggle pins the Tab-toggle behaviour: focus flips
// between summary and description and is a no-op when the active
// popup is something else.
func TestTaskComposeToggle(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.openTaskCompose("", "", "", func(string, string) error { return nil })
	if gu.st.popup.ComposeFocus != composeSummary {
		t.Fatalf("seed focus wrong")
	}
	_ = gu.taskComposeToggle(nil, nil)
	if gu.st.popup.ComposeFocus != composeDescription {
		t.Fatalf("after 1 toggle: focus = %v, want composeDescription", gu.st.popup.ComposeFocus)
	}
	_ = gu.taskComposeToggle(nil, nil)
	if gu.st.popup.ComposeFocus != composeSummary {
		t.Fatalf("after 2 toggles: focus = %v, want composeSummary", gu.st.popup.ComposeFocus)
	}

	// Toggle on a different popup kind is a no-op.
	gu.st.popup = popupState{Kind: popupMenu, ComposeFocus: composeSummary}
	_ = gu.taskComposeToggle(nil, nil)
	if gu.st.popup.ComposeFocus != composeSummary {
		t.Errorf("toggle should be a no-op on non-compose popup")
	}
}

// TestLayoutTaskComposePersistsTypedText drives a real headless
// gocui, opens the compose popup, types into BOTH panes, then runs
// another layout pass. The typed text must survive \u2014 same view-
// lifetime invariant that fixed the single-pane prompt. If a future
// refactor reintroduces delete-on-every-pass for the compose views,
// this test fails.
func TestLayoutTaskComposePersistsTypedText(t *testing.T) {
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
	gu.openTaskCompose("New task", "", "", func(string, string) error { return nil })
	gu.layoutPopup(g, 120, 40)

	summV, err := g.View(winTaskComposeSummary)
	if err != nil {
		t.Fatalf("summary view missing after first layout: %v", err)
	}
	descV, err := g.View(winTaskComposeDescription)
	if err != nil {
		t.Fatalf("description view missing after first layout: %v", err)
	}
	if !summV.Editable || !descV.Editable {
		t.Fatalf("both compose views must be Editable")
	}

	// Type into both panes (mimicking what SimpleEditor does per key).
	summV.TextArea.TypeString("hello")
	summV.RenderTextArea()
	descV.TextArea.TypeString("line one\nline two")
	descV.RenderTextArea()

	// Force the second layout pass (this is what every keystroke does
	// via g.Update \u2192 flush \u2192 layout).
	gu.layoutPopup(g, 120, 40)

	summV2, _ := g.View(winTaskComposeSummary)
	descV2, _ := g.View(winTaskComposeDescription)
	if summV2 != summV {
		t.Errorf("summary view re-created across layout passes \u2014 typed text would be lost")
	}
	if descV2 != descV {
		t.Errorf("description view re-created across layout passes")
	}
	if got := summV2.TextArea.GetContent(); got != "hello" {
		t.Errorf("summary content lost across layout pass: got %q", got)
	}
	if got := descV2.TextArea.GetContent(); got != "line one\nline two" {
		t.Errorf("description content lost across layout pass: got %q", got)
	}
}

// TestLayoutTaskComposeFocusRoutesByPane verifies that the layout
// switches g.currentView to match ComposeFocus on every pass \u2014 so
// taskComposeToggle actually changes which pane the next keystroke
// goes into.
func TestLayoutTaskComposeFocusRoutesByPane(t *testing.T) {
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
	gu.openTaskCompose("", "", "", func(string, string) error { return nil })
	gu.layoutPopup(g, 120, 40)

	if cv := g.CurrentView(); cv == nil || cv.Name() != winTaskComposeSummary {
		t.Fatalf("initial focus must be summary, got %v", currentViewName(g))
	}

	_ = gu.taskComposeToggle(nil, nil)
	gu.layoutPopup(g, 120, 40)
	if cv := g.CurrentView(); cv == nil || cv.Name() != winTaskComposeDescription {
		t.Fatalf("after toggle focus must be description, got %v", currentViewName(g))
	}

	_ = gu.taskComposeToggle(nil, nil)
	gu.layoutPopup(g, 120, 40)
	if cv := g.CurrentView(); cv == nil || cv.Name() != winTaskComposeSummary {
		t.Fatalf("after second toggle focus must be summary again, got %v", currentViewName(g))
	}
}

// TestTaskComposeConfirmReadsBothPanes verifies the confirm handler
// reads the typed text out of both views and feeds it to the
// OnComposeAccept callback (NOT from popupState.Summary/Description
// \u2014 those are only initial seeds, the view's TextArea is the
// source of truth, mirroring lazygit's HandleCommitConfirm).
func TestTaskComposeConfirmReadsBothPanes(t *testing.T) {
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
	var gotSummary, gotDescription string
	var fired int
	gu.openTaskCompose("", "", "", func(s, d string) error {
		fired++
		gotSummary, gotDescription = s, d
		return nil
	})
	gu.layoutPopup(g, 120, 40)

	summV, _ := g.View(winTaskComposeSummary)
	descV, _ := g.View(winTaskComposeDescription)
	summV.TextArea.TypeString("Refactor auth")
	descV.TextArea.TypeString("Split token validation\nand JWT signing")

	if err := gu.taskComposeConfirm(nil, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if fired != 1 {
		t.Fatalf("OnComposeAccept fired %d times, want 1", fired)
	}
	if gotSummary != "Refactor auth" {
		t.Errorf("got summary %q, want %q", gotSummary, "Refactor auth")
	}
	if gotDescription != "Split token validation\nand JWT signing" {
		t.Errorf("got description %q, want %q", gotDescription, "Split token validation\nand JWT signing")
	}
	if k := gu.st.popup.Kind; k != popupNone {
		t.Errorf("popup not cleared after confirm: kind=%v", k)
	}
}

// TestLayoutTaskComposeSwitchToOtherPopupCleansUp pins that when
// the active popup changes from compose to (say) menu, BOTH compose
// views are deleted on the next layout pass. Otherwise stale frames
// would linger under the new popup.
func TestLayoutTaskComposeSwitchToOtherPopupCleansUp(t *testing.T) {
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
	gu.openTaskCompose("", "", "", func(string, string) error { return nil })
	gu.layoutPopup(g, 120, 40)
	if _, err := g.View(winTaskComposeSummary); err != nil {
		t.Fatalf("summary view missing after first layout")
	}

	// Close compose, open menu, layout again \u2014 compose views gone.
	gu.st.popup = popupState{}
	gu.openMenu("pick", []string{"a", "b"}, func(int) error { return nil })
	gu.layoutPopup(g, 120, 40)

	for _, name := range []string{winTaskComposeSummary, winTaskComposeDescription} {
		if _, err := g.View(name); err == nil {
			t.Errorf("compose view %q must be deleted after switching to menu", name)
		}
	}
	if _, err := g.View(winPopupMenu); err != nil {
		t.Errorf("menu view should exist after switching: %v", err)
	}
}

// --- helpers ----------------------------------------------------------------

func currentViewName(g *gocui.Gui) string {
	if cv := g.CurrentView(); cv != nil {
		return cv.Name()
	}
	return ""
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
