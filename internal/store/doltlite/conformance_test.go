package doltlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"autosk/internal/store"
	"autosk/internal/store/conformance"
	"autosk/internal/store/doltlite"
)

// factory returns a freshly opened, migrated doltlite store backed by a
// temp file. The conformance suite uses this for every backend.
func factory(t *testing.T) (store.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s := doltlite.New()
	ctx := context.Background()
	if err := s.Open(ctx, dbPath); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		t.Fatalf("Migrate: %v", err)
	}
	return s, func() { _ = s.Close() }
}

func TestConformance(t *testing.T) {
	conformance.RunConformance(t, factory)
}

func TestOpen_InMemory(t *testing.T) {
	s := doltlite.New()
	ctx := context.Background()
	if err := s.Open(ctx, ":memory:"); err != nil {
		t.Fatalf("Open :memory:: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("want schema_version >= 1, got %d", v)
	}
}

func TestSchemaVersion_BeforeOpen(t *testing.T) {
	s := doltlite.New()
	_, err := s.SchemaVersion(context.Background())
	conformance.AssertErrIs(t, err, store.ErrNotOpen)
}
