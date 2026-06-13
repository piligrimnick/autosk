package projectdb_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autosk/internal/projectdb"
)

func TestResolveRoot_WalkUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".autosk"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := projectdb.ResolveRoot(deep)
	if err != nil {
		t.Fatalf("walk-up failed: %v", err)
	}
	if got != root {
		t.Fatalf("want %q, got %q", root, got)
	}
}

func TestResolveRoot_AtRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".autosk"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := projectdb.ResolveRoot(root)
	if err != nil {
		t.Fatalf("resolve at root failed: %v", err)
	}
	if got != root {
		t.Fatalf("want %q, got %q", root, got)
	}
}

func TestResolveRoot_NotFound(t *testing.T) {
	_, err := projectdb.ResolveRoot(t.TempDir())
	if !errors.Is(err, projectdb.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestResolveRoot_FileNotDir(t *testing.T) {
	// A regular file named .autosk must NOT be treated as a project marker.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".autosk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := projectdb.ResolveRoot(root); !errors.Is(err, projectdb.ErrNotFound) {
		t.Fatalf("want ErrNotFound for a non-dir marker, got %v", err)
	}
}
