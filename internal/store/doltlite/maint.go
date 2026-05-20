// Maintenance helpers for the doltlite store.
//
// doltlite's chunk-store is content-addressed: every write appends new
// chunks to the on-disk database, and stale chunks are reclaimed only
// by an explicit garbage-collection call. Until that call runs the
// chunk-store's internal WAL ("csReplayWal") keeps growing, and every
// query begins by replaying the whole thing — which is exactly the
// 100%-CPU footprint that surfaced in the `autosk lazy` dashboard
// after a few hours of daemon activity (see the troubleshooting note
// in docs/daemon.md).
//
// Compact wraps `SELECT dolt_gc()` so the daemon (or the `autosk gc`
// CLI) can schedule compaction without callers having to know about
// the underlying SQL function name. The output mirrors what
// `dolt_gc()` returns to the SQL caller:
//
//	"<removed> chunks removed, <kept> chunks kept"
//
// Both numbers are surfaced as a typed CompactResult so log lines can
// include them without re-parsing the string.

package doltlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"autosk/internal/store"
)

// CompactResult is the parsed return value of `SELECT dolt_gc()`.
// ChunksRemoved counts stale chunks reclaimed by the call; ChunksKept
// is the live working set after compaction.
//
// Duration is wall-clock time spent inside the SQL call (not the full
// round-trip from the caller's perspective; the caller adds its own
// timing if it wants that).
type CompactResult struct {
	ChunksRemoved int64
	ChunksKept    int64
	Raw           string // verbatim return string from dolt_gc()
	Duration      time.Duration
}

// String returns a compact human-readable summary suitable for log
// lines and CLI output.
func (r CompactResult) String() string {
	return fmt.Sprintf("removed=%d kept=%d duration=%s",
		r.ChunksRemoved, r.ChunksKept, r.Duration.Round(time.Millisecond))
}

// Compact invokes doltlite's chunk-store garbage collector.
//
// The call is a single SQL function invocation (`SELECT dolt_gc()`)
// and runs inside the doltlite single-writer connection — it
// serialises against any other write in this process. Read queries
// see the compacted chunk-store on their next BEGIN. The on-disk
// representation is rewritten via write-to-sidecar + atomic rename,
// which means the inode of `.autosk/db` changes; concurrent processes
// that opened the file before the rewrite keep their fd on the now-
// orphan inode and silently serve a stale snapshot. The defence is
// the doltlite store's SetConnMaxLifetime (see DefaultConnLifetime in
// store.go): every long-lived autosk process rotates its underlying
// *sqlite3.SQLiteConn periodically, so the next query re-opens the
// file at the current path and picks up the new inode. Within a
// rotation period (default 2s) the dashboard refreshes its chunk-
// store cache and is fast again post-GC.
//
// Compact is safe to call repeatedly; on a fresh DB it's a no-op
// (returns ChunksRemoved=0).
func (s *Store) Compact(ctx context.Context) (CompactResult, error) {
	if s.db == nil {
		return CompactResult{}, store.ErrNotOpen
	}
	start := time.Now()
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT dolt_gc()`).Scan(&raw); err != nil {
		return CompactResult{}, fmt.Errorf("doltlite compact: %w", err)
	}
	res := CompactResult{Raw: raw, Duration: time.Since(start)}
	// Parse "N chunks removed, M chunks kept". The format has been
	// stable across the doltlite versions we link against; if a
	// future version changes the wording we still surface the raw
	// string so the operator can see what happened, and the counters
	// just stay at zero (the daemon's log line is still useful).
	parseCounter(raw, "chunks removed", &res.ChunksRemoved)
	parseCounter(raw, "chunks kept", &res.ChunksKept)
	return res, nil
}

// parseCounter pulls "<int> <suffix>" out of s. Best-effort; on a
// no-match it leaves *out untouched (typically zero).
func parseCounter(s, suffix string, out *int64) {
	idx := strings.Index(s, suffix)
	if idx <= 0 {
		return
	}
	// Walk back from the suffix to find the integer.
	end := idx
	for end > 0 && s[end-1] == ' ' {
		end--
	}
	start := end
	for start > 0 && s[start-1] >= '0' && s[start-1] <= '9' {
		start--
	}
	if start == end {
		return
	}
	var n int64
	for i := start; i < end; i++ {
		n = n*10 + int64(s[i]-'0')
	}
	*out = n
}
