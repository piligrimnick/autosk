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
	sessions := dims[winSessions]
	tasksH := tasks.Y1 - tasks.Y0
	sessionsH := sessions.Y1 - sessions.Y0
	if tasksH <= sessionsH {
		t.Fatalf("focused tasks (%d) should be taller than sessions (%d)", tasksH, sessionsH)
	}
}

// TestDashboardArrangement_AllWindowsPresent ensures every named
// window gets a slot in the boxlayout tree. winSessionInput is NOT in
// the boxlayout — it is overlaid on top of winDetail's bottom rows
// by layout.go when the selected session is live.
func TestDashboardArrangement_AllWindowsPresent(t *testing.T) {
	dims := arrange(arrangeArgs{width: 120, height: 40, focusedSide: winTasks})
	for _, w := range []string{winTasks, winSessions, winWorkflows, winAgents, winDetail, winLog, winStatusBar} {
		if _, ok := dims[w]; !ok {
			t.Errorf("missing window %q", w)
		}
	}
	// winSessionInput must never appear in the boxlayout tree — it is
	// overlaid by layout.go on top of winDetail's bottom rows.
	if _, ok := dims[winSessionInput]; ok {
		t.Errorf("winSessionInput must not be returned by boxlayout (it is overlaid in layout.go)")
	}
}

// TestAllDashboardWindows_OverlayOrder pins the load-bearing
// invariant the layout pass depends on: winDetail must appear
// before winSessionInput in allDashboardWindows, because the layout
// visits views in this order when creating them and gocui draws
// views in creation order (last-created on top). If winSessionInput
// were created before winDetail, detail's frame would paint over
// the input overlay every frame.
//
// A future contributor reordering allDashboardWindows (e.g.
// alphabetising) would silently break the overlay z-order without
// any other test failing. This test exists specifically to
// catch that class of refactor.
func TestAllDashboardWindows_OverlayOrder(t *testing.T) {
	detailIdx := -1
	inputIdx := -1
	for i, n := range allDashboardWindows {
		if n == winDetail {
			detailIdx = i
		}
		if n == winSessionInput {
			inputIdx = i
		}
	}
	if detailIdx < 0 {
		t.Fatalf("winDetail missing from allDashboardWindows: %v", allDashboardWindows)
	}
	if inputIdx < 0 {
		t.Fatalf("winSessionInput missing from allDashboardWindows: %v", allDashboardWindows)
	}
	if detailIdx >= inputIdx {
		t.Errorf("winDetail (index %d) must appear before winSessionInput (index %d) in allDashboardWindows so the overlay draws on top: %v",
			detailIdx, inputIdx, allDashboardWindows)
	}
}
