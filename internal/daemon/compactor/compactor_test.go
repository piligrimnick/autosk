package compactor_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/compactor"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// helper: open a doltlite store + run a bit of churn so dolt_gc has
// something to reclaim. The 10-row burst is small enough to keep the
// test fast (<100ms in CI) but large enough for ChunksKept > 0.
func openChurnyStore(t *testing.T) *doltlite.Store {
	t.Helper()
	ctx := context.Background()
	s := doltlite.New()
	if err := s.Open(ctx, filepath.Join(t.TempDir(), "compactor.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	done := store.StatusDone
	for i := 0; i < 10; i++ {
		tk, err := s.CreateTask(ctx, store.Task{
			Title:  "churn",
			Status: store.StatusNew,
		})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if _, err := s.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: &done}); err != nil {
			t.Fatalf("UpdateTask: %v", err)
		}
	}
	return s
}

// TestRunOnce_ReclaimsAndIsSerialised: RunOnce exposes the underlying
// dolt_gc() return shape and serialises with itself via the busy
// lock.
func TestRunOnce_ReclaimsAndIsSerialised(t *testing.T) {
	s := openChurnyStore(t)
	c := compactor.New(s, compactor.Config{Interval: time.Hour, ProjectKey: "test"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.ChunksKept == 0 {
		t.Fatalf("RunOnce reclaimed everything? raw=%q", res.Raw)
	}

	// Concurrent RunOnce calls should both succeed (the busy mutex
	// serialises them; neither should error or hang).
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel2()
			_, err := c.RunOnce(ctx2)
			errCh <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("concurrent RunOnce: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("concurrent RunOnce timed out")
		}
	}
}

// TestScheduledTick_RunsCompaction: a short-interval Start/Stop cycle
// must run dolt_gc at least once. Asserts on the exposed Ticks()
// counter so the test is independent of whether doltlite actually
// had stale chunks to reclaim — what we care about here is that the
// goroutine wakes up and calls Compact.
func TestScheduledTick_RunsCompaction(t *testing.T) {
	s := openChurnyStore(t)
	// 25ms interval — short enough that one tick definitely fires in
	// the 250ms we wait below, slow enough that we still exercise
	// the ticker path rather than back-to-back ticks racing the
	// busy mutex.
	c := compactor.New(s, compactor.Config{
		Interval:   25 * time.Millisecond,
		ProjectKey: "test",
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait until Ticks() advances, bounded by a generous timeout so a
	// slow CI doesn't false-fail.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && c.Ticks() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Ticks() == 0 {
		t.Fatalf("scheduled tick never advanced Ticks() after 2s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStartStop_Idempotent: double Start returns ErrAlreadyStarted;
// Stop before Start returns ErrNotStarted; Stop after Stop returns
// ErrNotStarted. Matches the poller's lifecycle shape.
func TestStartStop_Idempotent(t *testing.T) {
	s := openChurnyStore(t)
	c := compactor.New(s, compactor.Config{Interval: time.Hour, ProjectKey: "test"})

	ctx := context.Background()
	if err := c.Stop(ctx); err != compactor.ErrNotStarted {
		t.Fatalf("Stop before Start: got %v, want ErrNotStarted", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := c.Start(ctx); err != compactor.ErrAlreadyStarted {
		t.Fatalf("double Start: got %v, want ErrAlreadyStarted", err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Stop(ctx); err != compactor.ErrNotStarted {
		t.Fatalf("double Stop: got %v, want ErrNotStarted", err)
	}
}

// TestDisabled_StartReturnsErr: cfg.Interval < 0 disables the
// compactor; Start returns ErrDisabled so the manager can log
// "GC disabled by --gc-interval=-1" once and move on.
func TestDisabled_StartReturnsErr(t *testing.T) {
	s := openChurnyStore(t)
	c := compactor.New(s, compactor.Config{Interval: -1, ProjectKey: "test"})
	if !c.Disabled() {
		t.Fatalf("Disabled() = false; want true")
	}
	if err := c.Start(context.Background()); err != compactor.ErrDisabled {
		t.Fatalf("Start(disabled): got %v, want ErrDisabled", err)
	}
	// RunOnce still works (operator can run `autosk gc` even when the
	// scheduled loop is off).
	res, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce(disabled): %v", err)
	}
	if res.Raw == "" {
		t.Fatalf("RunOnce(disabled): empty raw output")
	}
}
