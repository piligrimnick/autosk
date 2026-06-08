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
	// Point the daemon's npm at a hermetic fake BEFORE it spawns so agent
	// install / workflow auto-install / init bootstrap stay offline regardless
	// of when a test sets its packages prefix (the daemon's npm binary is fixed
	// at spawn time). Tests may still override AUTOSK_NPM_BIN before runRoot.
	if os.Getenv("AUTOSK_NPM_BIN") == "" {
		t.Setenv("AUTOSK_NPM_BIN", writeFakeNpmBinary(t))
	}

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

// writeFakeNpmBinary writes a hermetic fake-npm shell script and returns its
// path. It mirrors fakeNpmInProcess's on-disk shapes so the daemon's ExecNpm
// (`<bin> --prefix P install --save --no-audit --no-fund SPEC`, and the
// matching uninstall) stays offline + deterministic.
func writeFakeNpmBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-npm.sh")
	if err := os.WriteFile(path, []byte(fakeNpmScript), 0o755); err != nil {
		t.Fatalf("write fake npm: %v", err)
	}
	return path
}

// fakeNpmScript is the daemon-side fake npm. It parses the ExecNpm argv, strips
// a trailing @version (preserving a leading scope @), and writes the fixture
// package.json for the known names; unknown names exit non-zero (loud drift).
const fakeNpmScript = `#!/usr/bin/env bash
set -euo pipefail
prefix=""
verb=""
arg=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --prefix) prefix="$2"; shift 2 ;;
    install) verb="install"; shift ;;
    uninstall) verb="uninstall"; shift ;;
    --save|--no-audit|--no-fund) shift ;;
    *) arg="$1"; shift ;;
  esac
done
[ -n "$prefix" ] || { echo "fake-npm: no --prefix" >&2; exit 2; }
name="$arg"
case "$arg" in
  @*@*) name="${arg%@*}" ;;
  @*)   name="$arg" ;;
  *@*)  name="${arg%@*}" ;;
  *)    name="$arg" ;;
esac
dir="$prefix/node_modules/$name"
if [ "$verb" = "uninstall" ]; then
  rm -rf "$dir"
  exit 0
fi
mkdir -p "$dir"
case "$name" in
  @autosk/agent-runtime)
    printf '%s' '{"name":"@autosk/agent-runtime","version":"0.1.0"}' > "$dir/package.json" ;;
  @autosk/dev-fixture)
    printf '%s' '{"name":"@autosk/dev-fixture","version":"0.2.5","autosk":{"agent":{"first_message":"You are the dev fixture.","model":"sonnet:high","thinking":"high"}}}' > "$dir/package.json" ;;
  @autogent/generic)
    printf '%s' '{"name":"@autogent/generic","version":"0.1.0","autosk":{"agent":{}}}' > "$dir/package.json" ;;
  @autosk/custom-fixture)
    printf '%s' '{"name":"@autosk/custom-fixture","version":"1.0.0","autosk":{"agent":{"runner":"./agent.ts"}}}' > "$dir/package.json"
    printf 'export default async () => {};\n' > "$dir/agent.ts" ;;
  *)
    echo "fake-npm: unknown package $name" >&2
    exit 1 ;;
esac
exit 0
`
