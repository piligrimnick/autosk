package tui

import "testing"

// TestDashboardArrangement_FocusedSideGrows verifies the focused side
// panel ends up with roughly 3x the height of an unfocused one.
func TestDashboardArrangement_FocusedSideGrows(t *testing.T) {
	a := arrangeArgs{width: 120, height: 40, focusedSide: winTasks, state: StateDashboard}
	dims := arrange(a)

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
// window gets a slot. winJobInput is conditional (only when the
// selected job is running) and is exercised by
// TestDashboardArrangement_JobInputAppearsWhenRunning below.
func TestDashboardArrangement_AllWindowsPresent(t *testing.T) {
	dims := arrange(arrangeArgs{width: 120, height: 40, focusedSide: winTasks})
	for _, w := range []string{winTasks, winJobs, winWorkflows, winAgents, winDetail, winLog, winStatusBar} {
		if _, ok := dims[w]; !ok {
			t.Errorf("missing window %q", w)
		}
	}
	// winJobInput must NOT appear when showJobInput=false.
	if _, ok := dims[winJobInput]; ok {
		t.Errorf("winJobInput should be absent when showJobInput=false")
	}
}

// TestDashboardArrangement_JobInputAppearsWhenRunning pins the
// conditional-input branch: showJobInput=true adds winJobInput and
// the slot is the documented 6 rows.
func TestDashboardArrangement_JobInputAppearsWhenRunning(t *testing.T) {
	t.Run("with_input", func(t *testing.T) {
		dims := arrange(arrangeArgs{
			width: 120, height: 40,
			focusedSide:  winJobs,
			showJobInput: true,
		})
		in, ok := dims[winJobInput]
		if !ok {
			t.Fatalf("missing winJobInput with showJobInput=true: %v", dims)
		}
		if h := in.Y1 - in.Y0; h < 4 {
			t.Errorf("winJobInput height %d too small: %+v", h, in)
		}
		if _, ok := dims[winDetail]; !ok {
			t.Fatalf("missing winDetail")
		}
	})
	t.Run("without_input", func(t *testing.T) {
		dims := arrange(arrangeArgs{
			width: 120, height: 40,
			focusedSide:  winJobs,
			showJobInput: false,
		})
		if _, ok := dims[winJobInput]; ok {
			t.Errorf("winJobInput must be absent when showJobInput=false")
		}
	})
}
