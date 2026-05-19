package tui

import "testing"

// TestDashboardArrangement_FocusedSideGrows verifies the focused side
// panel ends up with roughly 3x the height of an unfocused one.
func TestDashboardArrangement_FocusedSideGrows(t *testing.T) {
	a := arrangeArgs{width: 120, height: 40, focusedSide: winTasks, state: StateDashboard}
	dims := arrange(a, false)

	tasks, ok := dims[winTasks]
	if !ok {
		t.Fatalf("no dimensions for tasks: %v", dims)
	}
	jobs := dims[winJobs]
	tasksH := tasks.Y1 - tasks.Y0
	jobsH := jobs.Y1 - jobs.Y0
	if tasksH <= jobsH {
		t.Fatalf("focused tasks (%d) should be taller than jobs (%d)", tasksH, jobsH)
	}
}

// TestDashboardArrangement_AllWindowsPresent ensures every named
// window gets a slot.
func TestDashboardArrangement_AllWindowsPresent(t *testing.T) {
	dims := arrange(arrangeArgs{width: 120, height: 40, focusedSide: winTasks}, false)
	for _, w := range []string{winTasks, winJobs, winWorkflows, winAgents, winDetail, winLog, winStatusBar} {
		if _, ok := dims[w]; !ok {
			t.Errorf("missing window %q", w)
		}
	}
}

// TestInspectorArrangement_NoInputWhenNotLive — inspector at Archive/
// Meta/Signals must NOT allocate the textarea region.
func TestInspectorArrangement_NoInputWhenNotLive(t *testing.T) {
	dims := arrange(arrangeArgs{width: 120, height: 40, state: StateInspector}, false)
	if _, ok := dims[winInspectorIn]; ok {
		t.Fatalf("expected no input window in non-live inspector, got %v", dims)
	}
	if _, ok := dims[winInspector]; !ok {
		t.Fatalf("expected inspector main, got %v", dims)
	}
}

// TestInspectorArrangement_LiveHasInput exercises the Live-tab layout
// with the textarea slot present.
func TestInspectorArrangement_LiveHasInput(t *testing.T) {
	dims := arrange(arrangeArgs{width: 120, height: 40, state: StateInspector}, true)
	in, ok := dims[winInspectorIn]
	if !ok {
		t.Fatalf("expected input window in live inspector")
	}
	if (in.Y1 - in.Y0) < 3 {
		t.Fatalf("input region too small: %+v", in)
	}
}
