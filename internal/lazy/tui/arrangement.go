package tui

import (
	"github.com/jesseduffield/lazycore/pkg/boxlayout"
)

// Window names referenced by both the arrangement and the layout
// loop. Keep them in one place so renames stay in sync.
const (
	winTasks        = "tasks"
	winJobs         = "jobs"
	winWorkflows    = "workflows"
	winAgents       = "agents"
	winDetail       = "main"
	winJobInput     = "jobInput"
	winLog          = "extras"
	winStatusBar    = "statusbar"
	// winOptionsStrip is the lazygit-style context-aware bindings
	// hint row painted on the very bottom of the screen (one row
	// below the status bar). Each entry is "<key>: <action>" joined
	// with " | "; the focused panel's high-traffic bindings come
	// first, then the global staples.
	winOptionsStrip = "options"
	winFlash        = "flash" // popup flash line (toast)
	winPopupMenu    = "popupMenu"
	// winPopupCheatsheet is the editable list view that backs the
	// new `?` cheatsheet popup (sections + filter + Enter-to-execute).
	winPopupCheatsheet = "popupCheatsheet"
	winPopupConfirm = "popupConfirm"
	winPopupPrompt  = "popupPrompt"
	// Task-compose popup (lazygit-style two-pane commit editor).
	winTaskComposeSummary     = "taskComposeSummary"
	winTaskComposeDescription = "taskComposeDescription"
	// Single-pane multi-line compose popup (comment / metadata).
	winSingleCompose = "singleCompose"
	// Two-pane enroll / resume picker (workflow list on the left,
	// step list on the right). Layout draws both views as framed
	// boxes side-by-side; the focused pane gets the PopupBox accent
	// on its frame.
	winEnrollWorkflowList = "enrollWorkflowList"
	winEnrollStepList     = "enrollStepList"
)

// allPopupWindows is every window name that belongs to a popup. The
// layout pass uses it to garbage-collect any popup view that doesn't
// belong to the currently-active popup kind, so closed popups leave
// no stale frame behind. Keep new popup views listed here or they'll
// linger after the popup that owns them is dismissed.
var allPopupWindows = []string{
	winPopupMenu,
	winPopupConfirm,
	winPopupPrompt,
	winTaskComposeSummary,
	winTaskComposeDescription,
	winSingleCompose,
	winEnrollWorkflowList,
	winEnrollStepList,
	winPopupCheatsheet,
}

// allDashboardWindows is every window name owned by the dashboard
// arrangement (excluding popups and the status bar). The layout
// garbage-collects entries not present in the current frame's
// boxlayout output.
//
// winOptionsStrip lives in the dashboard tree too (the
// boxlayout-aware status bar + options-strip pair) so it is
// allocated and GC'd alongside the other dashboard views.
var allDashboardWindows = []string{
	winTasks, winJobs, winWorkflows, winAgents,
	winDetail, winJobInput, winLog,
}

// ViewState distinguishes the two top-level arrangements.
//
// Kept as an enum (even though only StateDashboard is in use after
// the Inspector removal) so a future second top-level state (e.g.
// a dedicated workflow editor) doesn't have to re-introduce the
// type.
type ViewState int

const (
	StateDashboard ViewState = iota
)

// arrangeArgs is the input to a window-arrangement function. Only the
// fields we actually consume; we don't need lazygit's whole bag.
//
// winJobInput is NOT positioned by the boxlayout tree — it is
// overlaid on top of winDetail's bottom rows by layout.go when the
// selected job is live. boxlayout doesn't support overlapping
// boxes, so we keep arrangement focused on the non-overlapping
// stack (side panels + detail + log + status bar) and inject the
// overlay coordinates downstream.
type arrangeArgs struct {
	width, height int
	focusedSide   string // one of winTasks / winJobs / winWorkflows / winAgents
	state         ViewState
	logHidden     bool
}

// dashboardArrangement returns the Box tree for the standard
// dashboard view: four side panels stacked left, detail+log right,
// then a status bar + options strip pinned across the very bottom
// of the screen.
//
// Status bar + options strip both run Frame=false with Size:1: one
// painted row each, no padding rows above or below. Matches
// lazygit's `infoSectionSize=1` pattern in
// pkg/gui/controllers/helpers/window_arrangement_helper.go.
//
// Size:1 only works because layout.go expands the SetView
// coordinates of these Frame=false views outward by 1 cell on each
// side (lazygit's `frameOffset` trick); otherwise gocui's
// `InnerHeight = Height - 2` quirk would collapse the writeable
// area to zero and the row would render blank. If you ever stop
// applying that offset in layout.go, bump these back to Size:3.
//
// winJobInput does NOT appear in this tree. It is overlaid on top of
// winDetail's bottom rows by layout.go when the selected job is
// live. boxlayout doesn't support overlapping boxes, so the input's
// position is computed in layout.go from dims[winDetail] after
// arrange() runs.
func dashboardArrangement(a arrangeArgs) *boxlayout.Box {
	logSize := 8
	if a.logHidden {
		logSize = 0
	}
	side := sideStack(a.focusedSide)

	rightChildren := []*boxlayout.Box{
		{Window: winDetail, Weight: 3},
	}
	if logSize > 0 {
		rightChildren = append(rightChildren, &boxlayout.Box{Window: winLog, Size: logSize})
	}

	return &boxlayout.Box{
		Direction: boxlayout.ROW,
		Children: []*boxlayout.Box{
			{
				Direction: boxlayout.COLUMN,
				Weight:    1,
				Children: []*boxlayout.Box{
					{Direction: boxlayout.ROW, Children: side, Weight: 1},
					{
						Direction: boxlayout.ROW,
						Weight:    2,
						Children:  rightChildren,
					},
				},
			},
			{Window: winStatusBar, Size: 1},
			{Window: winOptionsStrip, Size: 1},
		},
	}
}

// sideStack returns the four left-panel boxes, growing the focused
// one. Same accordion pattern lazygit uses for its side panels.
func sideStack(focused string) []*boxlayout.Box {
	w := func(name string) int {
		if name == focused {
			return 3
		}
		return 1
	}
	return []*boxlayout.Box{
		{Window: winTasks, Weight: w(winTasks)},
		{Window: winJobs, Weight: w(winJobs)},
		{Window: winWorkflows, Weight: w(winWorkflows)},
		{Window: winAgents, Weight: w(winAgents)},
	}
}

// arrange runs boxlayout on the chosen tree and returns the window
// dimensions map. Pure function; testable in isolation.
func arrange(a arrangeArgs) map[string]boxlayout.Dimensions {
	box := dashboardArrangement(a)
	return boxlayout.ArrangeWindows(box, 0, 0, a.width, a.height)
}
