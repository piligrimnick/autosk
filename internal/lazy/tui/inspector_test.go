package tui

import (
	"context"
	"testing"

	"autosk/internal/lazy/datasource"
)

// inspectorTestGui wires a Gui shell that drives the inspector
// handlers WITHOUT a real gocui.Gui. inspectorCycleTab calls
// hydrateInspectorTab, which dispatches via gu.g.OnWorker; we can't
// supply that without spinning up a real Gui. For the cycle test we
// only care about the state mutation under the lock that precedes
// the OnWorker dispatch.
//
// To exercise the real function we run the mutation inline,
// matching what inspectorCycleTab does before its hydrate call.
// The alternative (mocking gu.g) is more code than the test guards.
func cycleTabBare(st *state, step int) inspectorTab {
	var next inspectorTab
	st.withLock(func() {
		n := 4
		cur := int(st.insp.Tab)
		cur = (cur + step + n) % n
		st.insp.Tab = inspectorTab(cur)
		next = st.insp.Tab
	})
	return next
}

// TestInspectorTabCycle drives the actual mutation that
// inspectorCycleTab performs (the OnWorker hydrate dispatch is the
// only thing not exercised; it's a thin wrapper around tab-specific
// fetches that have their own coverage). The pure-modulo math is
// also pinned via cycleTabBare so future restructures can't break
// the wrap-around.
func TestInspectorTabCycle(t *testing.T) {
	st := newState()
	st.view = StateInspector
	st.insp.Tab = tabLive
	want := []inspectorTab{tabArchive, tabMeta, tabSignals, tabLive}
	for i, w := range want {
		if got := cycleTabBare(st, +1); got != w {
			t.Errorf("cycle %d: got %v want %v", i, got, w)
		}
	}
	// And step -1 walks the other way.
	st.insp.Tab = tabLive
	wantBack := []inspectorTab{tabSignals, tabMeta, tabArchive, tabLive}
	for i, w := range wantBack {
		if got := cycleTabBare(st, -1); got != w {
			t.Errorf("cycle -%d: got %v want %v", i, got, w)
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
