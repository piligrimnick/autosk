package scheduler_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

const projKey = "proj-A"

type schedFixture struct {
	runs   *runstore.Store
	taskID string
	stepID string
	close  func()
}

// newFixture installs a doltlite store, a runstore, and a (task, workflow,
// step) tuple so daemon_runs FKs are satisfied.
func newFixture(t *testing.T) *schedFixture {
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
	tk, err := ts.CreateTask(ctx, store.Task{Title: "scheduled", Status: store.StatusNew, Priority: 2})
	if err != nil {
		_ = ts.Close()
		t.Fatalf("CreateTask: %v", err)
	}
	db := ts.DB()
	var humanID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agents WHERE name='human'`).Scan(&humanID); err != nil {
		_ = ts.Close()
		t.Fatalf("select human: %v", err)
	}
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES ('wf-sched', 'wf-sched', '', 'st-sched', 0, ?)`, now); err != nil {
		_ = ts.Close()
		t.Fatalf("insert workflow: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-sched', 'wf-sched', 'do', ?, 0)`, humanID); err != nil {
		_ = ts.Close()
		t.Fatalf("insert step: %v", err)
	}
	return &schedFixture{
		runs:   runstore.New(db),
		taskID: tk.ID,
		stepID: "st-sched",
		close:  func() { _ = ts.Close() },
	}
}

// makeQueuedRun inserts a queued run and returns its job_id.
func makeQueuedRun(t *testing.T, fx *schedFixture) string {
	t.Helper()
	r, err := fx.runs.CreateRun(context.Background(), runstore.NewRun{TaskID: fx.taskID, StepID: fx.stepID})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r.JobID
}

func TestScheduler_DispatchesJobsConcurrently(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	defer fx.close()

	const n = 5
	ids := make([]string, n)
	for i := range ids {
		ids[i] = makeQueuedRun(t, fx)
	}

	var (
		running   atomic.Int32
		maxConcur atomic.Int32
		ran       atomic.Int32
		wg        sync.WaitGroup
	)
	wg.Add(n)

	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		defer wg.Done()
		cur := running.Add(1)
		defer running.Add(-1)
		for {
			m := maxConcur.Load()
			if cur <= m || maxConcur.CompareAndSwap(m, cur) {
				break
			}
		}
		_, _ = fx.runs.MarkRunning(ctx, job.ID, 0)
		time.Sleep(80 * time.Millisecond)
		_, _ = fx.runs.MarkDone(ctx, job.ID, 0, nil)
		ran.Add(1)
		return nil
	})

	s := scheduler.New(exec, scheduler.Config{Workers: 3, QueueDepth: n})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := s.Enqueue(scheduler.Job{Project: projKey, ID: id}); err != nil {
			t.Fatalf("Enqueue %s: %v", id, err)
		}
	}
	wg.Wait()
	if got := ran.Load(); got != int32(n) {
		t.Fatalf("only %d of %d ran", got, n)
	}
	if got := maxConcur.Load(); got != 3 {
		t.Errorf("expected max concurrency 3, got %d", got)
	}

	gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
	defer gc()
	if err := s.Stop(gctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	rows, _ := fx.runs.ListRuns(ctx, runstore.RunFilter{Statuses: []runstore.RunStatus{runstore.StatusDone}})
	if len(rows) != n {
		t.Fatalf("got %d done, want %d", len(rows), n)
	}
}

func TestScheduler_CancelInterruptsExecutor(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	defer fx.close()
	id := makeQueuedRun(t, fx)

	started := make(chan struct{})
	var observedDone atomic.Bool
	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		_, _ = fx.runs.MarkRunning(ctx, job.ID, 0)
		close(started)
		<-ctx.Done()
		observedDone.Store(true)
		_, _ = fx.runs.MarkCancelled(context.Background(), job.ID, nil)
		return ctx.Err()
	})

	s := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), time.Second)
		defer gc()
		_ = s.Stop(gctx)
	}()

	job := scheduler.Job{Project: projKey, ID: id}
	if err := s.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	<-started
	if !s.IsActive(job) {
		t.Fatal("expected job to be active")
	}
	if err := s.Cancel(job); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for !observedDone.Load() {
		select {
		case <-deadline:
			t.Fatal("executor never observed cancellation")
		case <-time.After(10 * time.Millisecond):
		}
	}
	got, _ := fx.runs.GetRun(ctx, id)
	if got.Status != runstore.StatusCancel {
		t.Fatalf("expected cancelled, got %+v", got)
	}
}

func TestScheduler_EnqueueBeforeStartIsRejected(t *testing.T) {
	fx := newFixture(t)
	defer fx.close()
	id := makeQueuedRun(t, fx)
	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error { return nil })
	s := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := s.Enqueue(scheduler.Job{Project: projKey, ID: id}); err == nil {
		t.Fatal("expected ErrNotStarted")
	}
}

func TestScheduler_StopWithGraceTimeoutErrors(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	defer fx.close()
	id := makeQueuedRun(t, fx)

	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		time.Sleep(300 * time.Millisecond)
		_, _ = fx.runs.MarkDone(ctx, job.ID, 0, nil)
		return nil
	})
	s := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Enqueue(scheduler.Job{Project: projKey, ID: id})
	time.Sleep(50 * time.Millisecond)

	gctx, gc := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer gc()
	err := s.Stop(gctx)
	if err == nil {
		t.Fatal("expected grace-timeout error")
	}
	time.Sleep(400 * time.Millisecond)
}

func TestScheduler_JobsAreScopedByProject(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	defer fx.close()
	id := makeQueuedRun(t, fx)

	started := make(chan struct{})
	exec := scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	s := scheduler.New(exec, scheduler.Config{Workers: 1})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), time.Second)
		defer gc()
		_ = s.Stop(gctx)
	}()
	jobA := scheduler.Job{Project: "proj-A", ID: id}
	jobB := scheduler.Job{Project: "proj-B", ID: id} // same id, different project
	if err := s.Enqueue(jobA); err != nil {
		t.Fatal(err)
	}
	<-started
	// Cancel for proj-B (a different qualified job) must not affect proj-A.
	if err := s.Cancel(jobB); err == nil {
		t.Fatal("expected ErrJobNotActive for cross-project cancel")
	}
	if !s.IsActive(jobA) {
		t.Fatal("expected proj-A job to still be active")
	}
	counts := s.ActiveCountByProject()
	if counts["proj-A"] != 1 {
		t.Fatalf("expected 1 active job for proj-A, got %d", counts["proj-A"])
	}
	if counts["proj-B"] != 0 {
		t.Fatalf("expected 0 active jobs for proj-B, got %d", counts["proj-B"])
	}
	_ = s.Cancel(jobA)
}
