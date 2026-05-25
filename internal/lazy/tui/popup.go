package tui

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/markdown"
	"autosk/internal/lazy/theme"
)

// replacePopup atomically swaps in `next` for gu.st.popup. When the
// outgoing popup is a popupChangelog with a non-nil
// OnDismissChangelog, the callback is dispatched onto the worker
// pool BEFORE the next render lands so the auto-popup's save-to-
// state.json side effect always fires once — opening another
// popup over the auto-popup counts as implicit acknowledgment of
// the changelog (review R10).
//
// Without this, any open helper that fires while the auto-popup
// is on screen (?, /, :, m, c, …) would silently drop the save
// callback. The first dismiss after that would clear the
// replacement popup via popupClose, which has no notion of the
// changelog contract; last_seen_changelog stays unwritten and the
// auto-popup re-fires on the next start — a popup loop on every
// curious operator.
//
// openChangelog deliberately does NOT route through this helper:
// handleWhatsNew already CARRIES the OnDismissChangelog into the
// upgraded popup so the dismiss-once contract still holds, and
// routing through replacePopup would either double-fire (when the
// new state also carries a callback) or, worse, fire the old
// callback at the same moment a fresh one is installed.
func (gu *Gui) replacePopup(next popupState) {
	var dismiss func() error
	gu.st.withLock(func() {
		if gu.st.popup.Kind == popupChangelog {
			dismiss = gu.st.popup.OnDismissChangelog
		}
		gu.st.popup = next
	})
	if dismiss != nil {
		gu.runDispatch(func() {
			if err := dismiss(); err != nil {
				gu.flashf("warn", "changelog: dismiss callback: %v", err)
			}
		})
	}
}

// openMenu pushes a Menu popup with the given title, lines, and the
// onSelect handler. Esc cancels; Enter calls onSelect(cursor).
//
// Routes through replacePopup so any active popupChangelog auto-
// popup gets its OnDismissChangelog fired before this popup
// replaces it (review R10).
func (gu *Gui) openMenu(title string, lines []string, onSelect func(int) error) {
	gu.replacePopup(popupState{
		Kind:     popupMenu,
		Title:    title,
		Lines:    lines,
		Cursor:   0,
		OnSelect: onSelect,
	})
	gu.requestRedraw()
}

// openIsolationMenu is the workflow-isolation flavour of openMenu. The
// renderer treats popupIsolation identically to popupMenu (same
// layout, same key bindings, same view) so it routes through the same
// drawPopup path; the only practical difference is the Kind field on
// popupState, which lets tests pin the binding without relying on
// title-string heuristics and keeps any future divergence cheap.
//
// Routes through replacePopup (see openMenu for the R10 rationale).
func (gu *Gui) openIsolationMenu(title string, lines []string, onSelect func(int) error) {
	gu.replacePopup(popupState{
		Kind:     popupIsolation,
		Title:    title,
		Lines:    lines,
		Cursor:   0,
		OnSelect: onSelect,
	})
	gu.requestRedraw()
}

// openConfirm pushes a Confirm popup; onAccept runs on y/Enter.
// Routes through replacePopup (see openMenu for the R10 rationale).
func (gu *Gui) openConfirm(prompt string, onAccept func() error) {
	gu.replacePopup(popupState{
		Kind:     popupConfirm,
		Title:    prompt,
		OnAccept: func(string) error { return onAccept() },
	})
	gu.requestRedraw()
}

// openPrompt pushes a Prompt popup; onAccept gets the typed value.
// Routes through replacePopup (see openMenu for the R10 rationale).
func (gu *Gui) openPrompt(prompt, initial string, onAccept func(string) error) {
	gu.replacePopup(popupState{
		Kind:     popupPrompt,
		Title:    prompt,
		Input:    initial,
		OnAccept: onAccept,
	})
	gu.requestRedraw()
}

// openSingleCompose pushes the one-pane multi-line editor used for
// task comments and metadata. Carries the same simple-contract
// semantics as openPrompt (Input seeds the initial value, OnAccept
// fires with the typed text on submit) — the differences are
// layout (a tall textarea, not a one-line strip) and the submit
// chord (Ctrl+S; plain Enter falls through to the editor and
// inserts "\n").
//
// hint is a short context label drawn alongside the always-on
// submit/cancel keybinding hint; pass "" for no hint.
func (gu *Gui) openSingleCompose(title, hint, initial string, onAccept func(string) error) {
	gu.replacePopup(popupState{
		Kind:     popupSingleCompose,
		Title:    title,
		Hint:     hint,
		Input:    initial,
		OnAccept: onAccept,
	})
	gu.requestRedraw()
}

// singleComposeConfirm reads the typed text out of the single-compose
// view and fires OnAccept with it. Symmetric to taskComposeConfirm,
// just without the summary pane. Bound on Ctrl+S; Esc dismisses
// without invoking OnAccept.
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
	gu.replacePopup(popupState{
		Kind:            popupTaskCompose,
		Title:           title,
		Summary:         initialSummary,
		Description:     initialDescription,
		ComposeFocus:    composeSummary,
		OnComposeAccept: onAccept,
	})
	gu.requestRedraw()
}

// openChangelog pushes a popupChangelog state with the given title +
// markdown body. onDismiss (when non-nil) fires exactly once on the
// dismiss handler before the popup is cleared; the auto-popup path
// uses it to stamp last_seen_changelog in ~/.autosk/state.json. The
// ctrl+w re-opener passes nil so dismissal doesn't mutate state.
//
// The body is the raw markdown source. layout renders it through
// internal/lazy/markdown.Render against the popup's inner content
// width on every pass and re-renders on resize (the source is held
// in popupState so a resize doesn't have to thread through the
// caller).
func (gu *Gui) openChangelog(title, body string, onDismiss func() error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:               popupChangelog,
			Title:              title,
			ChangelogSource:    body,
			OnDismissChangelog: onDismiss,
		}
	})
	// Drop any stale bodyCache entry for the popup view: the
	// previous open (if any) may have committed a different body
	// that writeView would otherwise short-circuit against. The
	// view itself is recreated on the next layout pass because
	// popupClose / changelogDismiss runs DeleteView through
	// layoutPopup's activeSet eviction, but writeView's body cache
	// is keyed by view name + dimensions and survives DeleteView.
	gu.invalidateBodyCache(winPopupChangelog)
	gu.requestRedraw()
}

// changelogDismiss is the handler bound on Esc / Enter when the
// changelog popup is active. Clears the popup synchronously so
// the next redraw repaints without the modal, then dispatches
// OnDismissChangelog (when set) onto the worker pool via
// runDispatch (review R8). Routing the dismiss callback through a
// worker matters because the auto-popup path's callback is
// userstate.Save — synchronous file I/O (MkdirAll, OpenFile,
// Write, Close, Rename, Chmod) that on a slow NFS-mounted $HOME
// can take tens to hundreds of milliseconds. Running it on the
// gocui main goroutine would freeze the redraw for that window
// (the rest of the TUI's file/network I/O routes the same way for
// the same reason). Errors from the callback are surfaced via a
// flash from the worker side.
//
// Tests that drive changelogDismiss without a real gocui.Gui set
// gu.dispatch to a synchronous shim; runDispatch's nil-dispatch
// fallback (gu.g.OnWorker) is only used in production.
func (gu *Gui) changelogDismiss(*gocui.Gui, *gocui.View) error {
	var onDismiss func() error
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupChangelog {
			return
		}
		onDismiss = gu.st.popup.OnDismissChangelog
		gu.st.popup = popupState{}
	})
	if onDismiss != nil {
		gu.runDispatch(func() {
			if err := onDismiss(); err != nil {
				gu.flashf("warn", "changelog: dismiss callback: %v", err)
			}
		})
	}
	return nil
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
// newline via the editor) and on Ctrl+S (both views).
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
	case popupMenu, popupIsolation:
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
			if gu.st.popup.Kind != popupMenu && gu.st.popup.Kind != popupIsolation {
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
// over the dashboard underneath.
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
	case popupMenu, popupIsolation:
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
	case popupEnroll:
		activeSet[winEnrollWorkflowList] = true
		activeSet[winEnrollStepList] = true
	case popupCheatsheet:
		activeSet[winPopupCheatsheet] = true
	case popupChangelog:
		activeSet[winPopupChangelog] = true
	}
	for _, name := range allPopupWindows {
		if activeSet[name] {
			continue
		}
		_ = g.DeleteView(name)
	}
	// Pin active popup views to the top of gocui's draw stack so a
	// dashboard view that was deleted-and-recreated mid-popup
	// (e.g. winJobInput allocated when a queued job promotes to
	// running while the popup is open) doesn't end up drawn ON TOP
	// of the popup. gocui flushes views in g.views insertion order;
	// SetViewOnTop reorders to the end without recreating the view,
	// preserving its TextArea / buffer.
	pinOnTop := func(names ...string) {
		for _, n := range names {
			if _, err := g.SetViewOnTop(n); err != nil && !isUnknownView(err) {
				dlog("SetViewOnTop(%s): %v", n, err)
			}
		}
	}
	switch kind {
	case popupMenu, popupIsolation:
		gu.drawPopup(g, winPopupMenu, w, h, title, renderMenuBody(lines, cur))
		pinOnTop(winPopupMenu)
		if _, err := g.SetCurrentView(winPopupMenu); err != nil && !isUnknownView(err) {
			return
		}
	case popupConfirm:
		gu.drawPopup(g, winPopupConfirm, w, h, title, "[y]es / [n]o / [esc] cancel")
		pinOnTop(winPopupConfirm)
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
		pinOnTop(winPopupPrompt)
		if _, err := g.SetCurrentView(winPopupPrompt); err != nil && !isUnknownView(err) {
			return
		}
	case popupTaskCompose:
		gu.layoutTaskCompose(g, w, h, title)
		pinOnTop(winTaskComposeSummary, winTaskComposeDescription)
	case popupSingleCompose:
		var hint string
		gu.st.withRLock(func() { hint = gu.st.popup.Hint })
		gu.layoutSingleCompose(g, w, h, title, hint, input)
		pinOnTop(winSingleCompose)
	case popupEnroll:
		gu.layoutEnrollPicker(g, w, h)
		pinOnTop(winEnrollWorkflowList, winEnrollStepList)
	case popupCheatsheet:
		gu.layoutCheatsheet(g, w, h, title)
		pinOnTop(winPopupCheatsheet)
	case popupChangelog:
		gu.layoutChangelog(g, w, h, title)
		pinOnTop(winPopupChangelog)
		if _, err := g.SetCurrentView(winPopupChangelog); err != nil && !isUnknownView(err) {
			return
		}
	}
}

// layoutChangelog draws the changelog modal at ~80% width / 80%
// height (a generous envelope: the embedded changelog can carry
// many sections of subsection lists and the operator needs a tall
// scroll buffer). The body is rendered through
// internal/lazy/markdown.Render at the popup's inner content
// width so a resize re-flows the markdown to the new width
// instead of clipping the previous render.
//
// View lifetime + body caching: the rendered ANSI blob lives on
// popupState as ChangelogBody; we re-render only when the source
// changes (rare — only on open) or when the content width
// changes (resize). gocui's NewView wipes view state on creation,
// so the layoutPopup activeSet keeps the view alive across
// passes — the rendered ANSI just goes through writeView's body
// cache like every other panel.
//
// Editable=false: the view is read-only; j/k/g/G/ctrl+f/ctrl+b
// scroll bindings are wired in bindKeys against winPopupChangelog
// so the operator can browse the body the same way as the Detail
// pane.
func (gu *Gui) layoutChangelog(g *gocui.Gui, termW, termH int, title string) {
	var (
		source  string
		cached  string
		cachedW int
	)
	gu.st.withRLock(func() {
		source = gu.st.popup.ChangelogSource
		cached = gu.st.popup.ChangelogBody
		cachedW = gu.st.popup.ChangelogWidth
	})

	// Outer rect — 80% of the terminal in both dims, centred, with
	// the now-standard "minPopup" floors so a degenerate small
	// terminal still renders a usable frame.
	pw := termW * 4 / 5
	ph := termH * 4 / 5
	if pw < 60 {
		pw = 60
	}
	if ph < 10 {
		ph = 10
	}
	if pw > termW-4 {
		pw = termW - 4
	}
	if ph > termH-4 {
		ph = termH - 4
	}
	if pw < 10 || ph < 5 {
		return
	}
	x0 := (termW - pw) / 2
	y0 := (termH - ph) / 2
	x1 := x0 + pw
	y1 := y0 + ph

	v, err := g.SetView(winPopupChangelog, x0, y0, x1, y1, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	if v == nil {
		return
	}
	v.Title = title
	v.Subtitle = changelogPopupSubtitle()
	v.Frame = true
	v.FrameRunes = roundedFrameRunes
	v.FrameColor = theme.Active().PopupBox.Gocui()
	v.TitleColor = theme.Active().PopupBox.Gocui()
	v.Editable = false
	v.Wrap = false

	// Inner content width — outer width minus the two frame cells
	// minus one cell of padding on each side. Floor at 1 so a
	// degenerate width doesn't make markdown.Render's wrap math
	// blow up (it falls back to raw text at width≤0 anyway).
	contentW := pw - 4
	if contentW < 1 {
		contentW = 1
	}

	render := cached
	if render == "" || cachedW != contentW {
		render = markdown.Render(source, contentW)
		gu.st.withLock(func() {
			if gu.st.popup.Kind != popupChangelog {
				return
			}
			gu.st.popup.ChangelogBody = render
			gu.st.popup.ChangelogWidth = contentW
		})
	}

	// Route through writeView so the spinner-tick / refresh-loop
	// repaints short-circuit against the body cache and don't re-
	// run gocui's per-byte ANSI-SGR parser on the rendered blob
	// (review R6). writeView keys the cache by view name +
	// dimensions, so a resize—which already invalidates our own
	// ChangelogBody cache via the cachedW!=contentW guard above—
	// also bypasses the writeView cache because the dimensions
	// will differ. The popup’s title is set above on `v` so we
	// pass an empty title here to keep writeView from clobbering
	// it with a stale value.
	gu.writeView(winPopupChangelog, "", render)
}

// changelogPopupSubtitle renders the always-on "scroll + dismiss"
// hint on the changelog popup's top frame. Same shape the
// single-compose popup uses for its submit/cancel hint.
func changelogPopupSubtitle() string {
	return " j/k g/G ctrl+f/ctrl+b scroll · esc/enter dismiss "
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
// from the description requires Ctrl+S (bound view-scoped).
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
// override on plain Enter); Ctrl+S is bound to
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
	// composeContentHeight already enforces a min of
	// composeMinDescriptionContent (7), so panelHeight starts at >=9
	// before the termH*3/4 cap. No explicit floor needed.
	panelHeight := composeContentHeight(content) + 2
	if panelHeight > termH*3/4 {
		panelHeight = termH * 3 / 4
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
	base := "<ctrl+s> submit · <esc> cancel"
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
		return " <ctrl+s> submit · <tab> toggle "
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

// minPopup picks the popup outer dimensions:
//
//   - width = max(60, longest body line + 4 cells of padding),
//     capped at termW-4. The padding accounts for the popup's left
//     frame + the renderMenuBody "▶ "/"  " row prefix; without it
//     the cheatsheet's wider lines (e.g. the `ctrl+r hard refresh
//     ...` row) wrapped or got truncated by gocui's clipping.
//   - height = 8 + body line count, capped at termH-4, floored at 5
//     so the popup is always tall enough to show a frame + title +
//     at least one content row even on very small terminals.
func minPopup(w, h int, body string) (int, int) {
	pw := 60
	if lw := maxLineWidth(body) + 4; lw > pw {
		pw = lw
	}
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

// maxLineWidth returns the visible cell width of the widest
// "\n"-separated line in s.
//
// Goes through lipgloss.Width (NOT utf8.RuneCountInString) so SGR
// escape codes — which renderMenuBody wraps the cursored row in
// via styleAccent.Render — don't leak into the measurement. Without
// this, sizing the popup against a body whose cursored row carries
// a truecolor SGR envelope (~9 "runes" of `\x1b[38;2;R;G;Bm…\x1b[0m`
// padding) would snap the popup ~9 cells wider whenever the cursor
// landed on the widest line and snap back when it moved away — a
// visible visual oscillation on a real TTY (review R8). lipgloss.Width
// strips SGR before counting visible cells AND is rune-aware, so
// multibyte cheatsheet entries still measure by their visible width
// rather than UTF-8 byte length.
func maxLineWidth(s string) int {
	if s == "" {
		return 0
	}
	max := 0
	for _, line := range strings.Split(s, "\n") {
		if n := lipgloss.Width(line); n > max {
			max = n
		}
	}
	return max
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

// ---- cheatsheet popup ---------------------------------------------------
//
// The cheatsheet popup is a lazygit-style sectioned, filterable,
// executable bindings menu. State lives on popupState
// (CheatsheetItems / CheatsheetFilter / CheatsheetCursor /
// CheatsheetFocused). The view (winPopupCheatsheet) is editable so
// printable runes flow through the gocui Editor into the filter;
// non-char keys are routed via view-scoped keybindings (see
// bindingSpecs() under the winPopupCheatsheet block).

// filterCheatsheetItems returns the subset of items that match the
// (case-insensitive) substring filter, preserving section headers
// for buckets that still have at least one matching row. An empty
// filter returns the input unchanged.
func filterCheatsheetItems(items []cheatsheetItem, filter string) []cheatsheetItem {
	if filter == "" {
		return items
	}
	needle := strings.ToLower(filter)
	out := make([]cheatsheetItem, 0, len(items))
	var pendingHeader *cheatsheetItem
	flushHeader := func() {
		if pendingHeader != nil {
			out = append(out, *pendingHeader)
			pendingHeader = nil
		}
	}
	for i, it := range items {
		if it.IsHeader {
			hdr := items[i]
			pendingHeader = &hdr
			continue
		}
		hay := strings.ToLower(it.Description + " " + it.KeyLabel)
		if !strings.Contains(hay, needle) {
			continue
		}
		flushHeader()
		out = append(out, it)
	}
	return out
}

// renderCheatsheetBody renders the filtered cheatsheet item slice as
// a multi-line body. Section headers wear styleHeader; binding rows
// are two-column (right-aligned key + space + left-aligned
// description). The cursored row (relative to the non-header rows in
// the filtered slice) is wrapped in styleAccent and prefixed with
// "▶ ". keyColW is computed from the widest visible key in the
// filtered set (using lipgloss.Width so SGR escapes don't leak).
func renderCheatsheetBody(items []cheatsheetItem, cursor int) string {
	keyW := 0
	for _, it := range items {
		if it.IsHeader {
			continue
		}
		if n := lipgloss.Width(it.KeyLabel); n > keyW {
			keyW = n
		}
	}
	var b strings.Builder
	rowIdx := -1
	for _, it := range items {
		if it.IsHeader {
			b.WriteString("  " + styleHeader.Render("--- "+it.Section+" ---") + "\n")
			continue
		}
		rowIdx++
		pad := keyW - lipgloss.Width(it.KeyLabel)
		if pad < 0 {
			pad = 0
		}
		keyCell := strings.Repeat(" ", pad) + styleAccent.Render(it.KeyLabel)
		line := keyCell + "  " + it.Description
		if rowIdx == cursor {
			b.WriteString("▶ " + styleAccent.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if b.Len() == 0 {
		b.WriteString("  " + styleMuted.Render("(no matches)") + "\n")
	}
	return b.String()
}

// cheatsheetVisibleRowCount returns the number of non-header items
// in the filtered slice. Used to clamp CheatsheetCursor when the
// filter shrinks the list.
func cheatsheetVisibleRowCount(items []cheatsheetItem) int {
	n := 0
	for _, it := range items {
		if !it.IsHeader {
			n++
		}
	}
	return n
}

// cheatsheetSelectedItem returns the (filtered slice index, item)
// pair for the currently-cursored binding row, or (-1, zero) when
// the filtered list is empty.
func cheatsheetSelectedItem(items []cheatsheetItem, cursor int) (int, cheatsheetItem) {
	row := -1
	for i, it := range items {
		if it.IsHeader {
			continue
		}
		row++
		if row == cursor {
			return i, it
		}
	}
	return -1, cheatsheetItem{}
}

// layoutCheatsheet draws the popup view, sized to fit the filtered
// body. The view is editable so the cheatsheet editor receives
// printable runes (everything not bound view-scoped); see
// cheatsheetEditor for the keystroke contract.
func (gu *Gui) layoutCheatsheet(g *gocui.Gui, w, h int, title string) {
	var (
		items  []cheatsheetItem
		filter string
		cursor int
	)
	gu.st.withRLock(func() {
		items = gu.st.popup.CheatsheetItems
		filter = gu.st.popup.CheatsheetFilter
		cursor = gu.st.popup.CheatsheetCursor
	})
	filtered := filterCheatsheetItems(items, filter)
	visible := cheatsheetVisibleRowCount(filtered)
	// Clamp cursor to the visible row count so the rendered marker
	// always lands on an actual binding row.
	if visible == 0 {
		cursor = 0
	} else if cursor >= visible {
		cursor = visible - 1
	} else if cursor < 0 {
		cursor = 0
	}
	// Title row: append a live filter hint when the operator has
	// typed something, so they can see what's narrowing the list.
	displayTitle := title
	if filter != "" {
		displayTitle = title + " — /" + filter
	}
	body := renderCheatsheetBody(filtered, cursor)
	v := gu.drawPopup(g, winPopupCheatsheet, w, h, displayTitle, body)
	if v == nil {
		return
	}
	v.Editable = true
	v.Editor = gocui.EditorFunc(gu.cheatsheetEditor)
	v.Wrap = false
	if _, err := g.SetCurrentView(winPopupCheatsheet); err != nil && !isUnknownView(err) {
		return
	}
}

// cheatsheetEditor is the gocui Editor attached to the cheatsheet
// view. Returns `matched=true` to swallow the keystroke (don't
// fall through to global keybindings); `matched=false` leaves
// gocui's standard "global keybinding after editor" fallback in
// effect (so e.g. Ctrl-R hard-refresh still works while the popup
// is open).
//
// Every printable rune (including 'j' / 'k' / digits / punctuation
// like '*', '/', ':', '?', '@') appends to the live filter — the
// plan and AC2 say "typing any printable rune appends it to the
// filter," with no exemptions. Cursor motion lives entirely on
// view-scoped bindings (arrows, mouse wheel) registered on
// winPopupCheatsheet. j/k are deliberately NOT a navigation
// short-circuit: doing so would silently swallow them from the
// filter, breaking searches for verbs like "block" / "kill" /
// "unblock". Modifier-bearing chords are ignored — Backspace,
// Esc, Enter, arrows and wheel are already covered by view-scoped
// keybindings.
func (gu *Gui) cheatsheetEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) bool {
	// gocui only exposes ModNone / ModAlt / ModMotion — there's no
	// explicit ModShift. Shift+letter arrives as the already-cased
	// rune (e.g. 'A') with ModNone, so a strict "only ModNone"
	// check is safe.
	if mod != gocui.ModNone {
		return false
	}
	if ch != 0 && unicode.IsPrint(ch) {
		gu.cheatsheetAppendRune(ch)
		return true
	}
	return false
}

// cheatsheetMoveCursor steps the cursor by `step` rows over the
// CURRENT filtered set (the post-filter visible-row count is the
// modulus). No-op when the popup isn't a cheatsheet — defensive
// against late-arriving wheel events after popupClose has reset the
// state.
func (gu *Gui) cheatsheetMoveCursor(step int) {
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupCheatsheet {
			return
		}
		filtered := filterCheatsheetItems(gu.st.popup.CheatsheetItems, gu.st.popup.CheatsheetFilter)
		n := cheatsheetVisibleRowCount(filtered)
		if n == 0 {
			gu.st.popup.CheatsheetCursor = 0
			return
		}
		gu.st.popup.CheatsheetCursor = (gu.st.popup.CheatsheetCursor + step + n) % n
	})
	gu.requestRedraw()
}

// cheatsheetCursor wraps cheatsheetMoveCursor as a keybinding
// handler factory. Used by the j/k and arrow + wheel bindings.
func (gu *Gui) cheatsheetCursor(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.cheatsheetMoveCursor(step)
		return nil
	}
}

// cheatsheetAppendRune appends ch to the live filter and resets
// the cursor so the first matching row is selected. Without the
// reset a tight filter (1–2 rows) inherited a stale cursor from
// a wider previous filter and the highlight could land out of
// view.
func (gu *Gui) cheatsheetAppendRune(ch rune) {
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupCheatsheet {
			return
		}
		gu.st.popup.CheatsheetFilter += string(ch)
		gu.st.popup.CheatsheetCursor = 0
	})
	gu.requestRedraw()
}

// cheatsheetBackspace pops the last rune off the filter, ignoring
// the gocui keybinding's (*gocui.Gui, *gocui.View) signature. With
// an empty filter the keystroke is a no-op (a previous press
// already cleared everything; Esc is the close path).
func (gu *Gui) cheatsheetBackspace(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupCheatsheet {
			return
		}
		rs := []rune(gu.st.popup.CheatsheetFilter)
		if len(rs) == 0 {
			return
		}
		gu.st.popup.CheatsheetFilter = string(rs[:len(rs)-1])
		gu.st.popup.CheatsheetCursor = 0
	})
	gu.requestRedraw()
	return nil
}

// cheatsheetEscape implements the two-step Esc semantics: a
// non-empty filter is cleared (the popup stays open), an empty
// filter closes the popup.
func (gu *Gui) cheatsheetEscape(g *gocui.Gui, v *gocui.View) error {
	var close bool
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupCheatsheet {
			close = true
			return
		}
		if gu.st.popup.CheatsheetFilter == "" {
			close = true
			return
		}
		gu.st.popup.CheatsheetFilter = ""
		gu.st.popup.CheatsheetCursor = 0
	})
	if close {
		return gu.popupClose(g, v)
	}
	gu.requestRedraw()
	return nil
}

// cheatsheetAccept fires the cursored binding's handler. The popup
// is closed BEFORE the handler runs so any handler that inspects
// g.CurrentView() sees the dashboard underneath instead of the
// dismissed cheatsheet view.
func (gu *Gui) cheatsheetAccept(g *gocui.Gui, v *gocui.View) error {
	var handler func() error
	gu.st.withLock(func() {
		if gu.st.popup.Kind != popupCheatsheet {
			return
		}
		filtered := filterCheatsheetItems(gu.st.popup.CheatsheetItems, gu.st.popup.CheatsheetFilter)
		_, item := cheatsheetSelectedItem(filtered, gu.st.popup.CheatsheetCursor)
		handler = item.Handler
		gu.st.popup = popupState{}
	})
	// Drop the cheatsheet view BEFORE invoking the handler so any
	// handler that opens another popup doesn't fight the
	// just-dismissed view for current-view ownership.
	if g != nil {
		_ = g.DeleteView(winPopupCheatsheet)
	}
	gu.requestRedraw()
	if handler != nil {
		return handler()
	}
	return nil
}
