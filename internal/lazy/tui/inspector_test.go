package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
)

// TestRenderInspectorBody_ArchiveStates pins the Archive tab's body
// rendering across the three states the hydrate path can leave the
// inspector in:
//
//	 archiveLoaded=false                  → "(loading…)"
//	 archiveLoaded=true, archiveErr!=nil  → "(archive load failed: …)"
//	 archiveLoaded=true, archiveErr==nil  → transcript / "(no transcript events)"
//
// Before the fix the hydrate path on error called only flashf and
// returned without flipping archiveLoaded, leaving the Archive tab
// stuck in "(loading…)" forever — the user's reported symptom.
// Regressing back to that state would now also need to break this test.
func TestRenderInspectorBody_ArchiveStates(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.view = StateInspector

	// loading
	gu.st.insp = inspectorState{JobID: "job-1", Tab: tabArchive}
	if body := gu.renderInspectorBody(); !strings.Contains(body, "loading") {
		t.Fatalf("loading: body=%q want substring 'loading'", body)
	}

	// error — the critical regression: loaded=true + err must NOT show "loading"
	gu.st.insp = inspectorState{
		JobID:         "job-1",
		Tab:           tabArchive,
		archiveLoaded: true,
		archiveErr:    errors.New("daemon: 503"),
	}
	body := gu.renderInspectorBody()
	if strings.Contains(body, "loading") {
		t.Fatalf("error: body still says 'loading': %q", body)
	}
	if !strings.Contains(body, "archive load failed") || !strings.Contains(body, "503") {
		t.Fatalf("error: body=%q want substring 'archive load failed' + '503'", body)
	}

	// empty success
	gu.st.insp = inspectorState{JobID: "job-1", Tab: tabArchive, archiveLoaded: true}
	if body := gu.renderInspectorBody(); !strings.Contains(body, "no transcript events") {
		t.Fatalf("empty: body=%q want substring 'no transcript events'", body)
	}
}

// TestHydrateContext_Budget pins the 5×Refresh / 5s floor budget so a
// regression that makes hydration deadline-free (the previous
// behaviour that let the Archive tab hang on a slow daemon) is caught
// at the unit level.
func TestHydrateContext_Budget(t *testing.T) {
	cases := []struct {
		name    string
		refresh time.Duration
		minWant time.Duration
	}{
		{"zero_refresh", 0, 5 * time.Second},
		{"tiny_refresh", 100 * time.Millisecond, 5 * time.Second},
		{"normal_refresh", 2 * time.Second, 10 * time.Second},
		{"large_refresh", 30 * time.Second, 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			ctx, cancel := hydrateContext(parent, tc.refresh)
			defer cancel()
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("%s: no deadline", tc.name)
			}
			if d := time.Until(dl); d < tc.minWant-200*time.Millisecond {
				t.Fatalf("%s: deadline in %v, want >= %v", tc.name, d, tc.minWant)
			}
		})
	}
}

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

// TestFilterCommentsSinceJob pins the design plan §5.5 contract:
// the Inspector Signals tab's comments sub-region must be scoped to
// "comments observed during this run" — i.e. only those whose
// CreatedAt is at or after the job's StartedAt. Prior to this fix
// the tab pulled every comment ever attached to the task, which for
// a kickback loop with 30 comments across 5 runs surfaced all 30
// when the operator opened run-5 (instead of only the comments
// added during run-5).
func TestFilterCommentsSinceJob(t *testing.T) {
	t0 := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	started := t0.Add(30 * time.Minute)
	comments := []datasource.Comment{
		{ID: 1, Text: "before run", CreatedAt: t0},
		{ID: 2, Text: "start boundary", CreatedAt: started},
		{ID: 3, Text: "during run", CreatedAt: started.Add(5 * time.Minute)},
	}
	t.Run("filters_before_started_at", func(t *testing.T) {
		j := datasource.Job{JobResponse: api.JobResponse{StartedAt: &started}}
		got := filterCommentsSinceJob(comments, j)
		if len(got) != 2 {
			t.Fatalf("got %d comments want 2 (boundary inclusive + during)", len(got))
		}
		if got[0].ID != 2 || got[1].ID != 3 {
			t.Fatalf("unexpected ids: %+v", got)
		}
	})
	t.Run("started_at_nil_keeps_all", func(t *testing.T) {
		j := datasource.Job{}
		got := filterCommentsSinceJob(comments, j)
		if len(got) != len(comments) {
			t.Fatalf("got %d want %d (no cutoff)", len(got), len(comments))
		}
	})
	t.Run("started_at_zero_keeps_all", func(t *testing.T) {
		var zero time.Time
		j := datasource.Job{JobResponse: api.JobResponse{StartedAt: &zero}}
		got := filterCommentsSinceJob(comments, j)
		if len(got) != len(comments) {
			t.Fatalf("got %d want %d (zero cutoff treated as no cutoff)", len(got), len(comments))
		}
	})
	t.Run("started_after_all_returns_empty", func(t *testing.T) {
		late := started.Add(time.Hour)
		j := datasource.Job{JobResponse: api.JobResponse{StartedAt: &late}}
		got := filterCommentsSinceJob(comments, j)
		if len(got) != 0 {
			t.Fatalf("got %d comments want 0 (all before cutoff)", len(got))
		}
	})
}
