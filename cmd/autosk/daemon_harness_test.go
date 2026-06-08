package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/rpcclient"
)

// --- CLI verb test harness: per-test isolated autoskd -----------------------
//
// The CLI write+read verbs route through autoskd (the daemon owns
// .autosk/db under the single-writer model). The in-process cobra command
// is therefore only a thin RPC client; a verb test needs a live daemon to
// talk to. ensureTestDaemon gives each test its own isolated daemon:
//
//   - AUTOSK_SOCK   — a short private UDS path (sun_path must stay < ~104
//     bytes, so we cannot use the long macOS $TMPDIR / t.TempDir()).
//   - HOME          — an isolated home so the daemon-token and the default
//     packages prefix never collide with the operator's real ~/.autosk
//     (which may host a long-lived daemon on the default socket).
//   - AUTOSK_NO_EXEC — the daemon serves reads+writes but never auto-runs
//     workflow steps, so verb assertions are deterministic and agent-free.
//
// The daemon itself is auto-spawned lazily by the rpcclient connector on
// the first verb call (it inherits the env above), and is asked to stop on
// test cleanup so we do not leak one process per test.
//
// Resolution of the autoskd binary is left to the connector: AUTOSKD_BIN
// (set by the Makefile test targets, which build autoskd first) → a sibling
// binary → PATH. When none is available we t.Skip with a pointer at `make
// test`, rather than emitting a wall of confusing connection failures.

var harnessDaemons sync.Map // *testing.T -> struct{}: set-up guard, one daemon per test

// ensureTestDaemon configures the current test to talk to its own isolated
// autoskd. Idempotent: safe to call from every runRoot invocation within a
// test (only the first call provisions the daemon + cleanup).
func ensureTestDaemon(t *testing.T) {
	t.Helper()
	if _, done := harnessDaemons.Load(t); done {
		return
	}
	if os.Getenv("AUTOSKD_BIN") == "" {
		if _, err := os.Stat(siblingAutoskd()); err != nil {
			t.Skip("autoskd binary not found: set AUTOSKD_BIN or run via `make test` / `make test-short` (which build autoskd first)")
		}
	}

	sock := shortSocketPath(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AUTOSK_SOCK", sock)
	t.Setenv("AUTOSK_NO_EXEC", "1")

	harnessDaemons.Store(t, struct{}{})
	t.Cleanup(func() {
		// Best-effort: ask the (possibly auto-spawned) daemon to stop so a
		// full run does not leave one lingering process per test. We never
		// auto-spawn here — if no daemon came up, there is nothing to stop.
		if cl, err := rpcclient.New(rpcclient.Options{Sock: sock, NoAutoSpawn: true}); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = cl.Shutdown(ctx)
			cancel()
		}
		harnessDaemons.Delete(t)
	})
}

// shortSocketPath returns a private UDS path short enough for sun_path.
// macOS $TMPDIR (and hence t.TempDir()) is well over 60 bytes, which
// overflows the 104-byte sun_path once the daemon appends nothing more, so
// we anchor under a short base (/tmp) when the default temp dir is long.
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

// siblingAutoskd is the autoskd path next to the test binary, mirroring the
// connector's fallback. Used only to decide whether to skip.
func siblingAutoskd() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "autoskd")
}
