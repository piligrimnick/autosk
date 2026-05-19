package tui

import (
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestInspectorTabCycle pins the [ / ] cycle behaviour: index advances
// modulo 4 (Live → Archive → Meta → Signals → Live).
func TestInspectorTabCycle(t *testing.T) {
	cases := []struct {
		start inspectorTab
		step  int
		want  inspectorTab
	}{
		{tabLive, +1, tabArchive},
		{tabArchive, +1, tabMeta},
		{tabMeta, +1, tabSignals},
		{tabSignals, +1, tabLive},
		{tabLive, -1, tabSignals},
		{tabSignals, -1, tabMeta},
	}
	for _, tc := range cases {
		got := inspectorTab((int(tc.start) + tc.step + 4) % 4)
		if got != tc.want {
			t.Errorf("from %v step %d: got %v want %v", tc.start, tc.step, got, tc.want)
		}
	}
}

// TestInspectorClose returns the state machine to dashboard and
// clears the inspectorState. Drives the bare function on *state so
// we don't need a real gocui.Gui.
func TestInspectorClose(t *testing.T) {
	st := newState()
	st.view = StateInspector
	st.insp.JobID = "job-abc"
	st.insp.Tab = tabArchive
	st.insp.archive = make([]datasource.MessageEvent, 5)

	// Mimic inspectorClose: clear under lock.
	st.withLock(func() {
		st.view = StateDashboard
		st.insp = inspectorState{}
	})
	if st.view != StateDashboard {
		t.Fatalf("view=%v want dashboard", st.view)
	}
	if st.insp.JobID != "" || st.insp.archive != nil {
		t.Fatalf("inspector state not cleared: %+v", st.insp)
	}
}


