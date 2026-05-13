package projectdb_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autosk/internal/projectdb"
)

func TestResolve_Override(t *testing.T) {
	got, err := projectdb.Resolve(t.TempDir(), "/explicit/path")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/path" {
		t.Fatalf("want override, got %q", got)
	}
}

func TestResolve_Env(t *testing.T) {
	t.Setenv(projectdb.EnvDB, "/from/env")
	got, err := projectdb.Resolve(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/from/env" {
		t.Fatalf("want env path, got %q", got)
	}
}

func TestResolve_WalkUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(root, ".autosk")
	if err := os.Mkdir(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "db")
	if err := os.WriteFile(dbPath, []byte("touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(projectdb.EnvDB, "")

	got, err := projectdb.Resolve(deep, "")
	if err != nil {
		t.Fatalf("walk-up failed: %v", err)
	}
	if got != dbPath {
		t.Fatalf("want %q, got %q", dbPath, got)
	}
}

func TestResolve_NotFound(t *testing.T) {
	t.Setenv(projectdb.EnvDB, "")
	_, err := projectdb.Resolve(t.TempDir(), "")
	if !errors.Is(err, projectdb.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestResolveOrInit_Creates(t *testing.T) {
	root := t.TempDir()
	t.Setenv(projectdb.EnvDB, "")
	t.Setenv(projectdb.EnvNoAutoInit, "")

	path, created, err := projectdb.ResolveOrInit(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("want created=true on empty dir")
	}
	wantDir := filepath.Join(root, ".autosk")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("want %s/db, got %s", wantDir, path)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestResolveOrInit_RespectsNoAutoInit(t *testing.T) {
	t.Setenv(projectdb.EnvDB, "")
	t.Setenv(projectdb.EnvNoAutoInit, "1")
	_, _, err := projectdb.ResolveOrInit(t.TempDir(), "")
	if !errors.Is(err, projectdb.ErrAutoInitDisabled) {
		t.Fatalf("want ErrAutoInitDisabled, got %v", err)
	}
}

func TestResolveOrInit_DiscoveryWins(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, ".autosk")
	if err := os.Mkdir(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "db")
	if err := os.WriteFile(dbPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(projectdb.EnvDB, "")
	t.Setenv(projectdb.EnvNoAutoInit, "1") // would fail if AutoInit triggered

	got, created, err := projectdb.ResolveOrInit(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("did not expect AutoInit")
	}
	if got != dbPath {
		t.Fatalf("want %q, got %q", dbPath, got)
	}
}
