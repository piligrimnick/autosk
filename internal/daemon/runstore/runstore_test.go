package runstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// newTestStores returns a freshly opened doltlite Store and a runstore.Store
// sharing its connection, plus a cleanup func.
func newTestStores(t *testing.T) (taskStore *doltlite.Store, runs *runstore.Store, cleanup func()) {
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
	r := runstore.New(ts.DB())
	return ts, r, func() { _ = ts.Close() }
}

func makeTask(t *testing.T, ts *doltlite.Store, title string) store.Task {
	t.Helper()
	ctx := context.Background()
	tk, err := ts.CreateTask(ctx, store.Task{Title: title, Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return tk
}

func TestCreateRun_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	tk := makeTask(t, ts, "do the thing")

	in := runstore.NewRun{
		TaskID:         tk.ID,
		Prompt:         "go fix it",
		Model:          "sonnet:high",
		Thinking:       runstore.ThinkingHigh,
		Cwd:            "/tmp/proj",
		AutoClaim:      true,
		MaxCorrections: 5,
		PreBlockedBy:   []string{"as-aaaa", "as-bbbb"},
	}
	r, err := runs.CreateRun(ctx, in)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.JobID == "" {
		t.Fatal("expected auto-assigned job_id")
	}
	if got, want := r.Status, runstore.StatusQueued; got != want {
		t.Errorf("status: got %q want %q", got, want)
	}
	if r.CreatedAt.IsZero() {
		t.Error("created_at not stamped")
	}

	// Read it back.
	got, err := runs.GetRun(ctx, r.JobID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.TaskID != tk.ID || got.Prompt != in.Prompt ||
		got.Model != in.Model || got.Thinking != in.Thinking ||
		got.Cwd != in.Cwd || got.MaxCorrections != in.MaxCorrections ||
		got.AutoClaim != in.AutoClaim {
		t.Fatalf("round-trip mismatch:\n in:  %+v\n got: %+v", in, got)
	}
	if len(got.PreBlockedBy) != 2 || got.PreBlockedBy[0] != "as-aaaa" {
		t.Fatalf("pre_blocked_by mismatch: %v", got.PreBlockedBy)
	}
}

func TestCreateRun_AdHoc_NoTask(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()

	r, err := runs.CreateRun(ctx, runstore.NewRun{
		Prompt: "explore this repo",
		Cwd:    "/tmp/proj",
	})
	if err != nil {
		t.Fatalf("CreateRun ad-hoc: %v", err)
	}
	if r.TaskID != "" {
		t.Errorf("expected empty task_id for ad-hoc run, got %q", r.TaskID)
	}
}

func TestCreateRun_ValidatesInput(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	cases := []struct{ name string; in runstore.NewRun }{
		{"empty prompt", runstore.NewRun{Cwd: "/x"}},
		{"empty cwd", runstore.NewRun{Prompt: "hi"}},
		{"bad thinking", runstore.NewRun{Prompt: "hi", Cwd: "/x", Thinking: "bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runs.CreateRun(ctx, c.in)
			if !errors.Is(err, runstore.ErrInvalidNewRun) {
				t.Fatalf("want ErrInvalidNewRun, got %v", err)
			}
		})
	}
}

func TestGetRun_NotFound(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	_, err := runs.GetRun(ctx, "job-deadbe")
	if !errors.Is(err, runstore.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMarkRunningAndTerminal(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	r, err := runs.CreateRun(ctx, runstore.NewRun{Prompt: "p", Cwd: "/x"})
	if err != nil {
		t.Fatal(err)
	}

	// queued -> running
	r2, err := runs.MarkRunning(ctx, r.JobID, 12345)
	if err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if r2.Status != runstore.StatusRunning {
		t.Fatalf("status: %q", r2.Status)
	}
	if r2.PID == nil || *r2.PID != 12345 {
		t.Fatalf("pid: %+v", r2.PID)
	}
	if r2.StartedAt == nil {
		t.Fatal("started_at not stamped")
	}

	// running -> done (with closure)
	r3, err := runs.MarkDone(ctx, r.JobID, 0, runstore.ClosureDone)
	if err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if r3.Status != runstore.StatusDone {
		t.Fatalf("status: %q", r3.Status)
	}
	if r3.ExitCode == nil || *r3.ExitCode != 0 {
		t.Fatalf("exit: %+v", r3.ExitCode)
	}
	if r3.ClosureKind != runstore.ClosureDone {
		t.Fatalf("closure: %q", r3.ClosureKind)
	}
	if r3.PID != nil {
		t.Fatalf("pid not cleared: %+v", r3.PID)
	}
	if r3.FinishedAt == nil {
		t.Fatal("finished_at not stamped")
	}
}

func TestMarkFailedAndCancelled(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	a, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "p", Cwd: "/x"})
	b, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "q", Cwd: "/x"})
	_, _ = runs.MarkRunning(ctx, a.JobID, 1)
	_, _ = runs.MarkRunning(ctx, b.JobID, 2)
	ec := 137
	failed, err := runs.MarkFailed(ctx, a.JobID, &ec, "agent_did_not_close_task")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != runstore.StatusFailed || failed.Error != "agent_did_not_close_task" {
		t.Fatalf("failed row wrong: %+v", failed)
	}
	cancelled, err := runs.MarkCancelled(ctx, b.JobID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != runstore.StatusCancelled {
		t.Fatalf("cancelled row wrong: %+v", cancelled)
	}
	if cancelled.ExitCode != nil {
		t.Fatalf("exit_code: %+v", cancelled.ExitCode)
	}
}

func TestSetPISessionAndPreBlocked(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	r, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "p", Cwd: "/x"})

	if err := runs.SetPISession(ctx, r.JobID, "sess-123", "/abs/session.jsonl"); err != nil {
		t.Fatalf("SetPISession: %v", err)
	}
	if err := runs.SetPreBlockedBy(ctx, r.JobID, []string{"as-1111"}); err != nil {
		t.Fatalf("SetPreBlockedBy: %v", err)
	}
	got, _ := runs.GetRun(ctx, r.JobID)
	if got.PISessionID != "sess-123" || got.SessionPath != "/abs/session.jsonl" {
		t.Fatalf("session not persisted: %+v", got)
	}
	if len(got.PreBlockedBy) != 1 || got.PreBlockedBy[0] != "as-1111" {
		t.Fatalf("pre_blocked_by: %v", got.PreBlockedBy)
	}
}

func TestIncCorrections(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	r, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "p", Cwd: "/x", MaxCorrections: 3})
	for want := 1; want <= 3; want++ {
		got, err := runs.IncCorrections(ctx, r.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("want %d, got %d", want, got)
		}
	}
}

func TestListRuns_FilterByStatus(t *testing.T) {
	ctx := context.Background()
	_, runs, done := newTestStores(t)
	defer done()
	a, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "a", Cwd: "/x"})
	b, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "b", Cwd: "/x"})
	_, _ = runs.MarkRunning(ctx, b.JobID, 1)
	_, _ = runs.MarkDone(ctx, b.JobID, 0, runstore.ClosureDone)

	queued, err := runs.ListRuns(ctx, runstore.RunFilter{Statuses: []runstore.RunStatus{runstore.StatusQueued}})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].JobID != a.JobID {
		t.Fatalf("queued: %+v", queued)
	}
	done2, _ := runs.ListRuns(ctx, runstore.RunFilter{Statuses: []runstore.RunStatus{runstore.StatusDone}})
	if len(done2) != 1 || done2[0].JobID != b.JobID {
		t.Fatalf("done: %+v", done2)
	}
}

func TestSweepRunningOnStartup(t *testing.T) {
	ctx := context.Background()
	_, runs, doneFn := newTestStores(t)
	defer doneFn()
	a, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "a", Cwd: "/x"})
	b, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "b", Cwd: "/x"})
	c, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "c", Cwd: "/x"})

	_, _ = runs.MarkRunning(ctx, a.JobID, 1)
	_, _ = runs.MarkRunning(ctx, b.JobID, 2)
	_, _ = runs.MarkRunning(ctx, c.JobID, 3)
	_, _ = runs.MarkDone(ctx, c.JobID, 0, runstore.ClosureDone)

	n, err := runs.SweepRunningOnStartup(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("rewrote %d, want 2", n)
	}
	got, _ := runs.GetRun(ctx, a.JobID)
	if got.Status != runstore.StatusFailed || got.Error != "daemon_restart" {
		t.Fatalf("a not failed: %+v", got)
	}
	got, _ = runs.GetRun(ctx, c.JobID)
	if got.Status != runstore.StatusDone {
		t.Fatalf("c clobbered: %+v", got)
	}
}

func TestTaskIDOnDeleteSetsNull(t *testing.T) {
	// Verify the FK ON DELETE SET NULL is wired up.
	ctx := context.Background()
	ts, runs, done := newTestStores(t)
	defer done()
	tk := makeTask(t, ts, "to be deleted")
	r, err := runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, Prompt: "p", Cwd: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ExecRaw(ctx, `DELETE FROM tasks WHERE id = ?`, tk.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	got, err := runs.GetRun(ctx, r.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "" {
		t.Fatalf("expected task_id cleared, got %q", got.TaskID)
	}
}
