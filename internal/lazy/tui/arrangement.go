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
	winFlash        = "flash" // popup flash line (toast)
	winPopupMenu    = "popupMenu"
	winPopupConfirm = "popupConfirm"
	winPopupPrompt  = "popupPrompt"
	// Task-compose popup (lazygit-style two-pane commit editor).
	winTaskComposeSummary     = "taskComposeSummary"
	winTaskComposeDescription = "taskComposeDescription"
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
}

// allDashboardWindows is every window name owned by the dashboard
// arrangement (excluding popups and the status bar). The layout
// garbage-collects entries not present in the current frame's
// boxlayout output.
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
type arrangeArgs struct {
	width, height int
	focusedSide   string // one of winTasks / winJobs / winWorkflows / winAgents
	state         ViewState
	logHidden     bool
	// showJobInput, when true, allocates winJobInput as a fixed-size
	// box under winDetail. Layout pass computes this from the
	// selected job's running status — see layout.go.
	showJobInput bool
}

// dashboardArrangement returns the Box tree for the standard
// dashboard view: four side panels stacked left, detail+log right,
// status bar across the bottom. The focused side panel grows.
//
// Frame=false views (the status bar) eat 2 cells of padding (see the
// pre-redesign inspector comment for the gocui-side rationale) so
// the status bar gets Size:3 to fit its single line of content.
//
// When showJobInput is true, a 6-row winJobInput box is inserted
// directly under winDetail (above winLog). The size is fixed at 6
// rows (1 frame top + 1 padding + ~2 content lines + 1 padding + 1
// frame bottom) — same magic number used for the previous
// inspector-input slot.
func dashboardArrangement(a arrangeArgs) *boxlayout.Box {
	logSize := 8
	if a.logHidden {
		logSize = 0
	}
	side := sideStack(a.focusedSide)

	rightChildren := []*boxlayout.Box{
		{Window: winDetail, Weight: 3},
	}
	if a.showJobInput {
		rightChildren = append(rightChildren, &boxlayout.Box{Window: winJobInput, Size: 6})
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
			{Window: winStatusBar, Size: 3},
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
