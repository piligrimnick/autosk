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
	"autosk/internal/store/doltlite"
)

func newRuns(t *testing.T) (*runstore.Store, func()) {
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
	return runstore.New(ts.DB()), func() { _ = ts.Close() }
}

// makeQueuedRun inserts a queued run and returns its job_id.
func makeQueuedRun(t *testing.T, runs *runstore.Store) string {
	t.Helper()
	r, err := runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: "/x"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r.JobID
}

func TestScheduler_StartSweepsRunning(t *testing.T) {
	ctx := context.Background()
	runs, cleanup := newRuns(t)
	defer cleanup()
	a, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "p", Cwd: "/x"})
	_, _ = runs.MarkRunning(ctx, a.JobID, 1)

	exec := scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
		return nil
	})
	s := scheduler.New(runs, exec, scheduler.Config{Workers: 1})
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), time.Second)
		defer gc()
		_ = s.Stop(gctx)
	}()

	got, _ := runs.GetRun(ctx, a.JobID)
	if got.Status != runstore.StatusFailed || got.Error != "daemon_restart" {
		t.Fatalf("expected sweep-to-failed, got %+v", got)
	}
}

func TestScheduler_DispatchesJobsConcurrently(t *testing.T) {
	ctx := context.Background()
	runs, cleanup := newRuns(t)
	defer cleanup()

	const n = 5
	ids := make([]string, n)
	for i := range ids {
		ids[i] = makeQueuedRun(t, runs)
	}

	var (
		running   atomic.Int32
		maxConcur atomic.Int32
		ran       atomic.Int32
		wg        sync.WaitGroup
	)
	wg.Add(n)

	exec := scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
		defer wg.Done()
		cur := running.Add(1)
		defer running.Add(-1)
		for {
			m := maxConcur.Load()
			if cur <= m || maxConcur.CompareAndSwap(m, cur) {
				break
			}
		}
		// Mark running/done ourselves to reflect what a real executor does.
		_, _ = runs.MarkRunning(ctx, jobID, 0)
		time.Sleep(80 * time.Millisecond)
		_, _ = runs.MarkDone(ctx, jobID, 0, "")
		ran.Add(1)
		return nil
	})

	s := scheduler.New(runs, exec, scheduler.Config{Workers: 3, QueueDepth: n})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := s.Enqueue(id); err != nil {
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
	// All jobs should be done.
	rows, _ := runs.ListRuns(ctx, runstore.RunFilter{Statuses: []runstore.RunStatus{runstore.StatusDone}})
	if len(rows) != n {
		t.Fatalf("got %d done, want %d", len(rows), n)
	}
}

func TestScheduler_CancelInterruptsExecutor(t *testing.T) {
	ctx := context.Background()
	runs, cleanup := newRuns(t)
	defer cleanup()
	id := makeQueuedRun(t, runs)

	started := make(chan struct{})
	var observedDone atomic.Bool
	exec := scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
		_, _ = runs.MarkRunning(ctx, jobID, 0)
		close(started)
		<-ctx.Done()
		observedDone.Store(true)
		// Persist terminal state on a fresh ctx — the cancelled ctx would
		// reject the SQL write. Real executors do the same.
		_, _ = runs.MarkCancelled(context.Background(), jobID, nil)
		return ctx.Err()
	})

	s := scheduler.New(runs, exec, scheduler.Config{Workers: 1})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		gctx, gc := context.WithTimeout(context.Background(), time.Second)
		defer gc()
		_ = s.Stop(gctx)
	}()

	if err := s.Enqueue(id); err != nil {
		t.Fatal(err)
	}
	<-started
	if !s.IsActive(id) {
		t.Fatal("expected job to be active")
	}
	if err := s.Cancel(id); err != nil {
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
	got, _ := runs.GetRun(ctx, id)
	if got.Status != runstore.StatusCancelled {
		t.Fatalf("expected cancelled, got %+v", got)
	}
}

func TestScheduler_EnqueueBeforeStartIsRejected(t *testing.T) {
	runs, cleanup := newRuns(t)
	defer cleanup()
	id := makeQueuedRun(t, runs)
	exec := scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error { return nil })
	s := scheduler.New(runs, exec, scheduler.Config{Workers: 1})
	if err := s.Enqueue(id); err == nil {
		t.Fatal("expected ErrNotStarted")
	}
}

func TestScheduler_StopWithGraceTimeoutErrors(t *testing.T) {
	ctx := context.Background()
	runs, cleanup := newRuns(t)
	defer cleanup()
	id := makeQueuedRun(t, runs)

	exec := scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
		// Ignore cancellation so Stop has to wait.
		time.Sleep(300 * time.Millisecond)
		_, _ = runs.MarkDone(ctx, jobID, 0, "")
		return nil
	})
	s := scheduler.New(runs, exec, scheduler.Config{Workers: 1})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Enqueue(id)
	// Wait long enough for the worker to pick the job up.
	time.Sleep(50 * time.Millisecond)

	gctx, gc := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer gc()
	err := s.Stop(gctx)
	if err == nil {
		t.Fatal("expected grace-timeout error")
	}
	// Allow background goroutine to finish so go test doesn't flag a leak.
	time.Sleep(400 * time.Millisecond)
}
