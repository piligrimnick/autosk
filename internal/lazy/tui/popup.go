package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/theme"
)

// openMenu pushes a Menu popup with the given title, lines, and the
// onSelect handler. Esc cancels; Enter calls onSelect(cursor).
func (gu *Gui) openMenu(title string, lines []string, onSelect func(int) error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:     popupMenu,
			Title:    title,
			Lines:    lines,
			Cursor:   0,
			OnSelect: onSelect,
		}
	})
	gu.requestRedraw()
}

// openConfirm pushes a Confirm popup; onAccept runs on y/Enter.
func (gu *Gui) openConfirm(prompt string, onAccept func() error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:     popupConfirm,
			Title:    prompt,
			OnAccept: func(string) error { return onAccept() },
		}
	})
	gu.requestRedraw()
}

// openPrompt pushes a Prompt popup; onAccept gets the typed value.
func (gu *Gui) openPrompt(prompt, initial string, onAccept func(string) error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:     popupPrompt,
			Title:    prompt,
			Input:    initial,
			OnAccept: onAccept,
		}
	})
	gu.requestRedraw()
}

// openSingleCompose pushes the one-pane multi-line editor used for
// task comments and metadata. Carries the same simple-contract
// semantics as openPrompt (Input seeds the initial value, OnAccept
// fires with the typed text on submit) — the differences are
// layout (a tall textarea, not a one-line strip) and the submit
// chords (Ctrl-S / Alt-Enter; plain Enter falls through to the
// editor and inserts "\n").
//
// hint is a short context label drawn alongside the always-on
// submit/cancel keybinding hint; pass "" for no hint.
func (gu *Gui) openSingleCompose(title, hint, initial string, onAccept func(string) error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:     popupSingleCompose,
			Title:    title,
			Hint:     hint,
			Input:    initial,
			OnAccept: onAccept,
		}
	})
	gu.requestRedraw()
}

// singleComposeConfirm reads the typed text out of the single-compose
// view and fires OnAccept with it. Symmetric to taskComposeConfirm,
// just without the summary pane. Bound on Ctrl+S and Alt+Enter; Esc
// dismisses without invoking OnAccept.
func (gu *Gui) singleComposeConfirm(*gocui.Gui, *gocui.View) error {
	var accept func(string) error
	var text string
	if v, err := gu.g.View(winSingleCompose); err == nil && v != nil {
		text = strings.TrimRight(v.TextArea.GetContent(), "\n")
	}
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupSingleCompose {
			return
		}
		accept = gu.st.popup.OnAccept
		gu.st.popup = popupState{}
	})
	if accept != nil {
		return accept(text)
	}
	return nil
}

// openTaskCompose pushes the two-pane lazygit-style commit editor
// used to create a task. The accept callback fires with the summary
// (single line) and the multi-line description; either may be empty
// (the caller decides whether an empty summary is a no-op or an
// error). Esc / popupClose dismisses the popup.
func (gu *Gui) openTaskCompose(title, initialSummary, initialDescription string, onAccept func(summary, description string) error) {
	if title == "" {
		title = "New task"
	}
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:            popupTaskCompose,
			Title:           title,
			Summary:         initialSummary,
			Description:     initialDescription,
			ComposeFocus:    composeSummary,
			OnComposeAccept: onAccept,
		}
	})
	gu.requestRedraw()
}

// taskComposeToggle flips focus between the summary and description
// panes of the active compose popup. No-op when the active popup is
// something else. Bound on Tab in both compose views (lazygit
// parity — Universal.TogglePanel).
func (gu *Gui) taskComposeToggle(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupTaskCompose {
			return
		}
		if gu.st.popup.ComposeFocus == composeSummary {
			gu.st.popup.ComposeFocus = composeDescription
		} else {
			gu.st.popup.ComposeFocus = composeSummary
		}
	})
	gu.requestRedraw()
	return nil
}

// taskComposeConfirm reads the typed text out of both compose views
// and fires the OnComposeAccept callback registered at openTaskCompose
// time. Bound on Enter (summary only — Enter on description inserts a
// newline via the editor) and on Ctrl+S / Alt+Enter (both views).
//
// The view's TextArea is the source of truth, not popupState.Summary /
// Description: those fields only seed the initial text on first
// layout. Reading from the live TextArea is what lazygit does too
// (Commits.HandleCommitConfirm reads via getCommitSummary /
// getCommitDescription, which proxy to the view).
func (gu *Gui) taskComposeConfirm(*gocui.Gui, *gocui.View) error {
	var accept func(string, string) error
	var summary, description string
	if v, err := gu.g.View(winTaskComposeSummary); err == nil && v != nil {
		summary = strings.TrimRight(v.TextArea.GetContent(), "\n")
	}
	if v, err := gu.g.View(winTaskComposeDescription); err == nil && v != nil {
		description = strings.TrimRight(v.TextArea.GetContent(), "\n")
	}
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupTaskCompose {
			return
		}
		accept = gu.st.popup.OnComposeAccept
		gu.st.popup = popupState{}
	})
	if accept != nil {
		return accept(summary, description)
	}
	return nil
}

// requestRedraw pokes the gocui main loop. Tolerates a nil gui (unit
// tests that drive the state machine without a real screen).
func (gu *Gui) requestRedraw() {
	if gu.g == nil {
		return
	}
	gu.g.Update(func(_ *gocui.Gui) error { return nil })
}

// popupClose dismisses the active popup.
func (gu *Gui) popupClose(*gocui.Gui, *gocui.View) error {
	var cancel func() error
	gu.st.withLock(func() {
		cancel = gu.st.popup.OnCancel
		gu.st.popup = popupState{}
	})
	if cancel != nil {
		return cancel()
	}
	return nil
}

// popupConfirmYes treats y / Y / Enter on a confirm popup.
func (gu *Gui) popupConfirmYes(*gocui.Gui, *gocui.View) error {
	var fn func(string) error
	gu.st.withLock(func() {
		fn = gu.st.popup.OnAccept
		gu.st.popup = popupState{}
	})
	if fn != nil {
		return fn("yes")
	}
	return nil
}

// popupAccept handles Enter on Menu / Prompt.
func (gu *Gui) popupAccept(_ *gocui.Gui, v *gocui.View) error {
	var (
		kind  popupKind
		sel   func(int) error
		acc   func(string) error
		cur   int
		input string
	)
	if v != nil {
		input = strings.TrimRight(v.Buffer(), "\n")
	}
	gu.st.withLock(func() {
		kind = gu.st.popup.Kind
		sel = gu.st.popup.OnSelect
		acc = gu.st.popup.OnAccept
		cur = gu.st.popup.Cursor
		if kind == popupPrompt {
			gu.st.popup.Input = input
		}
		gu.st.popup = popupState{}
	})
	switch kind {
	case popupMenu:
		if sel != nil {
			return sel(cur)
		}
	case popupPrompt:
		if acc != nil {
			return acc(input)
		}
	}
	return nil
}

// popupCursor moves the menu selection.
func (gu *Gui) popupCursor(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			if gu.st.popup.Kind != popupMenu {
				return
			}
			n := len(gu.st.popup.Lines)
			if n == 0 {
				return
			}
			gu.st.popup.Cursor = (gu.st.popup.Cursor + step + n) % n
		})
		return nil
	}
}

// layoutPopup draws whatever popup is active. Centered, with overlap
// over the dashboard / inspector underneath.
//
// Popup-view lifetime is load-bearing for the prompt: the typed text
// lives in the view's TextArea, and gocui's NewView constructor
// reinitialises that TextArea every time SetView has to create a
// fresh view. layout runs on every flush — i.e. once per keypress —
// so deleting + re-creating the popup view on every pass throws away
// the character the editor just inserted. (Symptom: "new task title"
// popup never accumulates text; every keystroke shows as a no-op.)
// Fix: clean up only the popup views that are NOT the active one.
// The active one is preserved across passes so its TextArea survives.
func (gu *Gui) layoutPopup(g *gocui.Gui, w, h int) {
	var (
		kind  popupKind
		title string
		lines []string
		cur   int
		input string
	)
	gu.st.withRLock(func() {
		kind = gu.st.popup.Kind
		title = gu.st.popup.Title
		lines = gu.st.popup.Lines
		cur = gu.st.popup.Cursor
		input = gu.st.popup.Input
	})
	activeSet := map[string]bool{}
	switch kind {
	case popupMenu:
		activeSet[winPopupMenu] = true
	case popupConfirm:
		activeSet[winPopupConfirm] = true
	case popupPrompt:
		activeSet[winPopupPrompt] = true
	case popupTaskCompose:
		activeSet[winTaskComposeSummary] = true
		activeSet[winTaskComposeDescription] = true
	case popupSingleCompose:
		activeSet[winSingleCompose] = true
	}
	for _, name := range allPopupWindows {
		if activeSet[name] {
			continue
		}
		_ = g.DeleteView(name)
	}
	switch kind {
	case popupMenu:
		gu.drawPopup(g, winPopupMenu, w, h, title, renderMenuBody(lines, cur))
		if _, err := g.SetCurrentView(winPopupMenu); err != nil && !isUnknownView(err) {
			return
		}
	case popupConfirm:
		gu.drawPopup(g, winPopupConfirm, w, h, title, "[y]es / [n]o / [Esc] cancel")
		if _, err := g.SetCurrentView(winPopupConfirm); err != nil && !isUnknownView(err) {
			return
		}
	case popupPrompt:
		v := gu.drawPopup(g, winPopupPrompt, w, h, title, "")
		if v != nil {
			v.Editable = true
			v.Wrap = false
			v.Editor = gocui.DefaultEditor
			// Preserve initial.
			if v.Buffer() == "" && input != "" {
				_, _ = v.Write([]byte(input))
			}
		}
		if _, err := g.SetCurrentView(winPopupPrompt); err != nil && !isUnknownView(err) {
			return
		}
	case popupTaskCompose:
		gu.layoutTaskCompose(g, w, h, title)
	case popupSingleCompose:
		var hint string
		gu.st.withRLock(func() { hint = gu.st.popup.Hint })
		gu.layoutSingleCompose(g, w, h, title, hint, input)
	}
}

// composePanelMaxWidth is the cap on the compose popup's outer
// width. Lazygit uses 100 for the commit-message panel (or wrap
// width + 25 when auto-wrap is on); autosk has no equivalent of
// auto-wrap so we use the plain 100-cell ceiling.
const composePanelMaxWidth = 100

// composeSummaryViewHeight is the OUTER height (including frame) of
// the summary pane. 3 = top frame + 1 content line + bottom frame.
// Same magic number lazygit hard-codes as summaryViewHeight.
const composeSummaryViewHeight = 3

// composeMinDescriptionContent is the minimum content-height (rows,
// EXCLUDING frame) the description pane allocates even when empty,
// so the popup doesn't visually shrink to a sliver before the user
// has typed anything. Lazygit uses minHeight=7 for the same reason.
const composeMinDescriptionContent = 7

// layoutTaskCompose draws the two-pane compose popup at the
// dimensions lazygit uses for the commit-message panel
// (pkg/gui/controllers/helpers/confirmation_helper.go::
// ResizeCommitMessagePanels):
//
//   - width = min(4*w/7, 100), floored to min(w-2, 80)
//   - description content height = max(7, line-count of typed body)
//   - panelHeight = contentHeight + 2 (frame), capped at 3*h/4
//   - centred horizontally and vertically
//   - summary: top, OUTER height 3 (1 content line + 2 frame rows)
//   - description: stacked below, expanding past the centred box
//     by composeSummaryViewHeight rows (lazygit's exact placement)
//
// Both views are editable and use SimpleEditor. The summary view's
// Enter is intercepted by a view-scoped keybinding (taskComposeConfirm)
// so the editor never sees Enter and can't insert a \n. The
// description view has NO Enter binding — the keystroke falls
// through to SimpleEditor which inserts "\n" verbatim. Submitting
// from the description requires Ctrl+S or Alt+Enter (both bound
// view-scoped).
//
// Like the single-pane prompt, view lifetime is load-bearing: the
// typed text lives in the view's TextArea, which gocui's NewView
// constructor reinitialises every time SetView creates a fresh
// view. layoutPopup keeps both compose views alive across layout
// passes (see the activeSet logic) so keystrokes don't get eaten.
func (gu *Gui) layoutTaskCompose(g *gocui.Gui, w, h int, title string) {
	var (
		initialSummary string
		initialDesc    string
		focus          composePane
	)
	gu.st.withRLock(func() {
		initialSummary = gu.st.popup.Summary
		initialDesc = gu.st.popup.Description
		focus = gu.st.popup.ComposeFocus
	})

	// Read the live description text to size the panel — lazygit
	// resizes on every keystroke so the popup grows with the body.
	descContent := ""
	if v, err := g.View(winTaskComposeDescription); err == nil && v != nil {
		descContent = v.TextArea.GetContent()
	}
	summaryContent := initialSummary
	if v, err := g.View(winTaskComposeSummary); err == nil && v != nil {
		// Once the view exists, its TextArea is the source of truth.
		summaryContent = v.TextArea.GetContent()
	}

	_, _, sx0, sy0, sx1, sy1, dx0, dy0, dx1, dy1 := composeRects(w, h, descContent)

	// Border-color rule mirrors the dashboard's focused-panel
	// affordance (layout.go): the pane with input focus gets the
	// PopupBox accent on both frame and title; the inactive pane
	// drops back to the default fg so the user can see at a glance
	// which pane will receive their keystrokes. Without this both
	// panes were tinted purple unconditionally and the cursor was
	// the only visual focus cue — invisible on terminals that don't
	// show one for editable views.
	activeColor := theme.Active().PopupBox.Gocui()
	inactiveColor := gocui.ColorDefault

	var summFrameColor, descFrameColor gocui.Attribute
	if focus == composeSummary {
		summFrameColor = activeColor
		descFrameColor = inactiveColor
	} else {
		summFrameColor = inactiveColor
		descFrameColor = activeColor
	}

	// Summary view (top).
	summV, err := g.SetView(winTaskComposeSummary, sx0, sy0, sx1, sy1, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	if summV != nil {
		summV.Title = title
		summV.Frame = true
		summV.FrameRunes = roundedFrameRunes
		summV.FrameColor = summFrameColor
		summV.TitleColor = summFrameColor
		summV.Editable = true
		summV.Wrap = false
		summV.Editor = gocui.DefaultEditor
		// Seed the initial value only on the very first layout pass
		// (TextArea is empty). After that the user owns it. We seed
		// via TextArea.TypeString + RenderTextArea so the editor's
		// view of the buffer matches v.lines — a plain v.Write would
		// only paint v.lines and the next keystroke would wipe the
		// seed when RenderTextArea fired off an empty TextArea.
		if summV.TextArea.GetContent() == "" && initialSummary != "" {
			summV.TextArea.TypeString(initialSummary)
			summV.RenderTextArea()
			summaryContent = initialSummary
		}
		// Lazygit-style character counter in the subtitle slot.
		summV.Subtitle = composeSummarySubtitle(summaryContent)
	}

	// Description view (below summary).
	descV, err := g.SetView(winTaskComposeDescription, dx0, dy0, dx1, dy1, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	if descV != nil {
		descV.Title = "Description"
		descV.Frame = true
		descV.FrameRunes = roundedFrameRunes
		descV.FrameColor = descFrameColor
		descV.TitleColor = descFrameColor
		descV.Editable = true
		descV.Wrap = false
		descV.Editor = gocui.DefaultEditor

		// Hint rendering note: gocui's drawListFooter no-ops when
		// v.lines is empty (see /Users/mentor/me/dev/gocui/gui.go
		// drawListFooter), and an editable view with an empty
		// TextArea has v.lines == nil. So a freshly-opened compose
		// popup with an empty description would silently swallow
		// v.Footer until the user typed one character — which is
		// exactly when the user MOST needs the hint, before they
		// type. drawSubtitle has no such guard (it's drawn straight
		// onto the top-frame row), so we render BOTH hints in the
		// subtitle slot. The toggle hint is always-on (the user's
		// "отображается всегда" requirement). The submit hint
		// appends only while the description pane has focus, the
		// equivalent of lazygit's CommitDescriptionFooter — just
		// rendered above the box instead of below it, because that's
		// the only slot gocui actually paints on empty content.
		descV.Subtitle = composeDescriptionSubtitle(focus == composeDescription)

		if descV.TextArea.GetContent() == "" && initialDesc != "" {
			descV.TextArea.TypeString(initialDesc)
			descV.RenderTextArea()
		}
	}

	// Route focus.
	var focusName string
	if focus == composeDescription {
		focusName = winTaskComposeDescription
	} else {
		focusName = winTaskComposeSummary
	}
	if _, err := g.SetCurrentView(focusName); err != nil && !isUnknownView(err) {
		return
	}
}

// layoutSingleCompose draws the one-pane multi-line popup used by
// the comment and metadata flows. Reuses the same panel-width
// formula as the two-pane compose (composePanelWidth) so the popup
// has visual parity with `n new`, and the same view-lifetime
// invariant: the view is preserved across layout passes via
// layoutPopup's activeSet so the TextArea's typed content survives
// each keystroke (gocui's NewView reinstalls a fresh TextArea
// otherwise).
//
// Height = composeContentHeight(initial) + 2 (frame), capped at
// 3/4 of the terminal — same envelope as the description pane in
// the two-pane compose so a freshly-opened popup with empty content
// is tall enough to type into and a popup pre-filled with long
// pretty-printed JSON grows to fit.
//
// Editor: gocui.DefaultEditor. Enter inserts "\n" (no view-scoped
// override on plain Enter); Ctrl-S and Alt-Enter are bound to
// singleComposeConfirm, Esc to popupClose.
func (gu *Gui) layoutSingleCompose(g *gocui.Gui, termW, termH int, title, hint, initial string) {
	// Read the live content to size the panel — same trick as the
	// two-pane compose, so a pretty-printed metadata dump grows the
	// box past the 7-row minimum without the user having to scroll.
	content := initial
	if v, err := g.View(winSingleCompose); err == nil && v != nil {
		content = v.TextArea.GetContent()
	}

	panelWidth := composePanelWidth(termW)
	contentHeight := composeContentHeight(content)
	panelHeight := contentHeight + 2
	if panelHeight > termH*3/4 {
		panelHeight = termH * 3 / 4
	}
	if panelHeight < 5 {
		panelHeight = 5
	}
	x0 := termW/2 - panelWidth/2
	y0 := termH/2 - panelHeight/2 - panelHeight%2
	x1 := termW/2 + panelWidth/2 - 1
	y1 := y0 + panelHeight - 1

	v, err := g.SetView(winSingleCompose, x0, y0, x1, y1, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	if v == nil {
		return
	}
	v.Title = title
	v.Frame = true
	v.FrameRunes = roundedFrameRunes
	v.FrameColor = theme.Active().PopupBox.Gocui()
	v.TitleColor = theme.Active().PopupBox.Gocui()
	v.Editable = true
	v.Wrap = false
	v.Editor = gocui.DefaultEditor
	v.Subtitle = composeSingleSubtitle(hint)
	// Seed initial on the first layout pass only: type the bytes
	// directly into the view's TextArea (NOT v.Write — v.Write only
	// populates v.lines, which the editor doesn't see), then sync
	// to v.lines via RenderTextArea so the user actually sees the
	// seed on the first frame. The TextArea-empty guard is the
	// equivalent of the single-pane prompt's Buffer()-empty guard:
	// after the first layout the user owns the textarea.
	if v.TextArea.GetContent() == "" && initial != "" {
		v.TextArea.TypeString(initial)
		v.RenderTextArea()
	}
	if _, err := g.SetCurrentView(winSingleCompose); err != nil && !isUnknownView(err) {
		return
	}
}

// composeSingleSubtitle renders the always-on submit / cancel hint
// for popupSingleCompose, optionally prefixed by a caller-supplied
// context label (e.g. "markdown ok" or "JSON object").
func composeSingleSubtitle(hint string) string {
	base := "<c-s>/<a-enter> submit · <esc> cancel"
	if hint == "" {
		return " " + base + " "
	}
	return " " + hint + " · " + base + " "
}

// composePanelWidth implements lazygit's getPopupPanelWidth (with
// maxWidth = composePanelMaxWidth): start at 4/7 of the terminal,
// cap at maxWidth, then floor to min(termWidth-2, 80) so the popup
// stays usable on narrow terminals.
func composePanelWidth(termWidth int) int {
	pw := 4 * termWidth / 7
	if pw > composePanelMaxWidth {
		pw = composePanelMaxWidth
	}
	const minWidth = 80
	if pw < minWidth {
		pw = minWidth
		if pw > termWidth-2 {
			pw = termWidth - 2
		}
	}
	if pw < 1 {
		pw = 1
	}
	return pw
}

// composeContentHeight returns the description content height in
// rows, given the typed body. min composeMinDescriptionContent.
func composeContentHeight(desc string) int {
	lines := 1
	if desc != "" {
		lines = strings.Count(desc, "\n") + 1
	}
	if lines < composeMinDescriptionContent {
		lines = composeMinDescriptionContent
	}
	return lines
}

// composeRects returns the panel size + the two child-view
// rectangles for the compose popup. Pure function so the layout
// math is testable without a gocui screen.
//
// Returns: panelWidth, panelHeight, summary(x0,y0,x1,y1),
// description(x0,y0,x1,y1). The panel is centred horizontally and
// vertically (lazygit getPopupPanelDimensionsAux). The description
// view extends past the centred box by composeSummaryViewHeight
// rows on its bottom edge — the same trick lazygit uses to keep
// the description's content area sized purely by
// composeContentHeight without bleeding into the summary.
func composeRects(termW, termH int, desc string) (pw, ph, sx0, sy0, sx1, sy1, dx0, dy0, dx1, dy1 int) {
	panelWidth := composePanelWidth(termW)
	contentHeight := composeContentHeight(desc)
	panelHeight := contentHeight + 2 // outer frame
	if panelHeight > termH*3/4 {
		panelHeight = termH * 3 / 4
	}
	if panelHeight < 5 {
		panelHeight = 5
	}

	x0 := termW/2 - panelWidth/2
	y0 := termH/2 - panelHeight/2 - panelHeight%2
	x1 := termW/2 + panelWidth/2 - 1
	y1 := termH/2 + panelHeight/2 - 1

	sx0, sy0, sx1, sy1 = x0, y0, x1, y0+composeSummaryViewHeight-1
	dx0, dy0, dx1, dy1 = x0, y0+composeSummaryViewHeight, x1, y1+composeSummaryViewHeight
	return panelWidth, panelHeight, sx0, sy0, sx1, sy1, dx0, dy0, dx1, dy1
}

// composeSummarySubtitle renders lazygit's `" N "` character-count
// subtitle for the summary pane.
func composeSummarySubtitle(summary string) string {
	return " " + strconv.Itoa(utf8.RuneCountInString(summary)) + " "
}

// composeDescriptionSubtitle renders the help text shown on the
// description pane's top frame (gocui drawSubtitle slot,
// right-aligned). Always shows the toggle hint — the focus-toggle
// is the one binding the user can't discover by trying things,
// since Tab is otherwise used by the dashboard panel cycler. When
// the description pane has focus we append the submit-keys hint
// (Enter inserts a newline there, so the user needs to be told how
// to actually save).
func composeDescriptionSubtitle(focused bool) string {
	if focused {
		// Submit comes first when the user is actually IN the
		// description pane — it's the action they're most likely
		// looking for the keybinding to. Tab-toggle is secondary
		// once they've already navigated to the right pane.
		return " <c-s>/<a-enter> submit · <tab> toggle "
	}
	return " <tab> toggle "
}

func (gu *Gui) drawPopup(g *gocui.Gui, name string, w, h int, title, body string) *gocui.View {
	pw, ph := minPopup(w, h, body)
	x0 := (w - pw) / 2
	y0 := (h - ph) / 2
	v, err := g.SetView(name, x0, y0, x0+pw, y0+ph, 0)
	if err != nil && !isUnknownView(err) {
		return nil
	}
	if v == nil {
		return nil
	}
	v.Title = title
	v.Frame = true
	v.FrameRunes = roundedFrameRunes
	// Popup frame uses the palette's PopupBox slot — picked to be a
	// neighbour-but-not-twin of Accent so the popup chrome doesn't get
	// visually confused with the cursor-row highlight underneath it.
	v.FrameColor = theme.Active().PopupBox.Gocui()
	if body != "" {
		v.Clear()
		_, _ = v.Write([]byte(body))
	}
	return v
}

func minPopup(w, h int, body string) (int, int) {
	pw := 60
	ph := 8 + strings.Count(body, "\n")
	if pw > w-4 {
		pw = w - 4
	}
	if ph > h-4 {
		ph = h - 4
	}
	if ph < 5 {
		ph = 5
	}
	return pw, ph
}

func renderMenuBody(lines []string, cur int) string {
	var b strings.Builder
	for i, l := range lines {
		if i == cur {
			b.WriteString("▶ " + styleAccent.Render(l) + "\n")
		} else {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}
