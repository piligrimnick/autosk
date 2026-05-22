package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/store"
)

// bindKeys wires every keybinding the TUI uses. Bindings are
// per-view: the global ones bind ViewName="" (all views).
//
// We split into three tiers:
//
//   - globals (quit, tab switching, palette, filter, help, scope clear)
//   - per-panel (Tasks/Jobs/Workflows/Agents list nav + write verbs)
//   - Detail pane scroll (winDetail) + Job-input textarea
//     (winJobInput) when a running job is selected
func (gu *Gui) bindKeys() error {
	type binding struct {
		view string
		key  any
		mod  gocui.Modifier
		h    func(*gocui.Gui, *gocui.View) error
	}

	bs := []binding{
		// global quit + help. Space is intentionally NOT global — it
		// has per-panel semantics (Tasks: commit scope) and a global
		// space binding would swallow the per-view ones underneath.
		{"", gocui.KeyCtrlC, gocui.ModNone, gu.quit},
		{"", 'q', gocui.ModNone, gu.quit},
		{"", '?', gocui.ModNone, gu.openHelp},
		{"", gocui.KeyEsc, gocui.ModNone, gu.handleEsc},
		// panel switching
		{"", '1', gocui.ModNone, gu.focusPanel(panelTasks)},
		{"", '2', gocui.ModNone, gu.focusPanel(panelJobs)},
		{"", '3', gocui.ModNone, gu.focusPanel(panelWorkflows)},
		{"", '4', gocui.ModNone, gu.focusPanel(panelAgents)},
		{"", '0', gocui.ModNone, gu.focusPanel(panelDetail)},
		{"", gocui.KeyTab, gocui.ModNone, gu.cyclePanel(+1)},
		// refresh + scope + filter + palette
		{"", 'R', gocui.ModNone, gu.handleRefresh},
		// Ctrl-R is the "hard refresh": force the doltlite store to drop
		// its pooled connection (recover from a cross-process dolt_gc
		// that atomic-rewrote .autosk/db). Plain R alone fires the same
		// adaptive tick the 2s timer uses — useful as a one-shot kick;
		// Ctrl-R is the escape hatch when the operator suspects the
		// underlying fd is on an orphan inode.
		{"", gocui.KeyCtrlR, gocui.ModNone, gu.handleForceReconnect},
		{"", '*', gocui.ModNone, gu.handleClearScope},
		{"", '/', gocui.ModNone, gu.openFilter},
		{"", ':', gocui.ModNone, gu.openPalette},
		{"", '@', gocui.ModNone, gu.toggleLog},

		// list nav (per panel; gocui binds per-view, so we attach once
		// per side panel + the detail pane)
		{winTasks, 'j', gocui.ModNone, gu.cursorDown(panelTasks)},
		{winTasks, 'k', gocui.ModNone, gu.cursorUp(panelTasks)},
		{winTasks, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelTasks)},
		{winTasks, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelTasks)},
		{winTasks, gocui.KeyEnter, gocui.ModNone, gu.tasksEnter},
		// Space — commit the cursor row as the Jobs-panel scope chip
		// without leaving the Tasks panel. The counterpart to Enter
		// (which commits + jumps). j/k do NOT auto-commit anymore
		// (operator complaint: cursor-driven re-filtering was noisy),
		// so this is the only path on Tasks for setting scope.TaskID.
		// Press * to clear.
		{winTasks, ' ', gocui.ModNone, gu.tasksScopeFromCursor},

		{winJobs, 'j', gocui.ModNone, gu.cursorDown(panelJobs)},
		{winJobs, 'k', gocui.ModNone, gu.cursorUp(panelJobs)},
		{winJobs, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelJobs)},
		{winJobs, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelJobs)},
		{winJobs, gocui.KeyEnter, gocui.ModNone, gu.jobsEnter},

		{winWorkflows, 'j', gocui.ModNone, gu.cursorDown(panelWorkflows)},
		{winWorkflows, 'k', gocui.ModNone, gu.cursorUp(panelWorkflows)},
		{winWorkflows, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelWorkflows)},
		{winWorkflows, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelWorkflows)},
		{winWorkflows, gocui.KeyEnter, gocui.ModNone, gu.workflowsEnter},

		{winAgents, 'j', gocui.ModNone, gu.cursorDown(panelAgents)},
		{winAgents, 'k', gocui.ModNone, gu.cursorUp(panelAgents)},
		{winAgents, gocui.KeyArrowDown, gocui.ModNone, gu.cursorDown(panelAgents)},
		{winAgents, gocui.KeyArrowUp, gocui.ModNone, gu.cursorUp(panelAgents)},
		{winAgents, gocui.KeyEnter, gocui.ModNone, gu.agentsEnter},

		// Tasks write verbs.
		{winTasks, 'n', gocui.ModNone, gu.taskNew},
		{winTasks, 'c', gocui.ModNone, gu.taskEdit},
		{winTasks, 'd', gocui.ModNone, gu.taskDone},
		{winTasks, 'x', gocui.ModNone, gu.taskCancel},
		{winTasks, 'e', gocui.ModNone, gu.taskEnroll},
		{winTasks, 'r', gocui.ModNone, gu.taskResume},
		{winTasks, 'b', gocui.ModNone, gu.taskBlock},
		{winTasks, 'u', gocui.ModNone, gu.taskUnblock},
		{winTasks, 'm', gocui.ModNone, gu.taskComment},
		{winTasks, 'M', gocui.ModNone, gu.taskMetadataEdit},
		{winTasks, 'p', gocui.ModNone, gu.taskPriority},
		{winTasks, 'o', gocui.ModNone, gu.taskReopen},
		// Note: there is no `c claim` binding. The v0.2 schema has no
		// claim verb — tasks self-advance via workflow steps. `c` is
		// bound to `change` (edit title + description in the two-pane
		// compose); use `e` to enroll instead.

		// Workflows write verbs.
		{winWorkflows, 'n', gocui.ModNone, gu.workflowNew},
		{winWorkflows, 'D', gocui.ModNone, gu.workflowDelete},

		// Agents "write" verbs are intentionally informational only:
		// the daemon has no /v1/agents endpoint in v1, so both keys
		// flash a toast directing the operator at the corresponding
		// `autosk agent {install|uninstall} <pkg>` CLI invocation. The
		// bindings stay so the help screen's 'i install / u uninstall'
		// row is honest and discoverable.
		{winAgents, 'i', gocui.ModNone, gu.agentInstall},
		{winAgents, 'u', gocui.ModNone, gu.agentUninstall},

		// Jobs hotkeys.
		{winJobs, 'K', gocui.ModNone, gu.jobCancel},

		// Detail pane scroll bindings. Same shape that used to live on
		// the Inspector body view — j/k single line, Ctrl-F/Ctrl-B page,
		// g/G start/end. Applies to BOTH task-detail and job-detail
		// content (the pane is one view; the renderer picks the body
		// based on focused panel).
		{winDetail, 'j', gocui.ModNone, gu.detailScroll(+1)},
		{winDetail, 'k', gocui.ModNone, gu.detailScroll(-1)},
		{winDetail, gocui.KeyArrowDown, gocui.ModNone, gu.detailScroll(+1)},
		{winDetail, gocui.KeyArrowUp, gocui.ModNone, gu.detailScroll(-1)},
		{winDetail, gocui.KeyCtrlF, gocui.ModNone, gu.detailScrollPage(+1)},
		{winDetail, gocui.KeyCtrlB, gocui.ModNone, gu.detailScrollPage(-1)},
		{winDetail, gocui.KeyPgdn, gocui.ModNone, gu.detailScrollPage(+1)},
		{winDetail, gocui.KeyPgup, gocui.ModNone, gu.detailScrollPage(-1)},
		{winDetail, 'g', gocui.ModNone, gu.detailScrollTo(false)},
		{winDetail, 'G', gocui.ModNone, gu.detailScrollTo(true)},

		// Job-input bindings (relocated from the old fullscreen-inspector input view).
		// Bound only on winJobInput; the view itself only exists when
		// the selected job is running, so these bindings are dormant
		// otherwise.
		{winJobInput, gocui.KeyCtrlD, gocui.ModNone, gu.liveSend},
		{winJobInput, gocui.KeyCtrlF, gocui.ModNone, gu.liveFollowUp},
		{winJobInput, gocui.KeyCtrlA, gocui.ModNone, gu.liveAbort},
		{winJobInput, gocui.KeyEsc, gocui.ModNone, gu.jobInputEscape},
		{winJobInput, gocui.KeyCtrlC, gocui.ModNone, gu.quit},
		// Scroll the Detail pane above without leaving the textarea.
		{winJobInput, gocui.KeyCtrlB, gocui.ModNone, gu.detailScrollPage(-1)},
		{winJobInput, gocui.KeyPgup, gocui.ModNone, gu.detailScrollPage(-1)},
		{winJobInput, gocui.KeyPgdn, gocui.ModNone, gu.detailScrollPage(+1)},
		{winJobInput, gocui.MouseWheelDown, gocui.ModNone, gu.detailScroll(+1)},
		{winJobInput, gocui.MouseWheelUp, gocui.ModNone, gu.detailScroll(-1)},

		// Mouse-wheel scroll bindings. gocui surfaces wheel events as
		// MouseWheelUp/Down keys per-view; without these bindings the
		// terminal's wheel forwarding is dropped on the floor and
		// overflowing content (Detail-pane transcripts, lists) is
		// only reachable via j/k or PgUp/PgDn.
		//
		// Side panels: wheel scrolls the VIEWPORT without moving the
		// selection — lazygit's convention (see
		// pkg/gui/controllers/list_controller.go:HandleScrollUp/Down).
		// j/k after a wheel-scroll snap the viewport back so the
		// selection is visible again (scrollOffAdjust's centring path).
		{winTasks, gocui.MouseWheelDown, gocui.ModNone, gu.panelScroll(panelTasks, +panelScrollStep)},
		{winTasks, gocui.MouseWheelUp, gocui.ModNone, gu.panelScroll(panelTasks, -panelScrollStep)},
		{winJobs, gocui.MouseWheelDown, gocui.ModNone, gu.panelScroll(panelJobs, +panelScrollStep)},
		{winJobs, gocui.MouseWheelUp, gocui.ModNone, gu.panelScroll(panelJobs, -panelScrollStep)},
		{winWorkflows, gocui.MouseWheelDown, gocui.ModNone, gu.panelScroll(panelWorkflows, +panelScrollStep)},
		{winWorkflows, gocui.MouseWheelUp, gocui.ModNone, gu.panelScroll(panelWorkflows, -panelScrollStep)},
		{winAgents, gocui.MouseWheelDown, gocui.ModNone, gu.panelScroll(panelAgents, +panelScrollStep)},
		{winAgents, gocui.MouseWheelUp, gocui.ModNone, gu.panelScroll(panelAgents, -panelScrollStep)},

		// Detail pane: wheel scrolls the viewport (no cursor concept).
		{winDetail, gocui.MouseWheelDown, gocui.ModNone, gu.detailScroll(+1)},
		{winDetail, gocui.MouseWheelUp, gocui.ModNone, gu.detailScroll(-1)},

		// Popup menus also benefit from wheel-driven cursor motion.
		{winPopupMenu, gocui.MouseWheelDown, gocui.ModNone, gu.popupCursor(+1)},
		{winPopupMenu, gocui.MouseWheelUp, gocui.ModNone, gu.popupCursor(-1)},

		// Popup keys.
		{winPopupMenu, 'j', gocui.ModNone, gu.popupCursor(+1)},
		{winPopupMenu, 'k', gocui.ModNone, gu.popupCursor(-1)},
		{winPopupMenu, gocui.KeyArrowDown, gocui.ModNone, gu.popupCursor(+1)},
		{winPopupMenu, gocui.KeyArrowUp, gocui.ModNone, gu.popupCursor(-1)},
		{winPopupMenu, gocui.KeyEnter, gocui.ModNone, gu.popupAccept},
		{winPopupMenu, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
		{winPopupConfirm, 'y', gocui.ModNone, gu.popupConfirmYes},
		{winPopupConfirm, 'Y', gocui.ModNone, gu.popupConfirmYes},
		{winPopupConfirm, 'n', gocui.ModNone, gu.popupClose},
		{winPopupConfirm, 'N', gocui.ModNone, gu.popupClose},
		{winPopupConfirm, gocui.KeyEnter, gocui.ModNone, gu.popupConfirmYes},
		{winPopupConfirm, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
		{winPopupPrompt, gocui.KeyEnter, gocui.ModNone, gu.popupAccept},
		{winPopupPrompt, gocui.KeyEsc, gocui.ModNone, gu.popupClose},

		// Task-compose popup keys (lazygit commit-message editor parity).
		//
		// Summary pane: Enter submits (so the editor never sees the
		// keystroke and can't insert "\n" into a single-line title),
		// Tab toggles to Description, Esc closes, Ctrl+S also submits
		// for symmetry with the description pane.
		//
		// Description pane: Enter is INTENTIONALLY NOT bound here —
		// gocui falls through to SimpleEditor which inserts "\n", which
		// is the whole point of having a description pane. Submitting
		// requires Ctrl+S or Alt+Enter (the user picked both). Tab
		// toggles back to Summary, Esc closes.
		{winTaskComposeSummary, gocui.KeyEnter, gocui.ModNone, gu.taskComposeConfirm},
		{winTaskComposeSummary, gocui.KeyCtrlS, gocui.ModNone, gu.taskComposeConfirm},
		{winTaskComposeSummary, gocui.KeyTab, gocui.ModNone, gu.taskComposeToggle},
		{winTaskComposeSummary, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
		{winTaskComposeDescription, gocui.KeyCtrlS, gocui.ModNone, gu.taskComposeConfirm},
		{winTaskComposeDescription, gocui.KeyEnter, gocui.ModAlt, gu.taskComposeConfirm},
		{winTaskComposeDescription, gocui.KeyTab, gocui.ModNone, gu.taskComposeToggle},
		{winTaskComposeDescription, gocui.KeyEsc, gocui.ModNone, gu.popupClose},

		// Single-pane multi-line compose popup (comment / metadata).
		//
		// Plain Enter is intentionally NOT bound — it falls through to
		// gocui.DefaultEditor which inserts "\n", which is the whole
		// point of the multi-line popup. Submitting requires Ctrl+S or
		// Alt+Enter. Esc cancels without invoking OnAccept.
		{winSingleCompose, gocui.KeyCtrlS, gocui.ModNone, gu.singleComposeConfirm},
		{winSingleCompose, gocui.KeyEnter, gocui.ModAlt, gu.singleComposeConfirm},
		{winSingleCompose, gocui.KeyEsc, gocui.ModNone, gu.popupClose},
		{winSingleCompose, gocui.KeyCtrlC, gocui.ModNone, gu.quit},
	}
	for _, b := range bs {
		if err := gu.g.SetKeybinding(b.view, b.key, b.mod, b.h); err != nil {
			return err
		}
	}
	return nil
}

// focusPanel returns a handler that focuses the named panel. After
// focus moves we snap the new panel's viewport so its selected row
// is visible — the user may have wheel-scrolled it out of view
// during the previous visit and now expects to see where they were.
// Matches lazygit's HandleFocus(ScrollSelectionIntoView=true).
//
// When p is panelDetail we also stash the OUTGOING focus into
// state.detailFocus so renderDetail can keep rendering the right
// entity body (the panelDetail arm itself has nothing to draw).
// The stash normalises panelJobInput → panelJobs so a transition
// from the input flow into the Detail pane still renders Job
// Detail (renderDetail also normalises on the read side, but the
// model field stays sensible this way too).
func (gu *Gui) focusPanel(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		changed := false
		gu.st.withLock(func() {
			if gu.st.focused != p {
				changed = true
			}
			if p == panelDetail && gu.st.focused != panelDetail {
				gu.st.detailFocus = gu.st.focused.normalizeForDetail()
			}
			gu.st.focused = p
		})
		if changed {
			gu.snapViewportToCursor(p)
		}
		return nil
	}
}

// cyclePanel cycles through the four side panels by step.
//
// Source-side normalisation: cyclePanel may fire from any view,
// including the synthetic panelJobInput (caret in winJobInput) and
// panelDetail (focus parked on the right pane via '0'). Neither is
// in the 0..3 cycle range, so we normalise them first —
// panelJobInput→panelJobs (predictable: the operator was working a
// running job, so Tab steps OFF the job they were composing for to
// the next side panel), panelDetail→detailFocus (the side panel
// that owned the selection before '0' was pressed). Without this
// normalisation, (int(panelJobInput=5) + 1) % 4 = 2 = panelWorkflows
// would skip the Jobs column entirely on Tab from the input — a
// UX regression vs. the pre-redesign state where Tab inside the
// fullscreen inspector was scoped to inspector-tab cycling.
func (gu *Gui) cyclePanel(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var landed panelID
		changed := false
		gu.st.withLock(func() {
			src := gu.st.focused
			switch src {
			case panelJobInput:
				src = panelJobs
			case panelDetail:
				src = gu.st.detailFocus.normalizeForDetail()
				if src == panelDetail {
					src = panelTasks
				}
			}
			n := 4
			next := (int(src) + step + n) % n
			if int(gu.st.focused) != next {
				changed = true
			}
			gu.st.focused = panelID(next)
			landed = panelID(next)
		})
		if changed {
			gu.snapViewportToCursor(landed)
		}
		return nil
	}
}

// cursorDown / cursorUp move the cursor for a given list panel. After
// the move we call scrollOffAdjust so the viewport follows the
// cursor with a 2-row look-ahead in the direction of motion (lazygit
// scrollOffMargin parity). Without that call j-spam past the bottom
// of a non-empty viewport leaves the highlight off-screen and the
// list visually stuck on the top page — the bug this fixes.
func (gu *Gui) cursorDown(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var before, after, header int
		gu.st.withLock(func() {
			switch p {
			case panelTasks:
				before = gu.st.taskCursor
				gu.st.taskCursor = clampCursor(gu.st.taskCursor+1, len(gu.st.tasks))
				after = gu.st.taskCursor
			case panelJobs:
				before = gu.st.jobCursor
				gu.st.jobCursor = clampCursor(gu.st.jobCursor+1, len(gu.st.jobs))
				after = gu.st.jobCursor
			case panelWorkflows:
				before = gu.st.workflowCursor
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor+1, len(gu.st.workflows))
				after = gu.st.workflowCursor
			case panelAgents:
				before = gu.st.agentCursor
				gu.st.agentCursor = clampCursor(gu.st.agentCursor+1, len(gu.st.agents))
				after = gu.st.agentCursor
			}
			header = headerLinesForLocked(p, gu.st)
		})
		gu.scrollOffAdjust(p.window(), header+before, header+after)
		gu.afterCursorMove(p)
		return nil
	}
}

func (gu *Gui) cursorUp(p panelID) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var before, after, header int
		gu.st.withLock(func() {
			switch p {
			case panelTasks:
				before = gu.st.taskCursor
				gu.st.taskCursor = clampCursor(gu.st.taskCursor-1, len(gu.st.tasks))
				after = gu.st.taskCursor
			case panelJobs:
				before = gu.st.jobCursor
				gu.st.jobCursor = clampCursor(gu.st.jobCursor-1, len(gu.st.jobs))
				after = gu.st.jobCursor
			case panelWorkflows:
				before = gu.st.workflowCursor
				gu.st.workflowCursor = clampCursor(gu.st.workflowCursor-1, len(gu.st.workflows))
				after = gu.st.workflowCursor
			case panelAgents:
				before = gu.st.agentCursor
				gu.st.agentCursor = clampCursor(gu.st.agentCursor-1, len(gu.st.agents))
				after = gu.st.agentCursor
			}
			header = headerLinesForLocked(p, gu.st)
		})
		gu.scrollOffAdjust(p.window(), header+before, header+after)
		gu.afterCursorMove(p)
		return nil
	}
}

// afterCursorMove fires the per-panel side-effect that should follow
// a cursor change. Pulled out of cursorDown/cursorUp so the policy
// lives in one place — and so we can give Tasks a different
// behaviour from Workflows (which auto-scopes Tasks) without
// duplicating the switch in both directions.
//
// Per-panel contract:
//
//   - panelTasks: schedule a refresh so the detail pane picks up
//     the highlighted task's comments and signals.
//   - panelWorkflows: applyScope so the Tasks panel auto-filters
//     to the highlighted workflow.
//   - panelJobs: scheduleRefresh + (debounced) SSE subscription
//     for the highlighted job + (eager) archive hydration so the
//     Detail pane has events to render. The chain is:
//     afterCursorMove(panelJobs)
//     → scheduleJobLive (debounced timer)
//     → scheduleJobArchive (eager OnWorker dispatch, coalesced).
//     The Detail pane reads state.jobTranscript[jobID] every
//     frame; the SSE pump appends into it.
//   - panelAgents: scheduleRefresh only.
func (gu *Gui) afterCursorMove(p panelID) {
	switch p {
	case panelWorkflows:
		gu.applyScope()
	case panelJobs:
		gu.scheduleRefresh()
		if j, ok := gu.st.selectedJobLocked(); ok {
			running := isJobRunning(j)
			// Discard any input draft that belongs to a different job
			// BEFORE we kick the live/archive scheduling — the next
			// renderViews pass will repopulate the view from
			// state.jobInput, which is now empty for this job.
			gu.clearJobInputIfStale(j.JobID)
			gu.scheduleJobLive(j.JobID, running)
			gu.scheduleJobArchive(j.JobID, running)
		} else {
			gu.clearJobInputIfStale("")
			gu.stopJobLive()
		}
	default:
		gu.scheduleRefresh()
	}
}

// tasksScopeFromCursor is the Space-key handler on the Tasks panel:
// commits the current cursor row as scope.TaskID (filtering the
// Jobs panel) without changing focus. The complement to tasksEnter
// (which commits + jumps) and the only path j/k navigation leaves
// open after the cursor-move auto-scope was removed.
func (gu *Gui) tasksScopeFromCursor(*gocui.Gui, *gocui.View) error {
	var id string
	gu.st.withRLock(func() {
		if t, ok := gu.st.selectedTask(); ok {
			id = t.ID
		}
	})
	if id == "" {
		return nil
	}
	gu.applyScope()
	gu.flashf("info", "filter: %s (press * to reset)", id)
	return nil
}

// applyScope re-derives the scope chips from the current cursor in
// Tasks / Workflows. Workflows call this on every cursor move via
// afterCursorMove; Tasks call it only from explicit commit paths
// (Enter / Space) per the cursor-move policy documented above.
func (gu *Gui) applyScope() {
	gu.st.withLock(func() {
		switch gu.st.focused {
		case panelTasks:
			if t, ok := gu.st.selectedTask(); ok {
				gu.st.scope.TaskID = t.ID
			} else {
				gu.st.scope.TaskID = ""
			}
		case panelWorkflows:
			if w, ok := gu.st.selectedWorkflow(); ok {
				gu.st.scope.WorkflowID = w.ID
				gu.st.scope.WorkflowName = w.Name
			} else {
				gu.st.scope.WorkflowID = ""
				gu.st.scope.WorkflowName = ""
			}
		}
	})
	gu.scheduleRefresh()
}

// handleClearScope clears every scope chip.
func (gu *Gui) handleClearScope(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() { gu.st.scope = scope{} })
	gu.flashf("info", "scope cleared")
	gu.scheduleRefresh()
	return nil
}

// handleRefresh re-fetches every panel on demand.
func (gu *Gui) handleRefresh(*gocui.Gui, *gocui.View) error {
	gu.scheduleRefresh()
	gu.flashf("info", "refresh")
	return nil
}

// handleForceReconnect is the Ctrl-R handler: drop any pooled
// *sqlite3.SQLiteConn inside the doltlite store, clear the per-job
// transcript cache (so the next selection re-hydrates from a fresh
// archive read), tear down any active SSE subscription, and
// schedule a refresh. The reconnect happens on a worker so the UI
// thread stays responsive (the Ping inside Reconnect blocks until
// the fresh conn handshakes). On error we still fall through to
// scheduleRefresh — a stale read is better than no read.
func (gu *Gui) handleForceReconnect(*gocui.Gui, *gocui.View) error {
	gu.flashf("info", "reconnect")
	gu.stopJobLive()
	gu.st.withLock(func() {
		gu.st.jobTranscript = map[string]*jobTranscriptEntry{}
	})
	gu.g.OnWorker(func(_ gocui.Task) error {
		ctx, cancel := context.WithTimeout(gu.ctx, 5*time.Second)
		defer cancel()
		if err := gu.ds.Reconnect(ctx); err != nil {
			dlog("reconnect: %v", err)
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.flashf("warn", "reconnect failed: %v", err)
				return nil
			})
		}
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// handleEsc closes the active popup; falls back to clearing filter
// chips. Esc on winJobInput is bound directly to jobInputEscape so
// it doesn't route through here.
func (gu *Gui) handleEsc(g *gocui.Gui, v *gocui.View) error {
	// Snapshot popup.Kind under the lock so concurrent mutations
	// from g.Update closures don't race against the read.
	var popupKind popupKind
	gu.st.withRLock(func() {
		popupKind = gu.st.popup.Kind
	})
	if popupKind != popupNone {
		return gu.popupClose(g, v)
	}
	// Otherwise: clear the focused panel's filter.
	gu.st.withLock(func() {
		switch gu.st.focused {
		case panelTasks:
			gu.st.filter.Tasks = ""
		case panelJobs:
			gu.st.filter.Jobs = ""
		case panelWorkflows:
			gu.st.filter.Workflows = ""
		case panelAgents:
			gu.st.filter.Agents = ""
		}
	})
	gu.scheduleRefresh()
	return nil
}

// toggleLog flips command-log visibility.
func (gu *Gui) toggleLog(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() { gu.st.logHide = !gu.st.logHide })
	return nil
}

// tasksEnter cross-links to Jobs (filter by this task) and focuses Jobs.
func (gu *Gui) tasksEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelJobs })
	return nil
}

// workflowsEnter cross-links to Tasks (filter by this workflow).
func (gu *Gui) workflowsEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelTasks })
	return nil
}

// agentsEnter prompts the user to opt into the agent scope.
//
// Design plan §3.4: the relation is ambiguous (author vs.
// current_step.agent are different concepts), so we force the
// operator to pick ONE. The choice flows into scope.AgentRel and is
// surfaced both on the Tasks-panel scope chip and in the status bar.
func (gu *Gui) agentsEnter(*gocui.Gui, *gocui.View) error {
	a, ok := gu.st.selectedAgentLocked()
	if !ok {
		return nil
	}
	gu.openMenu("Filter Tasks by agent "+a.Name+"?", []string{
		"by author (author_id == " + a.Name + ")",
		"by current step (current_step.agent == " + a.Name + ")",
		"cancel",
	}, func(i int) error {
		var rel agentRel
		switch i {
		case 0:
			rel = agentRelAuthor
		case 1:
			rel = agentRelStep
		default:
			return nil
		}
		gu.st.withLock(func() {
			gu.st.scope.Agent = a.Name
			gu.st.scope.AgentRel = rel
			gu.st.focused = panelTasks
		})
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// jobsEnter focuses the appropriate view for the highlighted job:
//   - non-terminal job (queued or running, daemon accepts SendInput)
//     → winJobInput, caret blinks inside the textarea.
//   - terminal job (no SendInput dispatch surface) → logical focus
//     to panelDetail so j/k scroll the transcript above.
//
// The visibility predicate (isJobLive) intentionally mirrors the
// layout-pass gate on winJobInput: Enter only routes to the input
// view when that view actually exists this frame.
//
// On the live-job branch we set state.focused = panelJobInput
// (a synthetic focus identity whose window() is winJobInput). The
// layout pass calls g.SetCurrentView(focused.window()) on every
// frame; without recording the focus in the model the next layout
// would yank current-view back to winJobs within ~100ms (spinner
// tick cadence) and the caret would jump out of the textarea.
//
// On the terminal-job branch we stash detailFocus = panelJobs so
// renderDetail keeps emitting the Job Detail body even though
// focused now reads panelDetail.
func (gu *Gui) jobsEnter(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	if isJobLive(j) {
		gu.st.withLock(func() { gu.st.focused = panelJobInput })
		// Also SetCurrentView synchronously so tests that don't
		// drive a layout pass between jobsEnter and the assertion
		// observe the focus change immediately. The layout pass
		// would otherwise be the one to actually move the caret.
		if gu.g != nil {
			_, _ = gu.g.SetCurrentView(winJobInput)
		}
		return nil
	}
	gu.st.withLock(func() {
		gu.st.detailFocus = panelJobs
		gu.st.focused = panelDetail
	})
	return nil
}

// openFilter opens the per-panel filter prompt.
func (gu *Gui) openFilter(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("filter: facets like p:1 status:done wf:name agent:name + free text", "", func(value string) error {
		gu.st.withLock(func() {
			switch gu.st.focused {
			case panelTasks:
				gu.st.filter.Tasks = value
			case panelJobs:
				gu.st.filter.Jobs = value
			case panelWorkflows:
				gu.st.filter.Workflows = value
			case panelAgents:
				gu.st.filter.Agents = value
			}
		})
		gu.scheduleRefresh()
		return nil
	})
	return nil
}

// openPalette opens the command palette.
func (gu *Gui) openPalette(*gocui.Gui, *gocui.View) error {
	cmds := []string{
		"task new",
		"task edit",
		"task done",
		"task cancel",
		"task reopen",
		"task priority",
		"task resume",
		"task enroll",
		"task block",
		"task unblock",
		"task comment",
		"task metadata",
		"workflow create",
		"workflow delete",
		"job cancel",
		"scope clear",
		"refresh",
		"quit",
	}
	gu.openMenu("command palette", cmds, func(i int) error {
		gu.dispatchPaletteCommand(cmds[i])
		return nil
	})
	return nil
}

func (gu *Gui) dispatchPaletteCommand(cmd string) {
	switch cmd {
	case "task new":
		_ = gu.taskNew(nil, nil)
	case "task edit":
		_ = gu.taskEdit(nil, nil)
	case "task done":
		_ = gu.taskDone(nil, nil)
	case "task cancel":
		_ = gu.taskCancel(nil, nil)
	case "task reopen":
		_ = gu.taskReopen(nil, nil)
	case "task priority":
		_ = gu.taskPriority(nil, nil)
	case "task resume":
		_ = gu.taskResume(nil, nil)
	case "task enroll":
		_ = gu.taskEnroll(nil, nil)
	case "task block":
		_ = gu.taskBlock(nil, nil)
	case "task unblock":
		_ = gu.taskUnblock(nil, nil)
	case "task comment":
		_ = gu.taskComment(nil, nil)
	case "task metadata":
		_ = gu.taskMetadataEdit(nil, nil)
	case "workflow create":
		_ = gu.workflowNew(nil, nil)
	case "workflow delete":
		_ = gu.workflowDelete(nil, nil)
	case "job cancel":
		_ = gu.jobCancel(nil, nil)
	case "scope clear":
		_ = gu.handleClearScope(nil, nil)
	case "refresh":
		_ = gu.handleRefresh(nil, nil)
	case "quit":
		gu.cancel()
		gu.g.Update(func(_ *gocui.Gui) error { return gocui.ErrQuit })
	}
}

// openHelp opens the per-binding cheatsheet.
func (gu *Gui) openHelp(*gocui.Gui, *gocui.View) error {
	lines := []string{
		"global:",
		"  1..4 Tab     focus side panel    /   filter",
		"  0            focus detail        :   palette",
		"  R            refresh             *   clear scope",
		"  Ctrl-R       hard refresh (reopen db conn; recover from cross-process gc)",
		"  @            toggle log          ?   help",
		"  q Ctrl-C     quit                Esc back/close",
		"",
		"tasks:",
		"  Space        filter Jobs by selected task (stay on Tasks)",
		"  Enter        filter Jobs by selected task and focus Jobs",
		"  n new    c edit    d done     x cancel   o reopen",
		"  e enroll r resume  b block    u unblock  m comment",
		"  p priority         M metadata",
		"",
		"jobs:",
		"  Enter      focus input (non-terminal) / Detail pane (terminal)",
		"  K          cancel job",
		"",
		"workflows:",
		"  n new (from file)   D delete",
		"",
		"agents:",
		"  i install   u uninstall",
		"",
		"detail:",
		"  j/k arrows                line scroll",
		"  Ctrl-F/Ctrl-B/PgUp/PgDn   page scroll",
		"  g/G                       top/bottom",
		"  wheel                     scroll",
		"",
		"job input (non-terminal job):",
		"  Ctrl-D send     Ctrl-F follow_up    Ctrl-A abort",
		"  Esc cancel      Ctrl-B/PgUp/PgDn    scroll transcript above",
		"",
		"compose (new task / edit task):",
		"  summary: Enter / Ctrl-S submit  Tab → description  Esc cancel",
		"  description: Ctrl-S / Alt-Enter submit  Tab → summary  Esc cancel",
		"compose (comment / metadata):",
		"  Ctrl-S / Alt-Enter submit  Enter newline  Esc cancel",
	}
	gu.openMenu("help", lines, func(_ int) error { return gu.popupClose(nil, nil) })
	return nil
}

// taskNew opens the lazygit-style two-pane compose editor and
// creates a task on submit. The summary pane carries the task title
// (required); the description pane carries an optional multi-line
// body that goes straight into the tasks.description column. Empty
// summary is treated as a silent cancel (matches the previous
// single-line prompt's behaviour).
func (gu *Gui) taskNew(*gocui.Gui, *gocui.View) error {
	gu.openTaskCompose("New task", "", "", func(summary, description string) error {
		if strings.TrimSpace(summary) == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			id, err := gu.ds.CreateTask(gu.ctx, summary, description, store.DefaultPriority)
			if err != nil {
				gu.flashf("err", "task new: %v", err)
				return nil
			}
			gu.flashf("info", "created %s", id)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

// taskEdit opens the two-pane compose editor pre-filled with the
// selected task's title + description. On submit the values flow to
// Datasource.UpdateTitleDescription. An empty title (post-trim) is
// rejected with a flash and the popup is re-opened with the typed
// text intact so the user can fix it without losing their work.
func (gu *Gui) taskEdit(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openTaskComposeForEdit(t.ID, t.Title, t.Description)
	return nil
}

// taskMetadataEdit opens the single-pane multi-line compose editor
// pre-filled with the selected task's metadata as pretty-printed
// JSON (`{}` when empty/nil). On submit the text is parsed back
// into a map[string]any and pushed via Datasource.SetMetadata. On
// parse failure (invalid JSON, or a JSON value that isn't an
// object) the popup is re-opened with the typed text intact so the
// user can fix it.
func (gu *Gui) taskMetadataEdit(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	initial := "{}"
	if len(t.Metadata) > 0 {
		b, err := json.MarshalIndent(t.Metadata, "", "  ")
		if err != nil {
			// Defensive: a map of any may carry a non-marshalable
			// value (e.g. a func smuggled in by a buggy caller).
			// Surface the error and bail without opening the popup.
			gu.flashf("err", "metadata: %v", err)
			return nil
		}
		initial = string(b)
	}
	gu.openSingleComposeForMetadata(t.ID, initial)
	return nil
}

// openSingleComposeForMetadata is the openSingleCompose wrapper for
// the metadata-edit flow. Same shape as openTaskComposeForEdit: the
// validation-error branch re-invokes itself with the same typed
// text so the popup "stays open" without the user re-typing.
//
// JSON validation has two gates: json.Unmarshal must succeed AND
// the decoded value must actually be a JSON object. Both `null` and
// non-object JSON (arrays, strings, numbers, bool) decode into a
// map[string]any without an error — `null` populates a nil map and
// non-objects fail mid-decode — but the user-visible contract per
// plan AC #9 is that only a JSON OBJECT is acceptable. Treat the
// nil-map case as a validation failure so typing the literal `null`
// doesn't silently wipe metadata. (Note: json.Unmarshal of an
// array / string / number / bool into a map[string]any returns a
// proper error, so they go through the existing err branch.)
func (gu *Gui) openSingleComposeForMetadata(id, initial string) {
	gu.openSingleCompose("Metadata "+id, "JSON object", initial, func(text string) error {
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			gu.flashf("err", "invalid JSON: %v", err)
			gu.openSingleComposeForMetadata(id, text)
			return nil
		}
		if m == nil {
			// `null` unmarshals cleanly into a nil map. Reject so it
			// doesn't act as a silent metadata-wipe (plan AC #9).
			gu.flashf("err", "invalid JSON: metadata must be a JSON object")
			gu.openSingleComposeForMetadata(id, text)
			return nil
		}
		gu.runDispatch(func() {
			if err := gu.ds.SetMetadata(gu.ctx, id, m); err != nil {
				gu.flashf("err", "metadata: %v", err)
				return
			}
			gu.flashf("info", "metadata updated %s", id)
			gu.refreshAll()
		})
		return nil
	})
}

// openTaskComposeForEdit is the openTaskCompose wrapper for the
// edit flow. Extracted so the validation-error branch (empty title)
// can re-invoke itself with the same (id, summary, description)
// triple — the compose confirm handler resets popup state to
// popupNone before invoking the accept callback, so re-opening from
// inside the callback is the supported way to "keep the popup
// open".
//
// The write itself goes through runDispatch (not gu.g.OnWorker
// directly) so tests can swap in a synchronous dispatcher and
// observe the datasource call without needing a gocui MainLoop.
func (gu *Gui) openTaskComposeForEdit(id, summary, description string) {
	gu.openTaskCompose("Edit "+id, summary, description, func(s, d string) error {
		if strings.TrimSpace(s) == "" {
			gu.flashf("err", "title required")
			gu.openTaskComposeForEdit(id, s, d)
			return nil
		}
		gu.runDispatch(func() {
			if err := gu.ds.UpdateTitleDescription(gu.ctx, id, s, d); err != nil {
				gu.flashf("err", "edit: %v", err)
				return
			}
			gu.flashf("info", "edited %s", id)
			gu.refreshAll()
		})
		return nil
	})
}

func (gu *Gui) taskDone(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("mark %s done?", t.ID), func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusDone); err != nil {
				gu.flashf("err", "done: %v", err)
				return nil
			}
			gu.flashf("info", "done %s", t.ID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

func (gu *Gui) taskCancel(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("cancel %s?", t.ID), func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusCancel); err != nil {
				gu.flashf("err", "cancel: %v", err)
				return nil
			}
			gu.flashf("info", "cancelled %s", t.ID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

func (gu *Gui) taskReopen(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		if err := gu.ds.UpdateStatus(gu.ctx, t.ID, store.StatusNew); err != nil {
			gu.flashf("err", "reopen: %v", err)
			return nil
		}
		gu.flashf("info", "reopened %s", t.ID)
		gu.refreshAll()
		return nil
	})
	return nil
}

// taskClaim was removed: the v0.2 schema has no claim verb. See the
// design plan + the help screen — 'e' enrolls, 'a' (palette) assigns.

func (gu *Gui) taskEnroll(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("enroll "+t.ID+" into workflow name:", "", func(wf string) error {
		wf = strings.TrimSpace(wf)
		if wf == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Enroll(gu.ctx, t.ID, wf); err != nil {
				gu.flashf("err", "enroll: %v", err)
				return nil
			}
			gu.flashf("info", "enroll %s -> %s ok", t.ID, wf)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskResume(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("resume "+t.ID+" to step (empty=current):", "", func(step string) error {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Resume(gu.ctx, t.ID, strings.TrimSpace(step)); err != nil {
				gu.flashf("err", "resume: %v", err)
				return nil
			}
			gu.flashf("info", "resumed %s", t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskBlock(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("block "+t.ID+" by id:", "", func(blocker string) error {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Block(gu.ctx, t.ID, blocker); err != nil {
				gu.flashf("err", "block: %v", err)
				return nil
			}
			gu.flashf("info", "blocked %s <- %s", t.ID, blocker)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskUnblock(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openPrompt("unblock "+t.ID+" from id:", "", func(blocker string) error {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.Unblock(gu.ctx, t.ID, blocker); err != nil {
				gu.flashf("err", "unblock: %v", err)
				return nil
			}
			gu.flashf("info", "unblocked %s <- %s", t.ID, blocker)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskComment(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	// Single-pane multi-line compose: Enter inserts \n, Ctrl-S /
	// Alt-Enter submit, Esc cancels. An empty submit (whitespace
	// only) is a silent cancel — same semantics as the previous
	// one-line prompt the comment popup used.
	gu.openSingleCompose("Comment on "+t.ID, "markdown ok", "", func(text string) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.AddComment(gu.ctx, t.ID, text); err != nil {
				gu.flashf("err", "comment: %v", err)
				return nil
			}
			gu.flashf("info", "comment on %s", t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) taskPriority(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.openMenu("priority for "+t.ID, []string{"0", "1", "2", "3"}, func(i int) error {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.UpdatePriority(gu.ctx, t.ID, i); err != nil {
				gu.flashf("err", "priority: %v", err)
				return nil
			}
			gu.flashf("info", "P%d on %s", i, t.ID)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) workflowNew(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("workflow file path:", "", func(path string) error {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil
		}
		gu.g.OnWorker(func(_ gocui.Task) error {
			name, err := gu.ds.CreateWorkflow(gu.ctx, path)
			if err != nil {
				gu.flashf("err", "workflow new: %v", err)
				return nil
			}
			gu.flashf("info", "workflow %s created", name)
			gu.refreshAll()
			return nil
		})
		return nil
	})
	return nil
}

func (gu *Gui) workflowDelete(*gocui.Gui, *gocui.View) error {
	w, ok := gu.st.selectedWorkflowLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("delete workflow "+w.Name+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.DeleteWorkflow(gu.ctx, w.Name); err != nil {
				gu.flashf("err", "wf delete: %v", err)
				return nil
			}
			gu.flashf("info", "wf %s deleted", w.Name)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

// agentInstall and agentUninstall are intentionally informational in
// v1: the daemon has no /v1/agents endpoint so live mode returns the
// same error as offline. A popup with two no-op options is a fake
// choice; flashf is the right verb — one piece of info, no demand
// for an action that doesn't exist. The hotkeys stay bound so the
// help screen line 'i install / u uninstall' is honest (it points
// to the CLI workaround).
func (gu *Gui) agentInstall(*gocui.Gui, *gocui.View) error {
	gu.flashf("info", "agent install: quit lazy and run 'autosk agent install <pkg>'")
	return nil
}

func (gu *Gui) agentUninstall(*gocui.Gui, *gocui.View) error {
	a, ok := gu.st.selectedAgentLocked()
	name := "<pkg>"
	if ok && a.Name != "" {
		name = a.Name
	}
	gu.flashf("info", "agent uninstall: quit lazy and run 'autosk agent uninstall %s'", name)
	return nil
}

func (gu *Gui) jobCancel(*gocui.Gui, *gocui.View) error {
	j, ok := gu.st.selectedJobLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("cancel job "+j.JobID+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.CancelJob(gu.ctx, j.JobID); err != nil {
				gu.flashf("err", "cancel job: %v", err)
				return nil
			}
			gu.flashf("info", "job %s cancelled", j.JobID)
			gu.refreshAll()
			return nil
		})
	})
	return nil
}

// confirmThen opens a confirm popup; on yes runs f.
func (gu *Gui) confirmThen(prompt string, f func()) {
	gu.openConfirm(prompt, func() error { f(); return nil })
}
