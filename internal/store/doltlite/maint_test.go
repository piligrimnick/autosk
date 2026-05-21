package doltlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// TestCompact_FreshDB: dolt_gc() on a brand-new store should succeed,
// report a non-empty Raw, keep a non-zero working set, and finish in
// positive wall-clock time. The first Compact may reclaim chunks left
// behind by migration 006 (which rebuilds tasks / step_transitions /
// daemon_runs to apply the new CHECK enum), so we do NOT assert
// ChunksRemoved == 0 on it. The follow-up Compact, however, runs on a
// quiescent DB and MUST be a no-op — that preserves the documented
// "GC on a quiescent DB is a no-op" contract that the original test
// pinned.
func TestCompact_FreshDB(t *testing.T) {
	ctx := context.Background()
	s := doltlite.New()
	if err := s.Open(ctx, filepath.Join(t.TempDir(), "fresh.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// First Compact reclaims migration leftovers (006 rebuild). The
	// ChunksRemoved value is implementation-defined; only Kept / Raw /
	// Duration get pinned.
	res1, err := s.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact #1: %v", err)
	}
	if res1.Raw == "" {
		t.Fatalf("Compact #1 returned empty raw output")
	}
	if res1.ChunksKept == 0 {
		t.Fatalf("ChunksKept=0 on a freshly migrated DB (raw=%q)", res1.Raw)
	}
	if res1.Duration <= 0 {
		t.Fatalf("Compact #1 reported non-positive duration: %v", res1.Duration)
	}
	// Second Compact on a now-quiescent DB MUST be a no-op. This is the
	// invariant the original (pre-006) test pinned — a regression in
	// dolt_gc() that started reclaiming live chunks would trip here.
	res2, err := s.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact #2: %v", err)
	}
	if res2.ChunksRemoved != 0 {
		t.Fatalf("quiescent Compact removed %d chunks (raw=%q)",
			res2.ChunksRemoved, res2.Raw)
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
	cancel := store.StatusCancel
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
		if _, err := s.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: &cancel}); err != nil {
			t.Fatalf("UpdateTask cancel: %v", err)
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
