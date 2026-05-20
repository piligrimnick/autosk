package doltlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// TestCompact_FreshDBIsNoOp: dolt_gc() on a brand-new store should
// succeed and report zero removed chunks. The parse helper should
// pick up "0 chunks kept" / "0 chunks removed" without choking.
func TestCompact_FreshDBIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := doltlite.New()
	if err := s.Open(ctx, filepath.Join(t.TempDir(), "fresh.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	res, err := s.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.ChunksRemoved != 0 {
		t.Fatalf("fresh DB removed %d chunks, want 0 (raw=%q)", res.ChunksRemoved, res.Raw)
	}
	if res.Raw == "" {
		t.Fatalf("Compact returned empty raw output")
	}
	if res.Duration <= 0 {
		t.Fatalf("Compact reported non-positive duration: %v", res.Duration)
	}
}

// TestCompact_AfterWrites_ReclaimsChunks: write some rows, then GC.
// Doltlite's chunk-store appends on every write, so an N-row insert
// burst leaves stale chunks behind that GC should reclaim.
func TestCompact_AfterWrites_ReclaimsChunks(t *testing.T) {
	ctx := context.Background()
	s := doltlite.New()
	if err := s.Open(ctx, filepath.Join(t.TempDir(), "writes.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Generate enough churn that dolt_gc() has something to reclaim.
	// 50 tasks × 2 status updates each is roughly the working set the
	// daemon racks up in ~20 minutes of busy activity.
	done := store.StatusDone
	cancelled := store.StatusCancelled
	for i := 0; i < 50; i++ {
		tk, err := s.CreateTask(ctx, store.Task{
			Title:  "gc-churn",
			Status: store.StatusNew,
		})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if _, err := s.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: &done}); err != nil {
			t.Fatalf("UpdateTask done: %v", err)
		}
		if _, err := s.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: &cancelled}); err != nil {
			t.Fatalf("UpdateTask cancelled: %v", err)
		}
	}
	res, err := s.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.ChunksRemoved == 0 && res.ChunksKept == 0 {
		// If both are zero the parse failed silently — surface the
		// raw output so we can update the format string.
		t.Fatalf("Compact reclaimed nothing AND kept nothing (raw=%q): "+
			"the dolt_gc() output format probably changed", res.Raw)
	}
	// We don't assert a specific count — doltlite's chunk-store
	// compaction is an implementation detail — but the working set
	// must be non-empty (the migrations + the 50 tasks have to live
	// somewhere).
	if res.ChunksKept == 0 {
		t.Fatalf("ChunksKept=0 after 50 inserts (raw=%q)", res.Raw)
	}
}
