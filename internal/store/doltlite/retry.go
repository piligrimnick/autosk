package doltlite

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"
)

// LockRetryAttempts caps the retry loop for "database is locked" errors.
// Doltlite's prolly-tree storage layer doesn't always honor SQLite's
// busy_timeout, so we add belt-and-suspenders retries at the Go level.
const (
	LockRetryAttempts = 50
	LockRetryBase     = 20 * time.Millisecond
	LockRetryMax      = 500 * time.Millisecond
)

// isLockBusy reports whether err looks like a transient lock-contention error
// from doltlite/sqlite. We string-match because the driver doesn't expose a
// typed error for this case.
func isLockBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

// retryOnBusy runs fn, retrying with exponential backoff (capped) while it
// returns a lock-contention error. Honors ctx cancellation.
func retryOnBusy(ctx context.Context, fn func() error) error {
	delay := LockRetryBase
	var lastErr error
	for i := 0; i < LockRetryAttempts; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isLockBusy(err) {
			return err
		}
		lastErr = err
		// Jittered backoff.
		jitter := time.Duration(rand.Int63n(int64(delay / 2)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay + jitter):
		}
		delay *= 2
		if delay > LockRetryMax {
			delay = LockRetryMax
		}
	}
	if lastErr == nil {
		lastErr = errors.New("doltlite: lock retry exhausted")
	}
	return lastErr
}
