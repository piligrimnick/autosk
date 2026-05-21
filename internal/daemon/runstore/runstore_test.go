package runstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// fixture installs a workflow + a single step so runstore tests have real
// FK targets for daemon_runs.task_id / step_id. Returns task id and step id.
func fixture(t *testing.T, ts *doltlite.Store, title string) (taskID, stepID string) {
	t.Helper()
	ctx := context.Background()
	tk, err := ts.CreateTask(ctx, store.Task{Title: title, Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// Pick the seeded human agent for the step. Workflow + step are inserted
	// raw because the Go-level workflow store doesn't exist yet (W3).
	db := ts.DB()
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES ('wf-test', 'test-wf', '', 'st-test', 0, ?)`, now); err != nil {
		// Workflow rows may already exist across sibling tests; ignore UNIQUE
		// conflict — that's fine.
	}
	var humanID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agents WHERE name='human'`).Scan(&humanID); err != nil {
		t.Fatalf("select human agent: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq)
		 VALUES ('st-test', 'wf-test', 'do', ?, 0)`, humanID); err != nil {
		// Same: tolerate UNIQUE conflicts from re-insertion in the same suite.
	}
	return tk.ID, "st-test"
}

func newTestStores(t *testing.T) (*doltlite.Store, *runstore.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ts := doltlite.New()
	if err := ts.Open(ctx, dbPath); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	return ts, runstore.New(ts.DB()), func() { _ = ts.Close() }
}

func TestCreateRun_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	taskID, stepID := fixture(t, ts, "do the thing")

	r, err := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID, MaxCorrections: 5})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.JobID == "" {
		t.Fatal("expected auto-assigned job_id")
	}
	if r.Status != runstore.StatusQueued {
		t.Errorf("status: %q", r.Status)
	}
	if r.TaskID != taskID || r.StepID != stepID {
		t.Errorf("fk fields: task=%s step=%s", r.TaskID, r.StepID)
	}
	if r.MaxCorrections != 5 {
		t.Errorf("max_corrections: %d", r.MaxCorrections)
	}
	got, err := runs.GetRun(ctx, r.JobID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.JobID != r.JobID {
		t.Fatalf("round-trip: %s vs %s", got.JobID, r.JobID)
	}
}

func TestCreateRun_Rejects_MissingFKs(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	for _, c := range []struct {
		name string
		in   runstore.NewRun
	}{
		{"empty task", runstore.NewRun{StepID: "x"}},
		{"empty step", runstore.NewRun{TaskID: "x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := runs.CreateRun(ctx, c.in)
			if !errors.Is(err, runstore.ErrInvalidNewRun) {
				t.Fatalf("want ErrInvalidNewRun, got %v", err)
			}
		})
	}
}

func TestGetRun_NotFound(t *testing.T) {
	_, runs, done := newTestStores(t)
	defer done()
	_, err := runs.GetRun(context.Background(), "job-deadbe")
	if !errors.Is(err, runstore.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestLifecycle_Done(t *testing.T) {
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	taskID, stepID := fixture(t, ts, "lifecycle")

	r, err := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r2, err := runs.MarkRunning(ctx, r.JobID, 4242)
	if err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if r2.Status != runstore.StatusRunning || r2.PID == nil || *r2.PID != 4242 {
		t.Fatalf("running state: %+v", r2)
	}
	r3, err := runs.MarkDone(ctx, r.JobID, 0, nil)
	if err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if r3.Status != runstore.StatusDone || r3.ExitCode == nil || *r3.ExitCode != 0 || r3.PID != nil {
		t.Fatalf("done state: %+v", r3)
	}
}

func TestLifecycle_FailedCancelled(t *testing.T) {
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	taskID, stepID := fixture(t, ts, "lifecycle2")

	a, _ := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID})
	b, _ := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID})
	exit := 1
	failed, err := runs.MarkFailed(ctx, a.JobID, &exit, "agent_did_not_emit_transition")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != runstore.StatusFailed || failed.Error != "agent_did_not_emit_transition" {
		t.Fatalf("failed: %+v", failed)
	}
	cancelled, err := runs.MarkCancelled(ctx, b.JobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != runstore.StatusCancel {
		t.Fatalf("cancelled: %+v", cancelled)
	}
}

func TestIncCorrectionsAndSession(t *testing.T) {
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	taskID, stepID := fixture(t, ts, "corr")
	r, _ := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID, MaxCorrections: 3})

	n, err := runs.IncCorrections(ctx, r.JobID)
	if err != nil || n != 1 {
		t.Fatalf("inc: %d %v", n, err)
	}
	if err := runs.SetPISession(ctx, r.JobID, "sess-xyz", "/tmp/x.jsonl"); err != nil {
		t.Fatal(err)
	}
	got, _ := runs.GetRun(ctx, r.JobID)
	if got.CorrectionsUsed != 1 || got.PISessionID != "sess-xyz" || got.SessionPath != "/tmp/x.jsonl" {
		t.Fatalf("set session: %+v", got)
	}
}

func TestListAndSweep(t *testing.T) {
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	taskID, stepID := fixture(t, ts, "sweep")

	a, _ := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID})
	b, _ := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID})
	c, _ := runs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: stepID})
	_, _ = runs.MarkRunning(ctx, a.JobID, 1)
	_, _ = runs.MarkRunning(ctx, b.JobID, 2)
	_, _ = runs.MarkDone(ctx, c.JobID, 0, nil)

	n, err := runs.SweepRunningOnStartup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("sweep n=%d", n)
	}
	got, _ := runs.GetRun(ctx, a.JobID)
	if got.Status != runstore.StatusFailed || got.Error != "daemon_restart" {
		t.Fatalf("swept: %+v", got)
	}
	got, _ = runs.GetRun(ctx, c.JobID)
	if got.Status != runstore.StatusDone {
		t.Fatalf("done preserved: %+v", got)
	}

	all, _ := runs.ListRuns(ctx, runstore.RunFilter{Statuses: []runstore.RunStatus{runstore.StatusFailed}})
	if len(all) != 2 {
		t.Fatalf("list failed: %d", len(all))
	}
}
