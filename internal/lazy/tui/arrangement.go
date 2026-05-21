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
	winLog          = "extras"
	winStatusBar    = "statusbar"
	winFlash        = "flash" // popup flash line (toast)
	winInspector    = "inspectorMain"
	winInspectorHdr = "inspectorHeader"
	winInspectorIn  = "inspectorInput"
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

// ViewState distinguishes the two top-level arrangements.
type ViewState int

const (
	StateDashboard ViewState = iota
	StateInspector
)

// arrangeArgs is the input to a window-arrangement function. Only the
// fields we actually consume; we don't need lazygit's whole bag.
type arrangeArgs struct {
	width, height int
	focusedSide   string // one of winTasks / winJobs / winWorkflows / winAgents
	state         ViewState
	logHidden     bool
}

// dashboardArrangement returns the Box tree for the standard
// dashboard view: four side panels stacked left, detail+log right,
// status bar across the bottom. The focused side panel grows.
//
// Frame=false views (the status bar) eat 2 cells of padding (see the
// inspectorArrangement comment) so the status bar gets Size:3 to fit
// its single line of content.
func dashboardArrangement(a arrangeArgs) *boxlayout.Box {
	logSize := 8
	if a.logHidden {
		logSize = 0
	}
	side := sideStack(a.focusedSide)
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
						Children: []*boxlayout.Box{
							{Window: winDetail, Weight: 3},
							{Window: winLog, Size: logSize},
						},
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

// inspectorArrangement is the fullscreen-job-inspector layout: one
// short header at the top (tab strip + run id), one large main view,
// one optional bottom input (Live tab), status bar.
//
// gocui's Frame=false views still cost 2 cells of padding (see the
// "weirdness" comment on view.InnerSize): a Size:2 view ends up with
// InnerHeight=0 and draws nothing. So we allocate 4 rows for the
// 2-line header (1-cell padding top + 2 content lines + 1-cell
// padding bottom) and 3 rows for the 1-line status bar.
func inspectorArrangement(a arrangeArgs, showInput bool) *boxlayout.Box {
	inputSize := 0
	if showInput {
		inputSize = 6
	}
	children := []*boxlayout.Box{
		{Window: winInspectorHdr, Size: 4},
		{Window: winInspector, Weight: 1},
	}
	if inputSize > 0 {
		children = append(children, &boxlayout.Box{Window: winInspectorIn, Size: inputSize})
	}
	children = append(children, &boxlayout.Box{Window: winStatusBar, Size: 3})
	return &boxlayout.Box{Direction: boxlayout.ROW, Children: children}
}

// arrange runs boxlayout on the chosen tree and returns the window
// dimensions map. Pure function; testable in isolation.
func arrange(a arrangeArgs, showLiveInput bool) map[string]boxlayout.Dimensions {
	var box *boxlayout.Box
	switch a.state {
	case StateInspector:
		box = inspectorArrangement(a, showLiveInput)
	default:
		box = dashboardArrangement(a)
	}
	return boxlayout.ArrangeWindows(box, 0, 0, a.width, a.height)
}
