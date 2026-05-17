package agent_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/store/doltlite"
)

func openTestDB(t *testing.T) (*agent.Store, func()) {
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

func TestSeededHumanIsListable(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	a, err := s.GetByName(context.Background(), "human")
	if err != nil {
		t.Fatalf("GetByName human: %v", err)
	}
	if !a.IsHuman {
		t.Fatalf("human row has is_human=false: %+v", a)
	}
	if !idLooksValid(a.ID) {
		t.Fatalf("human id malformed: %q", a.ID)
	}
}

func TestCreateAgent(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	a, err := s.Create(ctx, "developer", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Name != "developer" || a.IsHuman || a.ID == "" {
		t.Fatalf("bad shape: %+v", a)
	}

	// Duplicate.
	_, err = s.Create(ctx, "developer", false)
	if !errors.Is(err, agent.ErrAlreadyExist) {
		t.Fatalf("want ErrAlreadyExist, got %v", err)
	}

	// Empty name and whitespace.
	for _, bad := range []string{"", "  ", "two words"} {
		_, err := s.Create(ctx, bad, false)
		if !errors.Is(err, agent.ErrInvalidName) {
			t.Errorf("name %q: want ErrInvalidName, got %v", bad, err)
		}
	}
}

func TestEnsureByName(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	// "human" already exists (seeded).
	h, err := s.EnsureByName(ctx, "human")
	if err != nil {
		t.Fatal(err)
	}
	if !h.IsHuman {
		t.Fatalf("ensure existing human: is_human=%t", h.IsHuman)
	}

	// "foo" doesn't exist; created with is_human=false.
	f, err := s.EnsureByName(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "foo" || f.IsHuman {
		t.Fatalf("ensure new agent: %+v", f)
	}

	// Calling again is idempotent (no error).
	f2, err := s.EnsureByName(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if f2.ID != f.ID {
		t.Fatalf("ensure not idempotent: %s vs %s", f.ID, f2.ID)
	}
}

func TestList(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()

	// Seeded human + two more.
	_, _ = s.Create(ctx, "dev", false)
	_, _ = s.Create(ctx, "reviewer", false)
	got, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("list len: %d", len(got))
	}
	// Sorted by name ascending: dev, human, reviewer.
	want := []string{"dev", "human", "reviewer"}
	for i, a := range got {
		if a.Name != want[i] {
			t.Errorf("idx %d: want %s, got %s", i, want[i], a.Name)
		}
	}
}

func TestGetByID(t *testing.T) {
	s, done := openTestDB(t)
	defer done()
	ctx := context.Background()
	a, _ := s.Create(ctx, "dev", false)
	got, err := s.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "dev" {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetByID(ctx, "ag-ffff"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func idLooksValid(id string) bool {
	return len(id) >= 4 && id[:3] == "ag-"
}
