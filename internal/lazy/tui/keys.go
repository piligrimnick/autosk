package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
)

// bindingSpec describes one TUI keybinding plus the metadata the
// cheatsheet popup and the bottom options strip need to render it.
//
// View / Key / Mod / Handler are the gocui plumbing: bindKeys()
// iterates the slice and pushes each entry through g.SetKeybinding.
// The remaining fields drive the cheatsheet and options-strip
// renderers:
//
//   - Description: human-readable label shown in the cheatsheet's
//     description column. Empty means the binding is hidden from
//     the cheatsheet (use for redundant aliases like Ctrl-C/q quit,
//     or for popup-scoped keys that aren't part of the user's
//     dashboard surface).
//   - Short: optional compact label for the options strip. When
//     empty the strip falls back to Description.
//   - Tag: bucket the cheatsheet groups the entry under. "global"
//     means the binding is in the Global bucket regardless of
//     View. "navigation" means the binding is a per-panel viewport
//     verb (j/k/arrows/page/wheel) and goes into the Navigation
//     bucket. Empty Tag is treated as Local (panel-specific
//     action).
//   - DisplayOnScreen: true to surface the binding on the bottom
//     options strip for whichever panel is focused. Plan picks a
//     small set of high-traffic verbs so the strip stays scannable.
type bindingSpec struct {
	View    string
	Key     any
	Mod     gocui.Modifier
	Handler func(*gocui.Gui, *gocui.View) error

	Description     string
	Short           string
	Tag             string
	DisplayOnScreen bool
}

// bindKeys wires every keybinding the TUI uses. Bindings are
// per-view: the global ones bind ViewName="" (all views).
//
// We split into three tiers:
//
//   - globals (quit, tab switching, palette, filter, help, scope clear)
//   - per-panel (Tasks/Sessions/Workflows/Agents list nav + write verbs)
//   - Detail pane scroll (winDetail) + session-input textarea
//     (winSessionInput) when a non-terminal session is selected
//
// The slice is materialised by bindingSpecs(); bindKeys() is the
// thin loop that registers each entry with gocui.
func (gu *Gui) bindKeys() error {
	for _, b := range gu.bindingSpecs() {
		if err := gu.g.SetKeybinding(b.View, b.Key, b.Mod, b.Handler); err != nil {
			return err
		}
	}
	return nil
}

// bindingSpecs returns the full registry of dashboard keybindings
// plus their cheatsheet / options-strip metadata. Re-built on every
// call (handler closures over `gu` are constructed in-line).
// Callers: bindKeys() (once at startup), openHelp (once per popup
// open), and renderOptionsStrip via renderViews (every redraw —
// per input event and per refresh tick, ~80 closure allocations
// per redraw). That's well within the budget for terminal-frame
// rebuilds; if the strip ever becomes a measurable hotspot the
// slice can be memoised on *Gui (the closures over `gu` are stable
// for the lifetime of the Gui).
func (gu *Gui) bindingSpecs() []bindingSpec {
	return []bindingSpec{
		// global quit + help. Space is intentionally NOT global — it
		// has per-panel semantics (Tasks: commit scope) and a global
		// space binding would swallow the per-view ones underneath.
		//
		// Ctrl-C and `q` both quit; only `q` carries the cheatsheet
		// Description so the row isn't duplicated (lazygit's
		// uniqueBindings dedup at registration time).
		{View: "", Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Handler: gu.quit, Tag: "global"},
		{View: "", Key: 'q', Mod: gocui.ModNone, Handler: gu.quit, Description: "quit", Short: "quit", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: '?', Mod: gocui.ModNone, Handler: gu.openHelp, Description: "help cheatsheet", Short: "help", Tag: "global", DisplayOnScreen: true},
		// Ctrl-W is the "what's new" re-opener: pops the changelog
		// modal with the FULL embedded CHANGELOG.md regardless of
		// `last_seen_changelog`, and does NOT mutate state.json on
		// dismiss. The auto-popup that fires once per release on
		// the first start of a new release uses the same
		// popupChangelog kind but carries an OnDismiss callback;
		// see internal/changelog and cmd/autosk/lazy.go.
		{View: "", Key: gocui.KeyCtrlW, Mod: gocui.ModNone, Handler: gu.handleWhatsNew, Description: "what's new (open CHANGELOG.md)", Short: "what's new", Tag: "global"},
		// Esc is universal across the TUI: it closes the active popup
		// (or, with no popup open, clears the focused panel's filter).
		// Deliberately NOT cheatsheet-visible: selecting a "back /
		// close" row in the cheatsheet would route through handleEsc
		// AFTER the popup is already cleared by cheatsheetAccept,
		// which would wipe the focused panel's filter as a destructive
		// side effect the operator did not signal. Discoverable by
		// trying Esc once.
		{View: "", Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.handleEsc, Tag: "global"},
		// panel switching
		{View: "", Key: '1', Mod: gocui.ModNone, Handler: gu.focusPanel(panelTasks), Description: "focus Tasks", Tag: "global"},
		{View: "", Key: '2', Mod: gocui.ModNone, Handler: gu.focusPanel(panelSessions), Description: "focus Sessions", Tag: "global"},
		{View: "", Key: '3', Mod: gocui.ModNone, Handler: gu.focusPanel(panelWorkflows), Description: "focus Workflows", Tag: "global"},
		{View: "", Key: '4', Mod: gocui.ModNone, Handler: gu.focusPanel(panelAgents), Description: "focus Agents", Tag: "global"},
		{View: "", Key: '0', Mod: gocui.ModNone, Handler: gu.focusPanel(panelDetail), Description: "focus Detail", Tag: "global"},
		{View: "", Key: gocui.KeyTab, Mod: gocui.ModNone, Handler: gu.cyclePanel(+1), Description: "cycle side panel", Tag: "global"},
		// refresh + scope + filter + palette
		{View: "", Key: 'R', Mod: gocui.ModNone, Handler: gu.handleRefresh, Description: "refresh now", Tag: "global"},
		// Ctrl-R is the "hard refresh": call the datasource Reconnect
		// lever and force a full re-fetch. With the RPC datasource
		// Reconnect is a daemon-side no-op (the Go binary holds no
		// store connection), so this is functionally a forced refresh;
		// it survives as the recovery hatch the contract still exposes.
		// Plain R alone fires the same adaptive tick the 2s timer uses.
		{View: "", Key: gocui.KeyCtrlR, Mod: gocui.ModNone, Handler: gu.handleForceReconnect, Description: "hard refresh", Tag: "global"},
		{View: "", Key: '*', Mod: gocui.ModNone, Handler: gu.handleClearScope, Description: "clear scope chips", Short: "clear scope", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: '/', Mod: gocui.ModNone, Handler: gu.openFilter, Description: "filter focused panel", Short: "filter", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: ':', Mod: gocui.ModNone, Handler: gu.openPalette, Description: "command palette", Short: "palette", Tag: "global", DisplayOnScreen: true},
		{View: "", Key: '@', Mod: gocui.ModNone, Handler: gu.toggleLog, Description: "toggle log", Tag: "global"},

		// list nav (per panel; gocui binds per-view, so we attach once
		// per side panel + the detail pane). Only j/k carry the
		// cheatsheet Description — arrow keys + wheel are equivalent
		// aliases and would just duplicate the Navigation bucket if
		// they each surfaced their own row.
		{View: winTasks, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelTasks), Description: "down", Tag: "navigation"},
		{View: winTasks, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelTasks), Description: "up", Tag: "navigation"},
		{View: winTasks, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelTasks), Tag: "navigation"},
		{View: winTasks, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelTasks), Tag: "navigation"},
		{View: winTasks, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.tasksEnter, Description: "filter Sessions + focus"},
		// Space — commit the cursor row as the Sessions-panel scope chip
		// without leaving the Tasks panel. The counterpart to Enter
		// (which commits + jumps). j/k do NOT auto-commit anymore
		// (operator complaint: cursor-driven re-filtering was noisy),
		// so this is the only path on Tasks for setting scope.TaskID.
		// Press * to clear.
		{View: winTasks, Key: ' ', Mod: gocui.ModNone, Handler: gu.tasksScopeFromCursor, Description: "set Sessions scope to selected task"},

		{View: winSessions, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelSessions), Description: "down", Tag: "navigation"},
		{View: winSessions, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelSessions), Description: "up", Tag: "navigation"},
		{View: winSessions, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelSessions), Tag: "navigation"},
		{View: winSessions, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelSessions), Tag: "navigation"},
		{View: winSessions, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.sessionsEnter, Description: "focus input (live) / Detail (terminal)"},

		{View: winWorkflows, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelWorkflows), Description: "down", Tag: "navigation"},
		{View: winWorkflows, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelWorkflows), Description: "up", Tag: "navigation"},
		{View: winWorkflows, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelWorkflows), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelWorkflows), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.workflowsEnter, Description: "filter Tasks by workflow + focus"},

		{View: winAgents, Key: 'j', Mod: gocui.ModNone, Handler: gu.cursorDown(panelAgents), Description: "down", Tag: "navigation"},
		{View: winAgents, Key: 'k', Mod: gocui.ModNone, Handler: gu.cursorUp(panelAgents), Description: "up", Tag: "navigation"},
		{View: winAgents, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cursorDown(panelAgents), Tag: "navigation"},
		{View: winAgents, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cursorUp(panelAgents), Tag: "navigation"},
		// Agents pane is READ-ONLY in v2 (no agent-scope picker); Enter
		// carries no binding so it never surfaces in the cheatsheet.

		// Tasks write verbs.
		{View: winTasks, Key: 'n', Mod: gocui.ModNone, Handler: gu.taskNew, Description: "new task", Short: "new", DisplayOnScreen: true},
		{View: winTasks, Key: 'c', Mod: gocui.ModNone, Handler: gu.taskEdit, Description: "edit task", Short: "edit", DisplayOnScreen: true},
		{View: winTasks, Key: 'd', Mod: gocui.ModNone, Handler: gu.taskDone, Description: "mark done", Short: "done", DisplayOnScreen: true},
		{View: winTasks, Key: 'x', Mod: gocui.ModNone, Handler: gu.taskCancel, Description: "cancel task", Short: "cancel", DisplayOnScreen: true},
		{View: winTasks, Key: 'e', Mod: gocui.ModNone, Handler: gu.taskEnroll, Description: "enroll into workflow", Short: "enroll", DisplayOnScreen: true},
		{View: winTasks, Key: 'r', Mod: gocui.ModNone, Handler: gu.taskResume, Description: "resume (human → work)", Short: "resume", DisplayOnScreen: true},
		{View: winTasks, Key: 'b', Mod: gocui.ModNone, Handler: gu.taskBlock, Description: "add blocker", Short: "block", DisplayOnScreen: true},
		{View: winTasks, Key: 'u', Mod: gocui.ModNone, Handler: gu.taskUnblock, Description: "remove blocker", Short: "unblock", DisplayOnScreen: true},
		{View: winTasks, Key: 'm', Mod: gocui.ModNone, Handler: gu.taskComment, Description: "add comment", Short: "comment", DisplayOnScreen: true},
		{View: winTasks, Key: 'M', Mod: gocui.ModNone, Handler: gu.taskEditMetadata, Description: "edit metadata ($EDITOR)", Short: "metadata"},
		{View: winTasks, Key: 'o', Mod: gocui.ModNone, Handler: gu.taskReopen, Description: "reopen task", Short: "reopen", DisplayOnScreen: true},
		// Note: there is no `c claim` binding. The v0.2 schema has no
		// claim verb — tasks self-advance via workflow steps. `c` is
		// bound to `change` (edit title + description in the two-pane
		// compose); use `e` to enroll instead.

		// Workflows + Agents panes are READ-ONLY in v2 (workflows/agents are
		// code registered by extensions), so they carry no write verbs.

		// Sessions hotkeys.
		{View: winSessions, Key: 'K', Mod: gocui.ModNone, Handler: gu.sessionAbort, Description: "abort session", Short: "abort", DisplayOnScreen: true},

		// Detail pane scroll bindings. Same shape that used to live on
		// the Inspector body view — j/k single line, Ctrl-F/Ctrl-B page,
		// g/G start/end. Applies to BOTH task-detail and session-detail
		// content (the pane is one view; the renderer picks the body
		// based on focused panel).
		{View: winDetail, Key: 'j', Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Description: "line down", Tag: "navigation"},
		{View: winDetail, Key: 'k', Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Description: "line up", Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyCtrlF, Mod: gocui.ModNone, Handler: gu.detailScrollPage(+1), Description: "page down", Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyCtrlB, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Description: "page up", Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyPgdn, Mod: gocui.ModNone, Handler: gu.detailScrollPage(+1), Tag: "navigation"},
		{View: winDetail, Key: gocui.KeyPgup, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Tag: "navigation"},
		{View: winDetail, Key: 'g', Mod: gocui.ModNone, Handler: gu.detailScrollTo(false), Description: "top", Tag: "navigation"},
		{View: winDetail, Key: 'G', Mod: gocui.ModNone, Handler: gu.detailScrollTo(true), Description: "bottom", Tag: "navigation"},
		{View: winDetail, Key: 'M', Mod: gocui.ModNone, Handler: gu.taskEditMetadata, Description: "edit metadata ($EDITOR)", Short: "metadata"},

		// Session-input bindings (relocated from the old fullscreen-inspector input view).
		// Bound only on winSessionInput; the view itself only exists when
		// the selected session is running, so these bindings are dormant
		// otherwise.
		{View: winSessionInput, Key: gocui.KeyCtrlD, Mod: gocui.ModNone, Handler: gu.liveSend, Description: "send"},
		{View: winSessionInput, Key: gocui.KeyCtrlF, Mod: gocui.ModNone, Handler: gu.liveFollowUp, Description: "follow up"},
		{View: winSessionInput, Key: gocui.KeyCtrlA, Mod: gocui.ModNone, Handler: gu.liveAbort, Description: "abort agent turn"},
		{View: winSessionInput, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.sessionInputEscape, Description: "cancel input"},
		{View: winSessionInput, Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Handler: gu.quit},
		// Scroll the Detail pane above without leaving the textarea.
		{View: winSessionInput, Key: gocui.KeyCtrlB, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Description: "page up Detail", Tag: "navigation"},
		{View: winSessionInput, Key: gocui.KeyPgup, Mod: gocui.ModNone, Handler: gu.detailScrollPage(-1), Tag: "navigation"},
		{View: winSessionInput, Key: gocui.KeyPgdn, Mod: gocui.ModNone, Handler: gu.detailScrollPage(+1), Description: "page down Detail", Tag: "navigation"},
		{View: winSessionInput, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Tag: "navigation"},
		{View: winSessionInput, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Tag: "navigation"},

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
		{View: winTasks, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelTasks, +panelScrollStep), Tag: "navigation"},
		{View: winTasks, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelTasks, -panelScrollStep), Tag: "navigation"},
		{View: winSessions, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelSessions, +panelScrollStep), Tag: "navigation"},
		{View: winSessions, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelSessions, -panelScrollStep), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelWorkflows, +panelScrollStep), Tag: "navigation"},
		{View: winWorkflows, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelWorkflows, -panelScrollStep), Tag: "navigation"},
		{View: winAgents, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.panelScroll(panelAgents, +panelScrollStep), Tag: "navigation"},
		{View: winAgents, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.panelScroll(panelAgents, -panelScrollStep), Tag: "navigation"},

		// Detail pane: wheel scrolls the viewport (no cursor concept).
		{View: winDetail, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.detailScroll(+1), Tag: "navigation"},
		{View: winDetail, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.detailScroll(-1), Tag: "navigation"},

		// Popup menus also benefit from wheel-driven cursor motion.
		{View: winPopupMenu, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.popupCursor(+1)},
		{View: winPopupMenu, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.popupCursor(-1)},

		// Popup keys.
		{View: winPopupMenu, Key: 'j', Mod: gocui.ModNone, Handler: gu.popupCursor(+1)},
		{View: winPopupMenu, Key: 'k', Mod: gocui.ModNone, Handler: gu.popupCursor(-1)},
		{View: winPopupMenu, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.popupCursor(+1)},
		{View: winPopupMenu, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.popupCursor(-1)},
		{View: winPopupMenu, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.popupAccept},
		{View: winPopupMenu, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupConfirm, Key: 'y', Mod: gocui.ModNone, Handler: gu.popupConfirmYes},
		{View: winPopupConfirm, Key: 'Y', Mod: gocui.ModNone, Handler: gu.popupConfirmYes},
		{View: winPopupConfirm, Key: 'n', Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupConfirm, Key: 'N', Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupConfirm, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.popupConfirmYes},
		{View: winPopupConfirm, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winPopupPrompt, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.popupAccept},
		{View: winPopupPrompt, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},

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
		// requires Ctrl+S. Tab toggles back to Summary, Esc closes.
		{View: winTaskComposeSummary, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.taskComposeConfirm},
		{View: winTaskComposeSummary, Key: gocui.KeyCtrlS, Mod: gocui.ModNone, Handler: gu.taskComposeConfirm},
		{View: winTaskComposeSummary, Key: gocui.KeyTab, Mod: gocui.ModNone, Handler: gu.taskComposeToggle},
		{View: winTaskComposeSummary, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winTaskComposeDescription, Key: gocui.KeyCtrlS, Mod: gocui.ModNone, Handler: gu.taskComposeConfirm},
		{View: winTaskComposeDescription, Key: gocui.KeyTab, Mod: gocui.ModNone, Handler: gu.taskComposeToggle},
		{View: winTaskComposeDescription, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},

		// Single-pane multi-line compose popup (comment / metadata).
		//
		// Plain Enter is intentionally NOT bound — it falls through to
		// gocui.DefaultEditor which inserts "\n", which is the whole
		// point of the multi-line popup. Submitting requires Ctrl+S.
		// Esc cancels without invoking OnAccept.
		{View: winSingleCompose, Key: gocui.KeyCtrlS, Mod: gocui.ModNone, Handler: gu.singleComposeConfirm},
		{View: winSingleCompose, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},
		{View: winSingleCompose, Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Handler: gu.quit},

		// Enroll / resume two-pane picker.
		//
		// Workflow pane: j/k/arrows + mouse wheel move the cursor and
		// re-render the step pane (no datasource call). Enter confirms
		// the workflow and moves focus to the step pane (with the step
		// cursor reset to 0). Esc closes the popup without dispatching.
		{View: winEnrollWorkflowList, Key: 'j', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, +1)},
		{View: winEnrollWorkflowList, Key: 'k', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, -1)},
		{View: winEnrollWorkflowList, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, +1)},
		{View: winEnrollWorkflowList, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, -1)},
		{View: winEnrollWorkflowList, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, +1)},
		{View: winEnrollWorkflowList, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneWorkflow, -1)},
		{View: winEnrollWorkflowList, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.enrollPickerWorkflowAccept},
		{View: winEnrollWorkflowList, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.popupClose},

		// Step pane: j/k/arrows + mouse wheel move the cursor. Enter
		// fires OnPick with (workflow, step) names. Esc returns focus
		// to the workflow pane (preserving the workflow cursor); on
		// the resume flow (WorkflowLocked) Esc falls through to
		// popupClose instead because there is no workflow pane to
		// return to.
		{View: winEnrollStepList, Key: 'j', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, +1)},
		{View: winEnrollStepList, Key: 'k', Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, -1)},
		{View: winEnrollStepList, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, +1)},
		{View: winEnrollStepList, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, -1)},
		{View: winEnrollStepList, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, +1)},
		{View: winEnrollStepList, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.enrollPickerCursor(pickerPaneStep, -1)},
		{View: winEnrollStepList, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.enrollPickerStepAccept},
		{View: winEnrollStepList, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.enrollPickerStepEscape},

		// Cheatsheet popup (winPopupCheatsheet). The view is editable
		// so printable runes flow through its Editor and append to
		// state.popup.CheatsheetFilter; non-char keys (Enter / Esc /
		// Backspace / arrows / wheel) are routed via these view-scoped
		// keybindings. gocui's matchView rejects char bindings on
		// editable views, so we don't need to enumerate every printable
		// rune here — the editor is the one source of truth for them.
		{View: winPopupCheatsheet, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.cheatsheetAccept},
		{View: winPopupCheatsheet, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.cheatsheetEscape},
		{View: winPopupCheatsheet, Key: gocui.KeyBackspace, Mod: gocui.ModNone, Handler: gu.cheatsheetBackspace},
		{View: winPopupCheatsheet, Key: gocui.KeyBackspace2, Mod: gocui.ModNone, Handler: gu.cheatsheetBackspace},
		{View: winPopupCheatsheet, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(+1)},
		{View: winPopupCheatsheet, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(-1)},
		{View: winPopupCheatsheet, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(+1)},
		{View: winPopupCheatsheet, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.cheatsheetCursor(-1)},

		// Changelog popup (winPopupChangelog). Same scroll key set as
		// the Detail pane (j/k/arrows/ctrl-f/ctrl-b/pgup/pgdn/g/G +
		// mouse wheel) so the operator browses the body with muscle
		// memory; Esc / Enter dismisses (firing OnDismissChangelog
		// once on the auto-popup path so last_seen_changelog gets
		// stamped into ~/.autosk/state.json). The bindings carry no
		// Description so they don't pollute the cheatsheet — the
		// popup itself shows a scroll/dismiss hint in its top frame.
		{View: winPopupChangelog, Key: 'j', Mod: gocui.ModNone, Handler: gu.changelogScroll(+1)},
		{View: winPopupChangelog, Key: 'k', Mod: gocui.ModNone, Handler: gu.changelogScroll(-1)},
		{View: winPopupChangelog, Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Handler: gu.changelogScroll(+1)},
		{View: winPopupChangelog, Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Handler: gu.changelogScroll(-1)},
		{View: winPopupChangelog, Key: gocui.KeyCtrlF, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(+1)},
		{View: winPopupChangelog, Key: gocui.KeyCtrlB, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(-1)},
		{View: winPopupChangelog, Key: gocui.KeyPgdn, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(+1)},
		{View: winPopupChangelog, Key: gocui.KeyPgup, Mod: gocui.ModNone, Handler: gu.changelogScrollPage(-1)},
		{View: winPopupChangelog, Key: 'g', Mod: gocui.ModNone, Handler: gu.changelogScrollTo(false)},
		{View: winPopupChangelog, Key: 'G', Mod: gocui.ModNone, Handler: gu.changelogScrollTo(true)},
		{View: winPopupChangelog, Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Handler: gu.changelogScroll(+1)},
		{View: winPopupChangelog, Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Handler: gu.changelogScroll(-1)},
		{View: winPopupChangelog, Key: gocui.KeyEsc, Mod: gocui.ModNone, Handler: gu.changelogDismiss},
		{View: winPopupChangelog, Key: gocui.KeyEnter, Mod: gocui.ModNone, Handler: gu.changelogDismiss},
	}
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
// The stash normalises panelSessionInput → panelSessions so a
// transition from the input flow into the Detail pane still renders
// Session Detail (renderDetail also normalises on the read side, but
// the model field stays sensible this way too).
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
			if p == panelSessions {
				// Hydrate the transcript cache for the now-selected session.
				// Without this the Detail pane stays on "(loading...)"
				// until the operator presses j/k — see settleOnPanelSessions.
				gu.settleOnPanelSessions()
			}
		}
		return nil
	}
}

// cyclePanel cycles through the four side panels by step.
//
// Source-side normalisation: cyclePanel may fire from any view,
// including the synthetic panelSessionInput (caret in winSessionInput)
// and panelDetail (focus parked on the right pane via '0'). Neither
// is in the 0..3 cycle range, so we normalise them first —
// panelSessionInput→panelSessions (predictable: the operator was
// working a running session, so Tab steps OFF the session they were
// composing for to the next side panel), panelDetail→detailFocus (the
// side panel that owned the selection before '0' was pressed).
// Without this normalisation, (int(panelSessionInput) + 1) % 4 would
// skip the Sessions column entirely on Tab from the input — a UX
// regression vs. the pre-redesign state where Tab inside the
// fullscreen inspector was scoped to inspector-tab cycling.
func (gu *Gui) cyclePanel(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var landed panelID
		changed := false
		gu.st.withLock(func() {
			src := gu.st.focused
			switch src {
			case panelSessionInput:
				src = panelSessions
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
			if landed == panelSessions {
				// Same hydration reason as focusPanel: Tab/Shift-Tab
				// landing on Sessions without moving the cursor would
				// otherwise leave the Detail pane stuck on
				// "(loading...)". See settleOnPanelSessions.
				gu.settleOnPanelSessions()
			}
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
			case panelSessions:
				before = gu.st.sessionCursor
				gu.st.sessionCursor = clampCursor(gu.st.sessionCursor+1, len(gu.st.sessions))
				after = gu.st.sessionCursor
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
			case panelSessions:
				before = gu.st.sessionCursor
				gu.st.sessionCursor = clampCursor(gu.st.sessionCursor-1, len(gu.st.sessions))
				after = gu.st.sessionCursor
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
//     the highlighted task's comments.
//   - panelWorkflows: applyScope so the Tasks panel auto-filters
//     to the highlighted workflow.
//   - panelSessions: delegates to settleOnPanelSessions — the same
//     per-session side-effect block that focusPanel('2'), cyclePanel
//     Tab, and tasksEnter invoke when focus LANDS on Sessions without
//     a cursor move. The chain is:
//     afterCursorMove(panelSessions)
//     → settleOnPanelSessions
//     → scheduleSessionLive (debounced timer)
//     → scheduleSessionArchive (eager OnWorker dispatch, coalesced).
//     The Detail pane reads state.sessionTranscript[sessionID] every
//     frame; the session-event pump appends into it.
//   - panelAgents: scheduleRefresh only.
func (gu *Gui) afterCursorMove(p panelID) {
	switch p {
	case panelWorkflows:
		gu.applyScope()
	case panelSessions:
		gu.settleOnPanelSessions()
	default:
		gu.scheduleRefresh()
	}
}

// settleOnPanelSessions runs the per-session side effects that must
// follow the operator arriving on the Sessions panel — whether by a
// j/k cursor move OR by a focus change that LANDS on panelSessions
// (focusPanel '2', cyclePanel Tab/Shift-Tab landing on Sessions,
// tasksEnter on a task).
//
// Without this on the focus-change paths the Detail pane would stay
// stuck on "(loading...)" until the operator pressed j/k:
// scheduleSessionArchive is the only thing that seeds
// state.sessionTranscript for a fresh selection, and afterCursorMove
// was the only caller, so arriving at panelSessions without moving
// the cursor left the transcript cache unhydrated. Reusing the same
// body keeps cursor moves and focus moves observationally equivalent
// — both arm the live session-event stream + archive fetch and both
// drop a stale input draft authored against a different session.
func (gu *Gui) settleOnPanelSessions() {
	gu.scheduleRefresh()
	if s, ok := gu.st.selectedSessionLocked(); ok {
		running := isSessionRunning(s)
		// Discard any input draft that belongs to a different session
		// BEFORE we kick the live/archive scheduling — the next
		// renderViews pass will repopulate the view from
		// state.sessionInput, which is now empty for this session.
		gu.clearSessionInputIfStale(s.ID)
		gu.scheduleSessionLive(s.ID, running)
		gu.scheduleSessionArchive(s.ID, running)
	} else {
		gu.clearSessionInputIfStale("")
		gu.stopSessionLive()
	}
}

// tasksScopeFromCursor is the Space-key handler on the Tasks panel:
// commits the current cursor row as scope.TaskID (filtering the
// Sessions panel) without changing focus. The complement to tasksEnter
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
				gu.st.scope.WorkflowName = w.Name
			} else {
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

// handleForceReconnect is the Ctrl-R handler: clear the per-session
// transcript cache (so the next selection re-hydrates from a fresh
// archive read), tear down any active session-event subscription,
// call the datasource Reconnect lever, and schedule a refresh. With
// the RPC datasource Reconnect is a daemon-side no-op, so this is
// effectively a forced re-fetch + transcript-cache flush. The
// Reconnect call happens on a worker so the UI thread stays
// responsive. On error we still fall through to scheduleRefresh — a
// stale read is better than no read.
func (gu *Gui) handleForceReconnect(*gocui.Gui, *gocui.View) error {
	gu.flashf("info", "reconnect")
	gu.stopSessionLive()
	gu.st.withLock(func() {
		gu.st.sessionTranscript = map[string]*sessionTranscriptEntry{}
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
// chips. Esc on winSessionInput is bound directly to
// sessionInputEscape so it doesn't route through here.
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
		case panelSessions:
			gu.st.filter.Sessions = ""
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

// tasksEnter cross-links to Sessions (filter by this task) and focuses Sessions.
func (gu *Gui) tasksEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelSessions })
	// Hydrate the Detail pane for the now-selected session. Without this
	// the operator lands on the Sessions panel with the transcript stuck
	// on "(loading...)" until they press j/k — see settleOnPanelSessions.
	gu.settleOnPanelSessions()
	return nil
}

// workflowsEnter cross-links to Tasks (filter by this workflow).
func (gu *Gui) workflowsEnter(*gocui.Gui, *gocui.View) error {
	gu.applyScope()
	gu.st.withLock(func() { gu.st.focused = panelTasks })
	return nil
}

// sessionsEnter focuses the appropriate view for the highlighted session:
//   - non-terminal session (queued or running, daemon accepts SessionInput)
//     → winSessionInput, caret blinks inside the textarea.
//   - terminal session (no SessionInput dispatch surface) → logical focus
//     to panelDetail so j/k scroll the transcript above.
//
// The visibility predicate (isSessionLive) intentionally mirrors the
// layout-pass gate on winSessionInput: Enter only routes to the input
// view when that view actually exists this frame.
//
// On the live-session branch we set state.focused = panelSessionInput
// (a synthetic focus identity whose window() is winSessionInput). The
// layout pass calls g.SetCurrentView(focused.window()) on every
// frame; without recording the focus in the model the next layout
// would yank current-view back to winSessions within ~100ms (spinner
// tick cadence) and the caret would jump out of the textarea.
//
// On the terminal-session branch we stash detailFocus = panelSessions so
// renderDetail keeps emitting the Session Detail body even though
// focused now reads panelDetail.
func (gu *Gui) sessionsEnter(*gocui.Gui, *gocui.View) error {
	s, ok := gu.st.selectedSessionLocked()
	if !ok {
		return nil
	}
	if isSessionLive(s) {
		gu.st.withLock(func() { gu.st.focused = panelSessionInput })
		// Also SetCurrentView synchronously so tests that don't
		// drive a layout pass between sessionsEnter and the assertion
		// observe the focus change immediately. The layout pass
		// would otherwise be the one to actually move the caret.
		if gu.g != nil {
			_, _ = gu.g.SetCurrentView(winSessionInput)
		}
		return nil
	}
	gu.st.withLock(func() {
		gu.st.detailFocus = panelSessions
		gu.st.focused = panelDetail
	})
	return nil
}

// openFilter opens the per-panel filter prompt.
func (gu *Gui) openFilter(*gocui.Gui, *gocui.View) error {
	gu.openPrompt("filter: status:done wf:name + free text", "", func(value string) error {
		gu.st.withLock(func() {
			switch gu.st.focused {
			case panelTasks:
				gu.st.filter.Tasks = value
			case panelSessions:
				gu.st.filter.Sessions = value
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
		"task resume",
		"task enroll",
		"task block",
		"task unblock",
		"task comment",
		"session abort",
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
	case "session abort":
		_ = gu.sessionAbort(nil, nil)
	case "scope clear":
		_ = gu.handleClearScope(nil, nil)
	case "refresh":
		_ = gu.handleRefresh(nil, nil)
	case "quit":
		gu.cancel()
		gu.g.Update(func(_ *gocui.Gui) error { return gocui.ErrQuit })
	}
}

// openHelp opens the lazygit-style sectioned + filterable
// cheatsheet popup. The bucket build is driven entirely by the
// bindingSpecs() registry — nothing in the help text is
// hand-curated anymore.
//
// Bucketing rules (lazygit's uniqueBindings parity):
//
//   - Local      = entries whose View == focused panel's window AND
//     Tag != "navigation".
//   - Global     = entries with Tag == "global" (or View == "").
//   - Navigation = entries whose View == focused panel's window AND
//     Tag == "navigation".
//
// Dedup is by Description within bucket (keep first occurrence) so
// equivalent aliases (j vs ArrowDown, q vs Ctrl-C) only surface
// once. Bindings whose Description is empty are excluded entirely
// — that's how we hide popup-scoped registrations and other
// non-discoverable plumbing.
func (gu *Gui) openHelp(*gocui.Gui, *gocui.View) error {
	var focused panelID
	gu.st.withRLock(func() {
		focused = gu.st.focused
	})
	gu.openCheatsheet(focused)
	return nil
}

// openCheatsheet builds the sectioned bucket of items for the given
// focused panel and seeds the popup state. Pulled out of openHelp
// so unit tests can drive the build directly without going through
// the openHelp keybinding handler.
func (gu *Gui) openCheatsheet(focused panelID) {
	specs := gu.bindingSpecs()
	items := buildCheatsheetItems(specs, focused)
	// Routes through replacePopup so any active popupChangelog
	// auto-popup gets its OnDismissChangelog fired before this
	// popup replaces it (review R10) — same contract as openMenu.
	gu.replacePopup(popupState{
		Kind:              popupCheatsheet,
		Title:             "Keybindings — type to filter · enter executes · esc close",
		CheatsheetItems:   items,
		CheatsheetFilter:  "",
		CheatsheetCursor:  0,
		CheatsheetFocused: focused,
	})
	gu.requestRedraw()
}

// buildCheatsheetItems assembles the section markers + binding rows
// for the cheatsheet given the binding spec registry and the
// currently focused panel. Pure function so the bucketing /
// deduplication contracts (AC2) can be exercised without a gui.
//
// The returned slice is the FULL list (header markers + non-header
// rows). The popup applies the filter at render time — the items
// slice is captured once at open and never mutated for the
// lifetime of the popup.
func buildCheatsheetItems(specs []bindingSpec, focused panelID) []cheatsheetItem {
	focusedWin := focused.window()

	dedup := map[string]map[string]bool{}
	local := []cheatsheetItem{}
	global := []cheatsheetItem{}
	navigation := []cheatsheetItem{}

	push := func(section string, dst *[]cheatsheetItem, sp bindingSpec) {
		if sp.Description == "" {
			return
		}
		if dedup[section] == nil {
			dedup[section] = map[string]bool{}
		}
		if dedup[section][sp.Description] {
			return
		}
		dedup[section][sp.Description] = true
		handler := sp.Handler
		item := cheatsheetItem{
			Section:     section,
			KeyLabel:    keyLabel(sp.Key),
			Description: sp.Description,
			Handler: func() error {
				if handler == nil {
					return nil
				}
				return handler(nil, nil)
			},
		}
		*dst = append(*dst, item)
	}

	for _, sp := range specs {
		isGlobal := sp.Tag == "global" || sp.View == ""
		isFocusedView := focusedWin != "" && sp.View == focusedWin
		switch {
		case isFocusedView && sp.Tag == "navigation":
			push("Navigation", &navigation, sp)
		case isFocusedView:
			push("Local", &local, sp)
		case isGlobal:
			push("Global", &global, sp)
		}
	}

	var items []cheatsheetItem
	appendBucket := func(section string, rows []cheatsheetItem) {
		if len(rows) == 0 {
			return
		}
		items = append(items, cheatsheetItem{IsHeader: true, Section: section})
		items = append(items, rows...)
	}
	appendBucket("Local", local)
	appendBucket("Global", global)
	appendBucket("Navigation", navigation)
	return items
}

// keyLabel renders a binding key (rune or gocui.Key) as the
// canonical operator-facing string. Project rule (kept stable
// across the cheatsheet, the options strip, and any future
// inline hint): plain letters are lowercase; uppercase letters
// mean shift+letter; modifier chords use the `ctrl+x` /
// `alt+enter` shape.
func keyLabel(k any) string {
	switch v := k.(type) {
	case rune:
		switch v {
		case ' ':
			return "space"
		}
		return string(v)
	case gocui.Key:
		switch v {
		case gocui.KeyEnter:
			return "enter"
		case gocui.KeyEsc:
			return "esc"
		case gocui.KeyTab:
			return "tab"
		case gocui.KeyArrowUp:
			return "↑"
		case gocui.KeyArrowDown:
			return "↓"
		case gocui.KeyArrowLeft:
			return "←"
		case gocui.KeyArrowRight:
			return "→"
		case gocui.KeyPgup:
			return "pgup"
		case gocui.KeyPgdn:
			return "pgdn"
		case gocui.KeyHome:
			return "home"
		case gocui.KeyEnd:
			return "end"
		case gocui.KeySpace:
			return "space"
		case gocui.KeyBackspace, gocui.KeyBackspace2:
			return "backspace"
		case gocui.KeyDelete:
			return "delete"
		case gocui.KeyCtrlA:
			return "ctrl+a"
		case gocui.KeyCtrlB:
			return "ctrl+b"
		case gocui.KeyCtrlC:
			return "ctrl+c"
		case gocui.KeyCtrlD:
			return "ctrl+d"
		case gocui.KeyCtrlE:
			return "ctrl+e"
		case gocui.KeyCtrlF:
			return "ctrl+f"
		case gocui.KeyCtrlR:
			return "ctrl+r"
		case gocui.KeyCtrlS:
			return "ctrl+s"
		case gocui.KeyCtrlU:
			return "ctrl+u"
		case gocui.KeyCtrlW:
			return "ctrl+w"
		case gocui.MouseWheelUp:
			return "wheel↑"
		case gocui.MouseWheelDown:
			return "wheel↓"
		}
	}
	return fmt.Sprintf("%v", k)
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
			id, err := gu.ds.CreateTask(gu.ctx, summary, description)
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
			if err := gu.ds.UpdateTask(gu.ctx, id, &s, &d); err != nil {
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
		gu.runTerminalVerb("done", t.ID, false)
	})
	return nil
}

func (gu *Gui) taskCancel(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.confirmThen(fmt.Sprintf("cancel %s?", t.ID), func() {
		gu.runTerminalVerb("cancel", t.ID, false)
	})
	return nil
}

// runTerminalVerb drives done/cancel on a worker. On a clean transition the
// daemon reaps the task's worktree (branch preserved). If the worktree is dirty
// and `force` was not set, the daemon refuses with ErrEnvironmentDirty; we then
// prompt to retry with force (which discards the uncommitted changes).
func (gu *Gui) runTerminalVerb(verb, id string, force bool) {
	gu.g.OnWorker(func(_ gocui.Task) error {
		var err error
		if verb == "done" {
			err = gu.ds.TaskDone(gu.ctx, id, force)
		} else {
			err = gu.ds.TaskCancel(gu.ctx, id, force)
		}
		if err != nil {
			if !force && errors.Is(err, datasource.ErrEnvironmentDirty) {
				gu.confirmThen(
					fmt.Sprintf("%s: isolation environment has uncommitted changes — %s --force (discards them)?", id, verb),
					func() { gu.runTerminalVerb(verb, id, true) },
				)
				return nil
			}
			gu.flashf("err", "%s: %v", verb, err)
			return nil
		}
		if verb == "done" {
			gu.flashf("info", "done %s", id)
		} else {
			gu.flashf("info", "cancelled %s", id)
		}
		gu.refreshAll()
		return nil
	})
}

func (gu *Gui) taskReopen(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		if err := gu.ds.TaskReopen(gu.ctx, t.ID); err != nil {
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
// design plan + the help screen — 'e' enrolls into a workflow; there is
// no separate assign verb anymore.

// taskEnroll opens the two-pane workflow + step picker. On open the
// workflow cursor pre-selects the task's current workflow when it
// exists in the cached slice; otherwise row 0. Enter on the step
// pane dispatches Datasource.Enroll(ctx, taskID, wfName, stepName).
func (gu *Gui) taskEnroll(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	var wfs []datasource.Workflow
	gu.st.withRLock(func() {
		wfs = filterPickerWorkflows(gu.st.workflows)
	})
	if len(wfs) == 0 {
		gu.flashf("info", "no workflows defined")
		return nil
	}
	wfCursor := pickerInitialWorkflowCursor(wfs, t.WorkflowName)
	stepCursor := pickerInitialStepCursor(wfs, wfCursor, t.StepName)
	taskID := t.ID
	gu.openEnrollPicker(
		"Enroll "+taskID+" — pick workflow",
		wfs, wfCursor, stepCursor, false,
		func(wfName, stepName string) error {
			gu.runDispatch(func() {
				if err := gu.ds.EnrollWorkflow(gu.ctx, taskID, wfName); err != nil {
					gu.flashf("err", "enroll: %v", err)
					return
				}
				gu.flashf("info", "enroll %s -> %s:%s ok", taskID, wfName, stepName)
				gu.refreshAll()
			})
			return nil
		})
	return nil
}

// taskResume opens the same picker with the workflow pane locked to
// the task's current workflow (resolved from the cached workflows
// slice). Focus starts on the step pane. Enter dispatches
// Datasource.Resume(ctx, taskID, stepName). If the task has no
// current workflow in the cached slice (e.g. stale cache; the task
// has WorkflowID == "") the picker is NOT opened and a flash hints
// at a refresh.
//
// CLI-parity for the no-bump path (review R7): the CLI has two
// distinct resume modes:
//
//   - `autosk resume <id>`           — status-only flip
//     (StatusHuman → StatusWork) at the current step. Routes
//     through Resume(id, "").
//   - `autosk resume <id> --to STEP` — deliberate transition to a
//     sibling step (runs the workflow's onTransit). Routes through
//     Resume(id, STEP).
//
// The picker always pre-selects the task's current step, so the
// natural "just resume" gesture (Enter on the pre-selected row)
// would otherwise dispatch the --to path against the same step the
// operator was parked on, re-running onTransit needlessly. To
// restore CLI parity we collapse "picked step == current step" to
// the status-only branch; picking a DIFFERENT step still transits.
func (gu *Gui) taskResume(*gocui.Gui, *gocui.View) error {
	t, ok := gu.st.selectedTaskLocked()
	if !ok {
		return nil
	}
	if t.WorkflowName == "" {
		gu.flashf("warn", "task has no workflow; enroll first")
		return nil
	}
	var wf *datasource.Workflow
	gu.st.withRLock(func() {
		for i := range gu.st.workflows {
			if gu.st.workflows[i].Name == t.WorkflowName {
				w := gu.st.workflows[i]
				wf = &w
				break
			}
		}
	})
	if wf == nil {
		gu.flashf("warn", "task workflow not loaded; refresh and retry")
		return nil
	}
	wfs := []datasource.Workflow{*wf}
	stepCursor := pickerInitialStepCursor(wfs, 0, t.StepName)
	taskID := t.ID
	currentStepName := resumeCurrentStepName(*wf, t.StepName)
	gu.openEnrollPicker(
		"Resume "+taskID+" — pick step",
		wfs, 0, stepCursor, true,
		func(_, stepName string) error {
			// CLI parity: Enter on the pre-selected current step
			// is the no-bump path (Resume(id, "")). Picking a
			// different step routes through the bumping --to STEP
			// path. resumeCurrentStepName returns "" when the task
			// has no current step (or it isn't in the workflow's
			// step list), which can't collide with any real step
			// name — so the equality test stays inert in that case
			// and Resume(id, stepName) takes the --to path.
			toStep := stepName
			noBump := currentStepName != "" && stepName == currentStepName
			if noBump {
				toStep = ""
			}
			gu.runDispatch(func() {
				if err := gu.ds.Resume(gu.ctx, taskID, toStep); err != nil {
					gu.flashf("err", "resume: %v", err)
					return
				}
				if noBump {
					gu.flashf("info", "resumed %s (no transition)", taskID)
				} else {
					gu.flashf("info", "resumed %s -> %s", taskID, stepName)
				}
				gu.refreshAll()
			})
			return nil
		})
	return nil
}

// resumeCurrentStepName returns the human-readable name of the
// task's current step inside wf, or "" when the task has no
// current step OR the step id isn't present in the workflow's
// step list (stale cache / hand-edit). Used by taskResume to
// detect the no-bump branch (review R7).
func resumeCurrentStepName(wf datasource.Workflow, currentStepID string) string {
	if currentStepID == "" {
		return ""
	}
	for _, s := range wf.Steps {
		if s.Name == currentStepID {
			return s.Name
		}
	}
	return ""
}

// pickerInitialWorkflowCursor returns the index of the workflow row
// matching workflowID, or 0 when no match exists (empty workflowID
// or stale cache). Pure helper so the open-flow tests can pin the
// pre-selection logic without driving the full handler chain.
func pickerInitialWorkflowCursor(wfs []datasource.Workflow, workflowID string) int {
	if workflowID == "" {
		return 0
	}
	for i := range wfs {
		if wfs[i].Name == workflowID {
			return i
		}
	}
	return 0
}

// pickerInitialStepCursor returns the index of the step row matching
// stepID inside workflows[wfCursor].Steps, or 0 when no match
// exists. Symmetric to pickerInitialWorkflowCursor and pulled out
// for the same testing-isolation reason.
func pickerInitialStepCursor(wfs []datasource.Workflow, wfCursor int, stepID string) int {
	if stepID == "" || wfCursor < 0 || wfCursor >= len(wfs) {
		return 0
	}
	for i, s := range wfs[wfCursor].Steps {
		if s.Name == stepID {
			return i
		}
	}
	return 0
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
	// Single-pane multi-line compose: Enter inserts \n, Ctrl+S
	// submits, Esc cancels. An empty submit (whitespace only) is a
	// silent cancel — same semantics as the previous one-line
	// prompt the comment popup used.
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

func (gu *Gui) sessionAbort(*gocui.Gui, *gocui.View) error {
	s, ok := gu.st.selectedSessionLocked()
	if !ok {
		return nil
	}
	gu.confirmThen("abort session "+s.ID+"?", func() {
		gu.g.OnWorker(func(_ gocui.Task) error {
			if err := gu.ds.AbortSession(gu.ctx, s.ID); err != nil {
				gu.flashf("err", "abort session: %v", err)
				return nil
			}
			gu.flashf("info", "session %s aborted", s.ID)
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
