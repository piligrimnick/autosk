// Package uds wraps net.Listen("unix", ...) with single-instance semantics
// suitable for the autosk daemon socket.
//
// Listen ensures the parent directory exists with mode 0700, refuses to
// take over a socket whose other end accepts connections (a live peer),
// unlinks stale leftovers, binds, and chmods the resulting socket to
// 0600. Cleanup deletes the socket on shutdown.
//
// Per docs/plans/20260518-Daemon-UDS-Plan.md §4.3.
package uds

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrAlreadyRunning is returned when the socket path already has a live peer.
var ErrAlreadyRunning = errors.New("uds: daemon already running")

// dialTimeout is the time we give a stale-vs-live probe before deciding
// the existing socket has no listener.
const dialTimeout = 200 * time.Millisecond

// Listen returns a unix-domain listener bound at path with strict single-
// instance semantics:
//
//  1. filepath.Dir(path) is created with mode 0700 (best-effort).
//  2. If a file already exists at path:
//     - net.Dial succeeds  → ErrAlreadyRunning.
//     - dial fails with ECONNREFUSED/ENOENT → unlink + bind.
//  3. After net.Listen succeeds, os.Chmod(path, 0o600).
//
// Any error from os.Remove other than ENOENT short-circuits — we never
// blindly clobber a path we cannot identify as our own stale socket.
func Listen(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("uds: empty socket path")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		// Best-effort: if the directory already exists with a different
		// mode we don't try to chmod it (mDirs created by the user are
		// out of our scope; we only own the socket file itself).
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("uds: mkdir %s: %w", dir, err)
		}
	}

	if _, err := os.Lstat(path); err == nil {
		// Path exists. Decide: live peer or stale leftover?
		c, derr := net.DialTimeout("unix", path, dialTimeout)
		if derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("%w at %s", ErrAlreadyRunning, path)
		}
		// Not a live peer — remove and continue.
		if !isDeadSocketErr(derr) {
			// Some other dial error (permission denied etc.) — surface so
			// the user can fix the root cause.
			return nil, fmt.Errorf("uds: probe %s: %w", path, derr)
		}
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return nil, fmt.Errorf("uds: remove stale socket %s: %w", path, rerr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("uds: stat %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("uds: listen %s: %w", path, err)
	}
	if cerr := os.Chmod(path, 0o600); cerr != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("uds: chmod %s: %w", path, cerr)
	}
	return ln, nil
}

// Cleanup removes the socket file at path. Errors other than ENOENT are
// surfaced; ENOENT is treated as a no-op.
func Cleanup(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// isDeadSocketErr reports whether a Dial error is consistent with a
// stale leftover (no peer accepting connections, or socket file missing
// between Stat and Dial).
func isDeadSocketErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENOENT) {
		return true
	}
	// net.OpError sometimes wraps a *os.PathError or a generic string.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.ECONNREFUSED) ||
			errors.Is(opErr.Err, syscall.ECONNRESET) ||
			errors.Is(opErr.Err, syscall.ENOENT) {
			return true
		}
	}
	return false
}
