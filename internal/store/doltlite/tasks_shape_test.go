package doltlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// freshStore stands up an isolated migrated doltlite store for a single
// test. Kept local to this file so the assertion suite doesn't depend
// on the larger conformance harness.
func freshStore(t *testing.T) *doltlite.Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s := doltlite.New()
	if err := s.Open(ctx, filepath.Join(dir, "t.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// TestCreateTask_RejectsLegacyIDShape pins the v0.2-post-007 invariant:
// callers must NOT supply legacy `as-XXXX` ids to CreateTask. The
// generator path (`id.NewUnique`) produces `ask-XXXXXX` automatically,
// so the only callers that hit `assertTaskIDShape` are ones that
// pre-populate Task.ID — and they must use the new shape.
func TestCreateTask_RejectsLegacyIDShape(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	_, err := s.CreateTask(ctx, store.Task{
		ID: "as-a1b2", Title: "t", Status: store.StatusNew, Priority: 2,
	})
	if err == nil {
		t.Fatal("legacy as-XXXX id accepted; expected ErrInvalidShape")
	}
	if !errors.Is(err, store.ErrInvalidShape) {
		t.Errorf("err = %v, want errors.Is(., ErrInvalidShape)", err)
	}
}

// TestCreateTask_RejectsBadCanonicalShape: even within the new prefix,
// the shape must be exactly `ask-` + 6 lowercase hex. 4 hex chars under
// the `ask-` prefix is also rejected (would collide with the migrated
// namespace) and 8 hex chars is rejected (no caller mints those).
func TestCreateTask_RejectsBadCanonicalShape(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	for _, bad := range []string{
		"ask-a1b2",     // 4 hex chars — old width under new prefix
		"ask-a1b2c3d4", // 8 hex chars — too wide
		"ask-A1B2C3",   // uppercase
		"task-a1b2c3",  // wrong prefix
		"ask-",         // no suffix
		"",             // genuine-empty handled by the auto-mint path
		"ask-z1b2c3",   // non-hex character
	} {
		if bad == "" {
			// Empty ID is the auto-mint signal; skip from the reject suite.
			continue
		}
		_, err := s.CreateTask(ctx, store.Task{
			ID: bad, Title: "t", Status: store.StatusNew, Priority: 2,
		})
		if !errors.Is(err, store.ErrInvalidShape) {
			t.Errorf("CreateTask(%q): err=%v, want ErrInvalidShape", bad, err)
		}
	}
}

// TestCreateTask_AcceptsCanonicalShape: a caller-supplied id that
// matches `ask-` + 6 lowercase hex chars round-trips through Create.
func TestCreateTask_AcceptsCanonicalShape(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	tk, err := s.CreateTask(ctx, store.Task{
		ID: "ask-a1b2c3", Title: "t", Status: store.StatusNew, Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != "ask-a1b2c3" {
		t.Errorf("ID = %q, want ask-a1b2c3", tk.ID)
	}
}

// TestCreateTask_AutomintIDIsCanonical: the generator path mints
// `ask-` + 6 hex chars when the caller leaves Task.ID empty.
func TestCreateTask_AutomintIDIsCanonical(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	tk, err := s.CreateTask(ctx, store.Task{
		Title: "t", Status: store.StatusNew, Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tk.ID) != 10 {
		t.Errorf("auto-mint length = %d, want 10 (id=%q)", len(tk.ID), tk.ID)
	}
	if !strings.HasPrefix(tk.ID, "ask-") {
		t.Errorf("auto-mint prefix wrong: id=%q", tk.ID)
	}
}
