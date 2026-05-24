package tui

import (
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/theme"
)

// TestComposeBorderHighlightFollowsFocus pins the bug-1 fix: the
// pane with input focus must get the PopupBox accent on both
// FrameColor and TitleColor; the inactive pane drops to
// gocui.ColorDefault so it visually blends with the rest of the
// chrome (matches the dashboard's focused-panel affordance in
// layout.go). Before the fix both panes were tinted unconditionally
// and the active pane was indistinguishable from the inactive one.
func TestComposeBorderHighlightFollowsFocus(t *testing.T) {
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

	activeColor := theme.Active().PopupBox.Gocui()
	defaultColor := gocui.ColorDefault

	// Initial: summary has focus → summary highlighted, description dimmed.
	gu.layoutPopup(g, 120, 40)
	summV, _ := g.View(winTaskComposeSummary)
	descV, _ := g.View(winTaskComposeDescription)

	if summV.FrameColor != activeColor {
		t.Errorf("focused summary FrameColor = %v, want activeColor (%v)", summV.FrameColor, activeColor)
	}
	if summV.TitleColor != activeColor {
		t.Errorf("focused summary TitleColor = %v, want activeColor", summV.TitleColor)
	}
	if descV.FrameColor != defaultColor {
		t.Errorf("inactive description FrameColor = %v, want ColorDefault", descV.FrameColor)
	}
	if descV.TitleColor != defaultColor {
		t.Errorf("inactive description TitleColor = %v, want ColorDefault", descV.TitleColor)
	}

	// Toggle: now description is focused. Colors must flip.
	_ = gu.taskComposeToggle(nil, nil)
	gu.layoutPopup(g, 120, 40)
	summV, _ = g.View(winTaskComposeSummary)
	descV, _ = g.View(winTaskComposeDescription)

	if summV.FrameColor != defaultColor {
		t.Errorf("inactive summary FrameColor = %v, want ColorDefault", summV.FrameColor)
	}
	if summV.TitleColor != defaultColor {
		t.Errorf("inactive summary TitleColor = %v, want ColorDefault", summV.TitleColor)
	}
	if descV.FrameColor != activeColor {
		t.Errorf("focused description FrameColor = %v, want activeColor", descV.FrameColor)
	}
	if descV.TitleColor != activeColor {
		t.Errorf("focused description TitleColor = %v, want activeColor", descV.TitleColor)
	}
}

// TestComposeDescriptionSubtitleFocusDependent pins bugs 3 and 4:
// the description's top-border subtitle ALWAYS contains the toggle
// hint, AND additionally lists the submit keys when description has
// focus. Together those two strings replace the lazygit
// CommitDescriptionSubTitle + CommitDescriptionFooter pair.
//
// We can't use gocui's drawListFooter for the submit hint because
// it no-ops on empty editable views (drawListFooter checks
// len(v.lines) == 0 and bails) \u2014 the description starts empty,
// so the user would never see the hint before they typed. Subtitle
// has no such guard, hence we render both hints there.
func TestComposeDescriptionSubtitleFocusDependent(t *testing.T) {
	// Pure-function check first \u2014 the helper that drives the
	// subtitle text. Pins the exact strings so a future copy edit
	// doesn't silently remove the keybinding hints the user can't
	// otherwise discover.
	always := composeDescriptionSubtitle(false)
	focused := composeDescriptionSubtitle(true)

	if !strings.Contains(always, "<tab>") {
		t.Errorf("subtitle (summary focused) must mention <tab>: %q", always)
	}
	if strings.Contains(always, "submit") {
		t.Errorf("subtitle (summary focused) must NOT mention submit keys: %q", always)
	}
	if !strings.Contains(focused, "<tab>") {
		t.Errorf("subtitle (description focused) must still mention <tab>: %q", focused)
	}
	for _, want := range []string{"<ctrl+s>", "submit"} {
		if !strings.Contains(focused, want) {
			t.Errorf("subtitle (description focused) must mention %q: %q", want, focused)
		}
	}
	// Order matters: when the user is already IN the description
	// pane, the submit hint is the discovery they actually need; the
	// toggle hint is secondary. So submit comes first.
	if si, ti := strings.Index(focused, "submit"), strings.Index(focused, "toggle"); si == -1 || ti == -1 || si >= ti {
		t.Errorf("subtitle (description focused): submit hint must come before toggle hint, got %q", focused)
	}

	// And the same assertions through layoutTaskCompose so a future
	// refactor that wires the subtitle from somewhere else is still
	// caught.
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
	descV, _ := g.View(winTaskComposeDescription)
	if !strings.Contains(descV.Subtitle, "<tab>") {
		t.Errorf("layout: summary-focused subtitle missing <tab>: %q", descV.Subtitle)
	}
	if strings.Contains(descV.Subtitle, "submit") {
		t.Errorf("layout: summary-focused subtitle should NOT mention submit: %q", descV.Subtitle)
	}

	_ = gu.taskComposeToggle(nil, nil)
	gu.layoutPopup(g, 120, 40)
	descV, _ = g.View(winTaskComposeDescription)
	for _, want := range []string{"<tab>", "<ctrl+s>"} {
		if !strings.Contains(descV.Subtitle, want) {
			t.Errorf("layout: description-focused subtitle missing %q: %q", want, descV.Subtitle)
		}
	}
}

// TestLayoutSyncsCursorVisibility pins the bug-2 fix: the terminal
// cursor (gocui.Gui.Cursor) MUST follow the editability of whatever
// view ends up current after the layout pass. Without this sync,
// text-input views (compose summary, compose description, prompt,
// inspector live input) silently swallow keystrokes \u2014 the text
// IS being inserted into the TextArea, but the user can't see the
// caret so it looks broken.
//
// Drives a real headless gocui through every focus permutation
// that the dashboard can land on:
//
//   - dashboard idle (Tasks panel, non-editable) \u2192 Cursor=false
//   - prompt popup \u2192 Cursor=true (editable view)
//   - compose summary \u2192 Cursor=true
//   - compose description \u2192 Cursor=true (after toggle)
//   - popup closed, back to dashboard \u2192 Cursor=false
func TestLayoutSyncsCursorVisibility(t *testing.T) {
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
	g.SetManagerFunc(gu.layout)

	// Idle dashboard: focused panel is Tasks (non-editable).
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout idle: %v", err)
	}
	if g.Cursor {
		t.Errorf("idle dashboard must hide terminal cursor (Cursor=false), got true")
	}

	// Single-pane prompt popup \u2014 also editable, also needs cursor.
	gu.openPrompt("filter?", "", func(string) error { return nil })
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout prompt: %v", err)
	}
	if !g.Cursor {
		t.Errorf("prompt popup must show terminal cursor (Cursor=true), got false")
	}

	// Close the prompt, open the compose popup on summary.
	gu.st.popup = popupState{}
	gu.openTaskCompose("", "", "", func(string, string) error { return nil })
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout compose summary: %v", err)
	}
	if !g.Cursor {
		t.Errorf("compose summary (editable) must show cursor, got false")
	}

	// Toggle to description \u2014 also editable.
	_ = gu.taskComposeToggle(nil, nil)
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout compose description: %v", err)
	}
	if !g.Cursor {
		t.Errorf("compose description (editable) must show cursor, got false")
	}

	// Close compose. Back to non-editable dashboard.
	gu.st.popup = popupState{}
	if err := gu.layout(g); err != nil {
		t.Fatalf("layout after close: %v", err)
	}
	if g.Cursor {
		t.Errorf("dashboard after popup close must hide cursor, got true")
	}
}
