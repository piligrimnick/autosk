package tui

import (
	"context"
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestInspectorTabCycle drives the real nextTab helper that
// inspectorCycleTab calls (the OnWorker hydrate dispatch is the only
// thing not exercised; it's a thin wrapper around tab-specific
// fetches that have their own coverage). Pinning the wrap-around
// here means a future restructure of the cycle math can't silently
// break it.
func TestInspectorTabCycle(t *testing.T) {
	// +1 walks Live → Archive → Meta → Signals → Live.
	cases := []struct {
		from inspectorTab
		step int
		want inspectorTab
	}{
		{tabLive, +1, tabArchive},
		{tabArchive, +1, tabMeta},
		{tabMeta, +1, tabSignals},
		{tabSignals, +1, tabLive},
		// -1 walks the other way.
		{tabLive, -1, tabSignals},
		{tabSignals, -1, tabMeta},
		{tabMeta, -1, tabArchive},
		{tabArchive, -1, tabLive},
		// Steps larger than one full cycle still land correctly.
		{tabLive, +5, tabArchive},
		{tabLive, -5, tabSignals},
	}
	for _, tc := range cases {
		if got := nextTab(tc.from, tc.step); got != tc.want {
			t.Errorf("nextTab(%v, %+d)=%v want %v", tc.from, tc.step, got, tc.want)
		}
	}
}

// TestInspectorClose drives the real inspectorClose function. It
// requires no real Gui because stopLiveStream is a no-op when no
// SSE handle is registered, and the lock-protected mutation is what
// we're pinning.
func TestInspectorClose(t *testing.T) {
	gu := &Gui{st: newState()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gu.ctx = ctx
	gu.st.view = StateInspector
	gu.st.insp = inspectorState{
		JobID:   "job-abc",
		Tab:     tabArchive,
		archive: make([]datasource.MessageEvent, 5),
	}
	if err := gu.inspectorClose(nil, nil); err != nil {
		t.Fatalf("inspectorClose: %v", err)
	}
	if gu.st.view != StateDashboard {
		t.Fatalf("view=%v want dashboard", gu.st.view)
	}
	if gu.st.insp.JobID != "" || gu.st.insp.archive != nil {
		t.Fatalf("inspector state not cleared: %+v", gu.st.insp)
	}
}
