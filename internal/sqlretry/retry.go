// Package sqlretry is a tiny belt-and-suspenders retry helper for the
// doltlite-backed *sql.DB shared across the daemon.
//
// Doltlite's prolly-tree storage layer doesn't always honor SQLite's
// busy_timeout: a "database is locked" / SQLITE_BUSY error can leak
// past it even though we hold the writer to a single connection via
// SetMaxOpenConns(1). Every write path that touches the daemon's
// shared *sql.DB should therefore funnel through OnBusy.
//
// Tuning rationale (50 attempts × 20ms .. 500ms with jitter) lives
// here rather than at call sites so all callers share one knob.
package sqlretry

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"
)

// MaxAttempts caps the retry loop for transient lock-contention errors.
const (
	MaxAttempts = 50
	BaseDelay   = 20 * time.Millisecond
	MaxDelay    = 500 * time.Millisecond
)

// IsLockBusy reports whether err looks like a transient lock-contention
// error from doltlite / mattn-go-sqlite3. The driver doesn't expose a
// typed error for this case, so we string-match the two known forms.
func IsLockBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

// OnBusy runs fn, retrying with exponential backoff (capped, jittered)
// while it returns a lock-contention error. Honors ctx cancellation —
// the caller's deadline always wins over our retry budget.
func OnBusy(ctx context.Context, fn func() error) error {
	delay := BaseDelay
	var lastErr error
	for i := 0; i < MaxAttempts; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !IsLockBusy(err) {
			return err
		}
		lastErr = err
		jitter := time.Duration(rand.Int63n(int64(delay / 2)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay + jitter):
		}
		delay *= 2
		if delay > MaxDelay {
			delay = MaxDelay
		}
	}
	if lastErr == nil {
		lastErr = errors.New("sqlretry: lock retry exhausted")
	}
	return lastErr
}
