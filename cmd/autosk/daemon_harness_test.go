package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/rpcclient"
)

// --- CLI verb test harness: per-test isolated autoskd (proto-v2) ------------
//
// The CLI verbs route through autoskd (the Bun daemon owns .autosk/). The
// in-process cobra command is a thin RPC client, so a verb test needs a live
// daemon. ensureTestDaemon gives each test its own isolated daemon:
//
//   - AUTOSK_SOCK   — a short private UDS path (sun_path < ~104 bytes).
//   - HOME          — an isolated home so ~/.autosk/projects.json + the daemon
//     token never collide with the operator's real ~/.autosk. We also seed its
//     ~/.autosk/extensions/ with the fixture workflow file (testdata/extensions/
//     test-flow.js — a deterministic human-only "human-flow" plus an in-process
//     "auto-flow", discovered via the global extensions source) AND an empty
//     ~/.autosk/settings.json. settings.json's presence is the daemon's "already
//     initialised" marker, so its first-run bootstrap is SKIPPED — a test never
//     triggers a real `npm install`.
//   - AUTOSK_IDLE_SECS=0 — disable idle-shutdown so the daemon stays up for the
//     whole test even across the brief gaps between verb calls.
//
// The daemon is auto-spawned lazily by the rpcclient connector on the first
// verb call (inheriting the env above) and asked to stop on test cleanup.
//
// Resolution of the autoskd binary is left to the connector: AUTOSKD_BIN (set
// by the Makefile test targets, which compile autoskd first) → a sibling binary
// → PATH. When none is available we t.Skip.

// fixtureExtDir is the absolute path to the test fixture extensions directory.
// `go test` runs with cwd = the package dir (cmd/autosk), so this resolves to
// <repo>/cmd/autosk/testdata/extensions.
var fixtureExtDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		return "testdata/extensions"
	}
	return filepath.Join(wd, "testdata", "extensions")
}()

var harnessDaemons sync.Map // *testing.T -> struct{}: set-up guard, one daemon per test

// ensureTestDaemon configures the current test to talk to its own isolated
// autoskd. Idempotent.
func ensureTestDaemon(t *testing.T) {
	t.Helper()
	if _, done := harnessDaemons.Load(t); done {
		return
	}
	if os.Getenv("AUTOSKD_BIN") == "" {
		if _, err := os.Stat(siblingAutoskd()); err != nil {
			t.Skip("autoskd binary not found: set AUTOSKD_BIN or run via `make test` / `make test-short` (which compile autoskd first)")
		}
	}

	sock := shortSocketPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AUTOSK_SOCK", sock)
	t.Setenv("AUTOSK_IDLE_SECS", "0")
	seedFixtureExtensions(t, home)

	harnessDaemons.Store(t, struct{}{})
	t.Cleanup(func() {
		if cl, err := rpcclient.New(rpcclient.Options{Sock: sock, NoAutoSpawn: true}); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = cl.Shutdown(ctx)
			cancel()
		}
		harnessDaemons.Delete(t)
	})
}

// seedFixtureExtensions provisions the test home's global ~/.autosk so the
// daemon discovers the fixture workflows AND skips its first-run bootstrap:
//   - ~/.autosk/extensions/test-flow.js — copied from testdata/extensions, a
//     self-contained plain-JS extension (no @autosk/* imports), discovered via
//     the global extensions source.
//   - ~/.autosk/settings.json — an empty extension list whose mere presence
//     marks the environment "already initialised", so no real `npm install` runs.
func seedFixtureExtensions(t *testing.T, home string) {
	t.Helper()
	autoskDir := filepath.Join(home, ".autosk")
	extDir := filepath.Join(autoskDir, "extensions")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extensions dir: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(fixtureExtDir, "test-flow.js"))
	if err != nil {
		t.Fatalf("read fixture extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "test-flow.js"), src, 0o644); err != nil {
		t.Fatalf("write fixture extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(autoskDir, "settings.json"), []byte("{\"extensions\":[]}\n"), 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

// runRoot executes the CLI's root cobra command in-process inside dir and
// captures stdout + stderr. The daemon is auto-spawned on the first call.
func runRoot(t *testing.T, dir string, argv ...string) (string, error) {
	t.Helper()
	ensureTestDaemon(t)

	root := newRootCmd()
	root.SetArgs(argv)
	origStdout := os.Stdout
	origStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe
	os.Stderr = wPipe
	root.SetOut(wPipe)
	root.SetErr(wPipe)

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		os.Stdout = origStdout
		os.Stderr = origStderr
		t.Fatalf("chdir %s: %v", dir, err)
	}
	defer func() {
		_ = os.Chdir(cwd)
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	var out bytes.Buffer
	doneCh := make(chan struct{})
	go func() {
		_, _ = out.ReadFrom(rPipe)
		close(doneCh)
	}()
	err := root.Execute()
	_ = wPipe.Close()
	<-doneCh
	return out.String(), err
}

// shortSocketPath returns a private UDS path short enough for sun_path.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if len(base) > 20 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "askd")
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// siblingAutoskd is the autoskd path next to the test binary.
func siblingAutoskd() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "autoskd")
}
