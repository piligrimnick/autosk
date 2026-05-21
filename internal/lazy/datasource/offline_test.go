package datasource_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

func newOfflineFx(t *testing.T) (*datasource.Offline, *doltlite.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	ds, err := datasource.NewOffline(ts, dir, nil)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("offline: %v", err)
	}
	return ds, ts, func() { _ = ts.Close() }
}

func TestOffline_CreateAndListTasks(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()

	id, err := ds.CreateTask(ctx, "Refactor token validator", "", 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tasks, err := ds.Tasks(ctx, datasource.DefaultTaskFilter())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("got %d tasks, want 1; ids=%v", len(tasks), tasks)
	}
	if tasks[0].Title != "Refactor token validator" {
		t.Fatalf("title=%q", tasks[0].Title)
	}
	if tasks[0].Status != store.StatusNew {
		t.Fatalf("status=%s", tasks[0].Status)
	}
}

func TestOffline_FilterByPriority(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, _ = ds.CreateTask(ctx, "lo", "", 3)
	_, _ = ds.CreateTask(ctx, "hi", "", 0)
	p := 0
	tasks, err := ds.Tasks(ctx, datasource.TaskFilter{Statuses: store.OpenStatuses(), Priority: &p})
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "hi" {
		t.Fatalf("p0 returned %v", tasks)
	}
}

func TestOffline_FilterBySearch(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, _ = ds.CreateTask(ctx, "auth thing", "", 2)
	_, _ = ds.CreateTask(ctx, "logging thing", "", 2)
	tasks, _ := ds.Tasks(ctx, datasource.TaskFilter{Statuses: store.OpenStatuses(), Search: "AUTH"})
	if len(tasks) != 1 || tasks[0].Title != "auth thing" {
		t.Fatalf("search=AUTH: %v", tasks)
	}
}

func TestOffline_BlockAndUnblock(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	a, _ := ds.CreateTask(ctx, "parent", "", 2)
	b, _ := ds.CreateTask(ctx, "child", "", 2)
	if err := ds.Block(ctx, a, b); err != nil {
		t.Fatalf("block: %v", err)
	}
	got, err := ds.GetTask(ctx, a)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Blocked || len(got.BlockedBy) != 1 || got.BlockedBy[0] != b {
		t.Fatalf("expected blocked by %s, got blocked=%v blockedBy=%v", b, got.Blocked, got.BlockedBy)
	}
	if err := ds.Unblock(ctx, a, b); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	got, _ = ds.GetTask(ctx, a)
	if got.Blocked {
		t.Fatalf("still blocked after unblock")
	}
}

func TestOffline_UpdateStatusAndPriority(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := ds.CreateTask(ctx, "x", "", 2)
	if err := ds.UpdateStatus(ctx, id, store.StatusDone); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := ds.UpdatePriority(ctx, id, 0); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	got, _ := ds.GetTask(ctx, id)
	if got.Status != store.StatusDone || got.Priority != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestOffline_CommentRoundTrip(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := ds.CreateTask(ctx, "x", "", 2)
	if err := ds.AddComment(ctx, id, "hello"); err != nil {
		t.Fatalf("add: %v", err)
	}
	cs, err := ds.Comments(ctx, id)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(cs) != 1 || cs[0].Text != "hello" {
		t.Fatalf("comments: %v", cs)
	}
	tk, _ := ds.GetTask(ctx, id)
	if tk.CommentCount != 1 {
		t.Fatalf("comment count = %d", tk.CommentCount)
	}
}

func TestOffline_HealthIsDown(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	h, _ := ds.Healthz(ctx)
	if h.Daemon != "down" {
		t.Fatalf("offline must report down, got %q", h.Daemon)
	}
}

func TestOffline_StreamLiveReturnsErrDaemonRequired(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, err := ds.StreamLive(ctx, "job-xxxx")
	if err != datasource.ErrDaemonRequired {
		t.Fatalf("want ErrDaemonRequired, got %v", err)
	}
}

// seedTwoStepWF installs developer + code-reviewer agents and a tiny
// workflow with two steps named "dev" and "review". Used by the
// Enroll / Resume tests below to exercise workflow.EnterStep without
// pulling in the full executor fixture.
func seedTwoStepWF(t *testing.T, ts *doltlite.Store, maxVisitsDev, maxVisitsReview int) (workflow.Workflow, string, string) {
	t.Helper()
	ctx := context.Background()
	ag := agent.New(ts.DB())
	for _, name := range []string{"developer", "code-reviewer"} {
		if _, err := ag.EnsureByName(ctx, name); err != nil {
			t.Fatalf("ensure agent %s: %v", name, err)
		}
	}
	ws := workflow.New(ts.DB(), ag)
	body := `{
		"name": "twostep",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},     "max_visits": ` +
		strconv.Itoa(maxVisitsDev) + `, "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": ` +
		strconv.Itoa(maxVisitsReview) + `, "next_steps": [{"step": "dev", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse wf: %v", err)
	}
	wf, err := ws.Create(ctx, def, false)
	if err != nil {
		t.Fatalf("create wf: %v", err)
	}
	var devID, revID string
	for _, s := range wf.Steps {
		switch s.Name {
		case "dev":
			devID = s.ID
		case "review":
			revID = s.ID
		}
	}
	return wf, devID, revID
}

// TestOfflineEnroll_BumpsFirstStepCounter is the regression test for
// (B): lazy.Enroll must route through workflow.EnterStep so the
// step_visits counter on the entry step is bumped to 1 — same shape
// as `autosk enroll` on the CLI. Before the fix the lazy code path
// did a raw UPDATE that left step_visits empty, so an as-290f-style
// loop ended up off-by-one on the source counter.
func TestOfflineEnroll_BumpsFirstStepCounter(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	tk, err := ts.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tk.WorkflowID != wf.ID {
		t.Fatalf("workflow_id: %q (want %q)", tk.WorkflowID, wf.ID)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q)", tk.CurrentStepID, devID)
	}
	if tk.Status != store.StatusInWorkflow {
		t.Fatalf("status: %s (want in_workflow)", tk.Status)
	}
	sv, _ := tk.Metadata["step_visits"].(map[string]any)
	if sv == nil {
		t.Fatalf("step_visits missing; metadata=%+v", tk.Metadata)
	}
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[dev]=%v (want 1)", sv[devID])
	}
}

// TestOfflineEnrollAgent_BumpsSyntheticStep covers the single:<agent>
// shorthand. The synthetic workflow has only one step named "do";
// after enroll the counter on that step must be 1. max_visits=0 on
// synthetics means uncapped — the bump must still happen.
func TestOfflineEnrollAgent_BumpsSyntheticStep(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()

	id, err := ds.CreateTask(ctx, "y", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.EnrollAgent(ctx, id, "developer"); err != nil {
		t.Fatalf("enroll-agent: %v", err)
	}
	tk, err := ts.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tk.Status != store.StatusInWorkflow {
		t.Fatalf("status: %s (want in_workflow)", tk.Status)
	}
	if tk.WorkflowID == "" {
		t.Fatal("workflow_id not stamped on the task")
	}
	if tk.CurrentStepID == "" {
		t.Fatal("current_step_id not stamped on the task")
	}
	sv, _ := tk.Metadata["step_visits"].(map[string]any)
	if sv == nil {
		t.Fatalf("step_visits missing; metadata=%+v", tk.Metadata)
	}
	if v, _ := sv[tk.CurrentStepID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[synthetic step]=%v (want 1)", sv[tk.CurrentStepID])
	}
}

// TestOfflineResume_WithTo_BumpsTargetStep covers the deliberate-
// transition branch of Resume. Park a task on "dev", Resume(--to
// review), assert the target counter bumped exactly once and
// current_step_id moved to review.
func TestOfflineResume_WithTo_BumpsTargetStep(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, revID := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "z", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Park on dev so Resume is callable. We bypass the executor by
	// flipping status directly — the only Resume contract is that the
	// task is in human_feedback.
	parked := store.StatusHumanFeedback
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := ds.Resume(ctx, id, "review"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.CurrentStepID != revID {
		t.Fatalf("current_step_id: %q (want review=%q)", tk.CurrentStepID, revID)
	}
	if tk.Status != store.StatusInWorkflow {
		t.Fatalf("status: %s (want in_workflow)", tk.Status)
	}
	sv, _ := tk.Metadata["step_visits"].(map[string]any)
	if v, _ := sv[revID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[review]=%v (want 1)", sv[revID])
	}
	// dev counter remains at 1 from the original Enroll (no resume bump).
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[dev]=%v (want 1; resume --to STEP must not touch the source)", sv[devID])
	}
}

// TestOfflineResume_NoTo_NoCounterBump covers the no-transition
// branch of Resume. Park a task on "dev", Resume(""), assert the
// step pointer and counters are unchanged — only the status flips
// back to in_workflow. Pins the documented "no transition" rule
// (docs/plans/20260520-Step-Visit-Limits.md) for the lazy surface.
func TestOfflineResume_NoTo_NoCounterBump(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "r", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	pre, _ := ts.GetTask(ctx, id)
	// Park (status flip only — leaves current_step_id / metadata alone).
	parked := store.StatusHumanFeedback
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := ds.Resume(ctx, id, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusInWorkflow {
		t.Fatalf("status: %s (want in_workflow)", tk.Status)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q; no-transition resume must not move the pointer)", tk.CurrentStepID, devID)
	}
	// Counters must be byte-identical to what Enroll left behind.
	preSV, _ := pre.Metadata["step_visits"].(map[string]any)
	postSV, _ := tk.Metadata["step_visits"].(map[string]any)
	if v, _ := postSV[devID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[dev]=%v (want 1; no-transition resume must not bump)", postSV[devID])
	}
	if len(preSV) != len(postSV) {
		t.Fatalf("step_visits len pre=%d post=%d (no-transition resume must not add keys)", len(preSV), len(postSV))
	}
}

// TestOfflineResume_NoTo_RefusesWhenNoCurrentStep mirrors the CLI's
// `task has no current_step_id; pass --to STEP` guard in
// cmd/autosk/resume.go. Without the guard the lazy path would trip
// the schema CHECK in 001_init.sql:55 (in_workflow requires a
// non-NULL current_step_id) and surface a cryptic doltlite error in
// the flash bar instead of an actionable hint.
func TestOfflineResume_NoTo_RefusesWhenNoCurrentStep(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, _, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "r2", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Hand-edit to mimic the pathological state: parked in
	// human_feedback with current_step_id NULL. The schema allows
	// human_feedback with a NULL step (it's only in_workflow that
	// requires it), so the corrupted intermediate state is reachable.
	parked := store.StatusHumanFeedback
	empty := ""
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked, CurrentStepID: &empty}); err != nil {
		t.Fatalf("park: %v", err)
	}
	err = ds.Resume(ctx, id, "")
	if err == nil {
		t.Fatal("expected error; lazy resume without --to must refuse when current_step_id is empty")
	}
	if !strings.Contains(err.Error(), "pass --to STEP") {
		t.Fatalf("err %q should hint at `pass --to STEP`", err.Error())
	}
	// Status must NOT have flipped — the operator has to choose a
	// target step before the task moves anywhere.
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusHumanFeedback {
		t.Fatalf("status flipped on refused resume: %s (want human_feedback)", tk.Status)
	}
}

// TestOfflineEnroll_CapHitOnFirstEntry verifies the cap-exceeded path
// on Enroll: with step_visits[dev]=1 already present (e.g. left over
// after a partial reset) and dev.max_visits=1, Enroll must surface a
// typed MaxVisitsExceededError-derived message and NOT mutate the
// task's workflow_id / current_step_id.
func TestOfflineEnroll_CapHitOnFirstEntry(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 1, 5) // dev capped at 1.

	id, err := ds.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Prime the counter to 1 so the next bump would cross the cap.
	md := map[string]any{"step_visits": map[string]any{devID: 1}}
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Metadata: &md}); err != nil {
		t.Fatalf("prime metadata: %v", err)
	}
	err = ds.Enroll(ctx, id, wf.Name)
	if err == nil {
		t.Fatal("expected cap-exceeded error")
	}
	// mapEnterStepError wraps the typed error but keeps the chain intact.
	var mve workflow.MaxVisitsExceededError
	if !errors.As(err, &mve) {
		t.Fatalf("err %T: %v (want MaxVisitsExceededError in chain)", err, err)
	}
	if mve.StepID != devID {
		t.Fatalf("mve.StepID: %q (want %q)", mve.StepID, devID)
	}
	if !strings.Contains(err.Error(), "reset-visits") {
		t.Fatalf("err %q should hint at reset-visits", err.Error())
	}
	// Task pointer must not have moved.
	tk, _ := ts.GetTask(ctx, id)
	if tk.WorkflowID != "" {
		t.Fatalf("workflow_id leaked on cap-fire: %q", tk.WorkflowID)
	}
	if tk.CurrentStepID != "" {
		t.Fatalf("current_step_id leaked on cap-fire: %q", tk.CurrentStepID)
	}
	if tk.Status != store.StatusNew {
		t.Fatalf("status flipped on cap-fire: %s (want new)", tk.Status)
	}
}
