package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/store/doltlite"
)

func openTestAgentStore(t *testing.T) (*agent.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	return agent.New(ts.DB()), func() { _ = ts.Close() }
}

// TestResolveCallerAgent_LazyInsert verifies that resolveCallerAgent
// inserts a fresh agent for an unknown $AUTOSK_AGENT value. This is the
// hook W4 uses to fill tasks.author_id without forcing users to run
// `agent create` first.
func TestResolveCallerAgent_LazyInsert(t *testing.T) {
	ag, done := openTestAgentStore(t)
	defer done()

	// Unset → defaults to "human" (already seeded).
	t.Setenv(envAgentName, "")
	got, err := resolveCallerAgent(context.Background(), ag)
	if err != nil {
		t.Fatalf("resolveCallerAgent: %v", err)
	}
	if got.Name != "human" || !got.IsHuman {
		t.Fatalf("default human: %+v", got)
	}

	// New name → lazy insert.
	t.Setenv(envAgentName, "developer")
	got, err = resolveCallerAgent(context.Background(), ag)
	if err != nil {
		t.Fatalf("resolveCallerAgent developer: %v", err)
	}
	if got.Name != "developer" || got.IsHuman {
		t.Fatalf("lazy-inserted developer: %+v", got)
	}

	// Calling again is idempotent.
	got2, err := resolveCallerAgent(context.Background(), ag)
	if err != nil {
		t.Fatal(err)
	}
	if got2.ID != got.ID {
		t.Fatalf("not idempotent: %s vs %s", got.ID, got2.ID)
	}
}

// TestCallerAgentName_Default verifies the env-var default.
func TestCallerAgentName_Default(t *testing.T) {
	os.Unsetenv(envAgentName)
	if got := callerAgentName(); got != "human" {
		t.Fatalf("default: got %q want human", got)
	}
	t.Setenv(envAgentName, "  bot  ")
	if got := callerAgentName(); got != "bot" {
		t.Fatalf("trim: got %q want bot", got)
	}
}
