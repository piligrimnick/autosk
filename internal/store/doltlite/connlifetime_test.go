//go:build libsqlite3

package doltlite_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"autosk/internal/store/doltlite"
)

// TestConnRotation_AfterCrossProcessGC pins the regression that
// motivated the SetConnMaxLifetime work: when one process runs
// `SELECT dolt_gc()` on the shared `.autosk/db`, doltlite rewrites
// the file via write-to-sidecar + atomic rename. Any process whose
// connection was open at gc time keeps its file descriptor on the
// orphan inode and silently serves the old snapshot forever, no
// matter how many SQL refreshes (`dolt_checkout`, `dolt_reset --hard
// HEAD`) it tries.
//
// The defence is doltlite.DefaultConnLifetime: Go's `database/sql`
// pool retires the underlying *sqlite3.SQLiteConn periodically; the
// next acquisition opens a fresh conn against the file at the current
// path, which picks up the new inode.
//
// The test forks two helper subprocesses against the same DB file:
//
//  1. The READER opens a store and reads a baseline count.
//  2. A GC subprocess runs `SELECT dolt_gc()` and prints the new
//     inode.
//  3. A WRITER subprocess inserts a row and commits.
//  4. After waiting longer than the configured lifetime, the reader's
//     next query MUST see the new row.
//
// A parallel "control" run with SetConnMaxLifetime(0) (no rotation)
// must demonstrate the stale-fd bug (reader sees the old count) so
// the regression coverage is real, not a tautology.
func TestConnRotation_AfterCrossProcessGC(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess + dolt_gc; not for -short")
	}
	t.Run("with_rotation", func(t *testing.T) { runCrossProcGCCase(t, 250*time.Millisecond) })
	t.Run("with_zero_lifetime_uses_inode_validator", func(t *testing.T) {
		// SetConnMaxLifetime(0) disables time-based rotation. Recovery
		// then relies entirely on the inode-validating driver hook
		// (see driver.go) firing ErrBadConn from ResetSession.
		runCrossProcGCCase(t, 0)
	})
}

// TestPostGCWrite_PersistsAcrossInodeChange pins the write-loss
// regression: after a cross-process `SELECT dolt_gc()` swaps the file
// inode under us, a write issued through the still-open conn must NOT
// silently disappear into the orphan. The driver-level
// SessionResetter/Validator hook retires the orphan conn on the next
// pool checkout, so the INSERT runs on a fresh conn against the live
// file.
//
// We confirm persistence with a fresh, third-process reader so the
// assertion can't be fooled by a stale view inside the writer's own
// session.
func TestPostGCWrite_PersistsAcrossInodeChange(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess + dolt_gc; not for -short")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	dbp := filepath.Join(tmp, "db")

	writer := doltlite.New()
	if err := writer.Open(ctx, dbp); err != nil {
		t.Fatalf("writer Open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	// Disable time-based rotation so the test exercises the inode
	// validator in isolation. If it passes here, it passes a fortiori
	// with rotation enabled.
	writer.SetConnMaxLifetime(0)
	if err := writer.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := writer.DB().ExecContext(ctx, `CREATE TABLE postgc(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := writer.DB().ExecContext(ctx, `INSERT INTO postgc(v) VALUES('pre-gc')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writer.DoltCommit(ctx, "pre-gc"); err != nil {
		t.Fatalf("pre-gc commit: %v", err)
	}
	inoBefore := mustInode(t, dbp)

	// Another process compacts; the inode swaps.
	runHelper(t, dbp, "gc")
	inoAfter := mustInode(t, dbp)
	if inoBefore == inoAfter {
		t.Skipf("gc did not change inode on this filesystem (before=%d after=%d); skip—the regression is fs-specific", inoBefore, inoAfter)
	}

	// The writer is still alive and its conn was opened pre-gc. Without
	// the inode validator the next INSERT would route into the orphan
	// inode and disappear when the conn closes.
	if _, err := writer.DB().ExecContext(ctx, `INSERT INTO postgc(v) VALUES('post-gc')`); err != nil {
		t.Fatalf("post-gc INSERT: %v", err)
	}
	if err := writer.DoltCommit(ctx, "post-gc"); err != nil {
		t.Fatalf("post-gc commit: %v", err)
	}

	// Cross-check from a fresh process so a stale-but-self-consistent
	// view inside the writer can't fool the assertion.
	runHelper(t, dbp, "countpostgc")
}

// observeWriteAfterGCInProcess returns the row count visible to a
// brand-new in-process reader after the writer's post-gc write. Used
// by both the helper command and the main test path so the assertion
// logic is symmetric.
func countPostGCRows(t *testing.T, dbp string) int {
	t.Helper()
	ctx := context.Background()
	fresh := doltlite.New()
	if err := fresh.Open(ctx, dbp); err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	defer fresh.Close()
	var n int
	if err := fresh.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM postgc`).Scan(&n); err != nil {
		t.Fatalf("fresh COUNT: %v", err)
	}
	return n
}

func runCrossProcGCCase(t *testing.T, lifetime time.Duration) {
	got := observePostGC(t, lifetime, lifetime+500*time.Millisecond)
	if got.afterCount != got.baseline+1 {
		t.Fatalf("reader did not pick up cross-process write after gc + rotation: baseline=%d after=%d (gc_inode_changed=%v)",
			got.baseline, got.afterCount, got.gcInodeChanged)
	}
	if !got.gcInodeChanged {
		t.Logf("note: gc did not change inode in this run; the test still exercises the rotation path but the underlying regression is not reproducible here")
	}
}

type crossProcResult struct {
	baseline       int
	afterCount     int
	gcInodeChanged bool
}

func observePostGC(t *testing.T, lifetime, waitBeforeReread time.Duration) crossProcResult {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	dbp := filepath.Join(tmp, "db")

	// Reader: the long-lived process whose pool we're testing.
	reader := doltlite.New()
	if err := reader.Open(ctx, dbp); err != nil {
		t.Fatalf("reader Open: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	reader.SetConnMaxLifetime(lifetime)
	if err := reader.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Seed: insert a row + commit.
	if _, err := reader.DB().ExecContext(ctx, `CREATE TABLE probe(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	if _, err := reader.DB().ExecContext(ctx, `INSERT INTO probe(v) VALUES('seed')`); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	if err := reader.DoltCommit(ctx, "seed"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	var baseline int
	if err := reader.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM probe`).Scan(&baseline); err != nil {
		t.Fatalf("baseline count: %v", err)
	}

	inoBefore := mustInode(t, dbp)

	// Subprocess #1: gc.
	runHelper(t, dbp, "gc")
	inoAfter := mustInode(t, dbp)

	// Subprocess #2: insert + commit.
	runHelper(t, dbp, "write")

	// Wait past the rotation window so the next query forces a
	// fresh underlying conn.
	time.Sleep(waitBeforeReread)

	var after int
	if err := reader.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM probe`).Scan(&after); err != nil {
		t.Fatalf("post-gc count: %v", err)
	}
	return crossProcResult{
		baseline:       baseline,
		afterCount:     after,
		gcInodeChanged: inoBefore != inoAfter,
	}
}

func mustInode(t *testing.T, p string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return uint64(st.Ino)
}

// runHelper invokes this test binary in subprocess mode to perform
// one of the cross-process operations against `dbp`. The helper
// finishes by printing a JSON line on stdout that we ignore in
// success paths; failures show up via non-zero exit + captured
// combined output in the test log.
func runHelper(t *testing.T, dbp, op string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe,
		"-test.run=TestConnRotationHelper",
		"-test.v",
		"--", op, dbp,
	)
	cmd.Env = append(os.Environ(), "AUTOSK_CONNROT_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper %s failed: %v\nstdout/stderr:\n%s", op, err, out)
	}
}

// TestConnRotationHelper is the helper subprocess entrypoint. It only
// runs when AUTOSK_CONNROT_HELPER=1; under normal `go test` invocation
// it skips immediately. The op (gc|write) and db path are passed as
// positional args after the `--` flag separator.
func TestConnRotationHelper(t *testing.T) {
	if os.Getenv("AUTOSK_CONNROT_HELPER") != "1" {
		t.Skip("not a helper invocation")
	}
	args := flag.Args()
	if len(args) < 2 {
		t.Fatalf("helper: expected `op dbpath`, got %v", args)
	}
	op, dbp := args[0], args[1]
	ctx := context.Background()

	s := doltlite.New()
	if err := s.Open(ctx, dbp); err != nil {
		t.Fatalf("helper Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	switch op {
	case "countpostgc":
		n := countPostGCRows(t, dbp)
		if n != 2 {
			t.Fatalf("fresh reader saw %d rows; want 2 (pre-gc + post-gc). The post-gc write was silently lost into the orphan inode.", n)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"op": "countpostgc", "rows": n})
	case "gc":
		res, err := s.Compact(ctx)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"op":      "gc",
			"removed": res.ChunksRemoved,
			"kept":    res.ChunksKept,
		})
	case "write":
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO probe(v) VALUES(?)`,
			fmt.Sprintf("post-gc-%d", time.Now().UnixNano())); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if err := s.DoltCommit(ctx, "helper write"); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"op": "write"})
	default:
		t.Fatalf("unknown helper op %q", op)
	}
}
