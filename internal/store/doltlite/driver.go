// driver.go wires an inode-validating wrapper around mattn/go-sqlite3's
// driver so `database/sql`'s connection pool can detect and retire
// connections whose underlying file descriptor points at an orphan
// inode — the failure mode that surfaces when another process runs
// `SELECT dolt_gc()` against the same `.autosk/db`. doltlite's gc
// rewrites the database via write-to-sidecar + atomic rename, so the
// inode of the path changes on every successful run; an unsuspecting
// reader keeps its fd on the orphan and silently serves the pre-gc
// snapshot, while an unsuspecting writer routes its INSERT/COMMIT
// into the orphan where it disappears the moment the conn closes
// (silent data loss).
//
// What we get with this wrapper:
//
//   - Every idle-conn checkout calls ResetSession (Go 1.10+) and
//     IsValid (Go 1.15+); we stat the path and report ErrBadConn /
//     false when the live inode no longer matches the one we opened
//     against. database/sql then retires the conn and opens a fresh
//     one, which sqlite3_open()s the file at the current path and
//     attaches to the post-gc state.
//
//   - The check runs between every pair of pool acquisitions — i.e.
//     between every pair of independent Exec/Query calls. That covers
//     the common case where a write follows a read or vice versa on
//     the same *sql.DB.
//
// What we DON'T cover, and is left as a known residual race:
//
//   - GC that finishes MID-statement. If gc completes between the
//     ResetSession check and the actual Exec returning, the Exec runs
//     on a now-orphan conn and its bytes go to the orphan. doltlite's
//     per-file flock can't help here because the gc'ing process locks
//     a different fd (the new sidecar, which then gets renamed onto
//     the path). Window: a single statement's duration (≤1ms typical)
//     vs the gap between gc cycles (default 30m), so the per-write
//     loss probability is on the order of 10^-7.
//
//   - GC that finishes MID-transaction. A conn held for the lifetime
//     of a BeginTx … Commit doesn't pass back through the pool, so
//     the validator doesn't get a chance to fire between statements
//     inside the tx. The window expands to the whole tx duration
//     (still bounded by ms typically). Transactions in autosk are
//     short and almost always write-only; the probability of a
//     concurrent gc landing inside one is similarly tiny.
//
// Both residual races are addressable only by adding a cross-process
// advisory lock (writers hold shared, the compactor holds exclusive)
// against a dedicated lock file. That's tracked as future work; the
// inode validator on its own brings the write-loss probability from
// "observable on every gc cycle" down to "will never be seen by
// anyone in practice".
package doltlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/mattn/go-sqlite3"
)

// driverName is the name we register our inode-validating wrapper
// under. Open uses sql.OpenDB(connector) instead of sql.Open so the
// driver registration is process-wide and not tied to a magic string
// callers might collide on.
const driverName = "sqlite3+autosk-inode-check"

// regOnce ensures the driver registration happens exactly once per
// process even when multiple Stores are opened concurrently.
var regOnce sync.Once

// registerDriver wires our wrapper into database/sql's registry. The
// wrapper Open()s an underlying mattn/go-sqlite3 conn, then decorates
// it with an inode-validating Conn that returns driver.ErrBadConn (and
// reports IsValid()==false) when `.autosk/db` has been replaced under
// our fd.
//
// We rely on the wrapper's Validator/SessionResetter hooks rather than
// post-hoc inode checks because `database/sql` calls them on every
// pool checkout: that's the natural place to retire a stale conn
// without touching every Store method.
func registerDriver() {
	regOnce.Do(func() {
		sql.Register(driverName, &checkingDriver{inner: &sqlite3.SQLiteDriver{}})
	})
}

// checkingDriver is the driver-level wrapper. It delegates Open to
// mattn/go-sqlite3 and immediately wraps the returned conn in a
// checkingConn that knows what file inode it was opened against.
type checkingDriver struct {
	inner driver.Driver
}

// Open implements driver.Driver.
func (d *checkingDriver) Open(dsn string) (driver.Conn, error) {
	inner, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	path := dsnPath(dsn)
	c := &checkingConn{inner: inner, path: path}
	c.refreshInode()
	return c, nil
}

// dsnPath strips mattn/go-sqlite3 DSN query parameters from the
// caller's path so syscall.Stat can find it. Mirrors the trivial
// parser the driver uses internally; we only care about correctness
// for our own DSNs (".../db?_foreign_keys=on&_busy_timeout=30000"
// and ":memory:"), so we don't need URI mode support.
func dsnPath(dsn string) string {
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		dsn = dsn[:i]
	}
	dsn = strings.TrimPrefix(dsn, "file:")
	return dsn
}

// checkingConn wraps a driver.Conn from mattn/go-sqlite3 and:
//
//  1. Records the inode of the underlying file at open time.
//  2. Validates the inode in ResetSession (called on every pool
//     checkout of an idle conn) and IsValid (also called by the
//     pool around checkout; Go 1.15+).
//
// When the cached inode no longer matches the live file we return
// driver.ErrBadConn / IsValid()==false; database/sql then closes the
// conn and opens a fresh one against the current path.
//
// All other driver.Conn methods are forwarded verbatim. We
// implement every "context" subinterface mattn/go-sqlite3 itself
// implements so wrapping doesn't downgrade us to the slower
// non-context paths.
type checkingConn struct {
	inner driver.Conn
	path  string
	// inode is the device-independent inode of the underlying file
	// at last open. Stored as atomic.Uint64 so the value can be
	// updated without locking (refreshInode is only ever called from
	// the goroutine that opened the conn, but ResetSession may run
	// from any goroutine the pool happens to pick).
	inode atomic.Uint64
}

// refreshInode re-stats the path and caches the inode. Called once
// at open time; never again (the whole point is to NOT update the
// cache until we get rid of the conn).
func (c *checkingConn) refreshInode() {
	if c.path == "" || c.path == ":memory:" {
		return
	}
	var st syscall.Stat_t
	if err := syscall.Stat(c.path, &st); err != nil {
		// File missing mid-rename (e.g. doltlite gc between rename
		// and rename-target settle); leave inode at zero. The next
		// ResetSession will see the live inode and fire ErrBadConn,
		// which is exactly the recovery path.
		c.inode.Store(0)
		return
	}
	c.inode.Store(uint64(st.Ino))
}

// staleInode reports true when the underlying file path no longer
// resolves to the same inode the conn was opened with. The ":memory:"
// case (and any DSN we couldn't parse a path from) is never stale.
//
// A transient syscall.Stat error (e.g. ENOENT in the tiny window
// between unlink-and-rename inside an atomic-replace) is reported as
// stale so the conn is retired and the next acquisition re-opens
// after the file settles.
func (c *checkingConn) staleInode() bool {
	if c.path == "" || c.path == ":memory:" {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Stat(c.path, &st); err != nil {
		return true
	}
	return uint64(st.Ino) != c.inode.Load()
}

// ---- driver.SessionResetter ---------------------------------------------

// ResetSession is called by database/sql's pool on every idle conn
// checkout. Returning driver.ErrBadConn here causes the pool to
// close this conn and open a fresh one, which is exactly the
// recovery path we want when `.autosk/db` was replaced by gc.
func (c *checkingConn) ResetSession(ctx context.Context) error {
	if c.staleInode() {
		return driver.ErrBadConn
	}
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// ---- driver.Validator ---------------------------------------------------

// IsValid is called by database/sql when checking out an idle conn
// (in addition to ResetSession). Returning false also triggers the
// pool to retire the conn.
func (c *checkingConn) IsValid() bool {
	if c.staleInode() {
		return false
	}
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// ---- driver.Conn passthroughs -------------------------------------------

func (c *checkingConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(query)
}
func (c *checkingConn) Close() error             { return c.inner.Close() }
func (c *checkingConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

// ---- driver.{Conn,Stmt}{Exec,Query}{Context}, driver.Pinger -------------

func (c *checkingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if cp, ok := c.inner.(driver.ConnPrepareContext); ok {
		return cp.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

func (c *checkingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if cb, ok := c.inner.(driver.ConnBeginTx); ok {
		return cb.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

func (c *checkingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.inner.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, errors.New("doltlite: inner conn does not implement ExecerContext")
}

func (c *checkingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.inner.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, errors.New("doltlite: inner conn does not implement QueryerContext")
}

func (c *checkingConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}
