// Package doltlite implements the store.Store interface backed by the
// embedded doltlite engine (a SQLite fork with a prolly-tree storage layer
// providing Dolt-style version control).
//
// We link against libdoltlite via the libsqlite3 build tag of mattn/go-sqlite3;
// see the project Makefile for the required CGO env vars.
//
// IMPORTANT: doltlite is single-writer. We force SetMaxOpenConns(1) so all
// queries share one connection — necessary for branch state and the file lock.
//
// Cross-process freshness. `SELECT dolt_gc()` atomically replaces the
// database file (write-to-sidecar + rename); any process whose connection
// was open at gc time keeps its file descriptor on the now-orphan inode
// and silently serves a stale snapshot forever. We defend by setting a
// short SetConnMaxLifetime so Go's `database/sql` pool retires the
// underlying *sqlite3.SQLiteConn periodically; the next query re-opens
// the file at the current path and picks up the new inode. See
// docs/lazy.md "cross-process freshness" and docs/daemon.md "compactor".
package doltlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver, linked to libdoltlite

	"autosk/internal/migrations"
	"autosk/internal/store"
)

// DefaultConnLifetime is the SetConnMaxLifetime we apply by default in
// Open. 2s matches the default `autosk lazy --refresh`, so the worst-
// case lag between a cross-process write and lazy seeing it is one
// refresh tick (~2s). Callers that need a different cadence call
// SetConnMaxLifetime after Open.
//
// Setting this to 0 disables connection rotation; do so only in tests
// (and only when the test never triggers `dolt_gc()` in another
// process — see TestConnRotation_AfterCrossProcessGC for the canonical
// regression).
const DefaultConnLifetime = 2 * time.Second

// Store is the doltlite implementation of store.Store.
type Store struct {
	db       *sql.DB
	path     string
	lifetime time.Duration // current SetConnMaxLifetime; mirrored for Reconnect
}

// Compile-time assertion that we satisfy the interface.
var _ store.Store = (*Store)(nil)

// New returns an unopened Store. Call Open to connect.
func New() *Store { return &Store{} }

// Open connects to a doltlite database at dbPath. Pass ":memory:" for a
// transient in-memory db (useful for tests).
//
// Open does NOT run migrations automatically; call Migrate after Open.
func (s *Store) Open(ctx context.Context, dbPath string) error {
	if s.db != nil {
		return errors.New("doltlite: already open")
	}
	// Enable foreign keys via the driver DSN. Also set a busy_timeout so
	// concurrent CLI invocations queue on the doltlite write lock instead of
	// returning SQLITE_BUSY immediately. The default (30s) is generous; tune
	// later via AUTOSK_LOCK_TIMEOUT if needed.
	dsn := dbPath
	if dbPath != ":memory:" {
		q := url.Values{}
		q.Set("_foreign_keys", "on")
		q.Set("_busy_timeout", "30000")
		dsn = dbPath + "?" + q.Encode()
	}
	// Use the inode-validating driver (see driver.go) so the pool
	// retires connections whose underlying fd points at an orphan
	// inode — the recovery path against a cross-process dolt_gc()
	// that atomic-renamed the file under us.
	registerDriver()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return fmt.Errorf("doltlite open: %w", err)
	}
	// Single connection — Dolt branch state and write lock live here.
	db.SetMaxOpenConns(1)
	// Rotate the underlying conn periodically so we recover from a
	// cross-process dolt_gc that atomic-renames the database file out
	// from under our fd. See the package-level comment for the gory
	// details. SetConnMaxLifetime can be tuned by callers after Open
	// via SetConnMaxLifetime.
	db.SetConnMaxLifetime(DefaultConnLifetime)
	s.lifetime = DefaultConnLifetime

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("doltlite ping: %w", err)
	}
	// Always pragma busy_timeout/foreign_keys explicitly. The DSN params are
	// honored by mattn/go-sqlite3 on new connections, but :memory: ignores
	// them and even file DBs benefit from a belt-and-suspenders PRAGMA —
	// some doltlite paths acquire locks before the driver applies DSN params.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 30000`); err != nil {
		_ = db.Close()
		return fmt.Errorf("set busy timeout: %w", err)
	}
	s.db = db
	s.path = dbPath
	return nil
}

// Close releases the underlying database.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// DB exposes the underlying *sql.DB for things that need raw SQL access
// (the `autosk sql` command, doltlite-specific helpers). Returns nil before Open.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the on-disk path the Store was opened against (":memory:"
// for in-memory stores). Empty before Open. Used by callers that need
// to derive the project root from the resolved .autosk/db location
// without re-running projectdb.Resolve.
func (s *Store) Path() string { return s.path }

// SetConnMaxLifetime overrides the default connection-rotation cadence
// set by Open. Long-lived readers that care about seeing cross-process
// writes promptly (e.g. the `autosk lazy` dashboard) wire this to their
// own refresh interval. Pass 0 to disable rotation entirely.
//
// No-op when the Store hasn't been opened yet.
func (s *Store) SetConnMaxLifetime(d time.Duration) {
	if s.db == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	s.db.SetConnMaxLifetime(d)
	s.lifetime = d
}

// ConnMaxLifetime returns the lifetime currently in effect on the
// underlying *sql.DB. Surfaced primarily for tests and observability.
func (s *Store) ConnMaxLifetime() time.Duration { return s.lifetime }

// Reconnect forces the underlying *sql.DB to drop any pooled connection
// immediately. The next Query/Exec opens a fresh sqlite3 conn against
// the file at the current path — which is the lever that recovers from
// a cross-process `dolt_gc()` rewrite (atomic rename leaves our fd on
// the orphan inode). Routine rotation is handled by SetConnMaxLifetime;
// Reconnect is the manual escape hatch surfaced as Ctrl-R in `autosk
// lazy`.
//
// Safe to call concurrently with in-flight queries: those finish on
// their current conn, and only the NEXT acquisition gets the fresh
// conn. Returns the error from a follow-up Ping that proves a fresh
// conn was successfully opened.
func (s *Store) Reconnect(ctx context.Context) error {
	if s.db == nil {
		return store.ErrNotOpen
	}
	// Trick the pool into retiring its idle conn: a 1ns lifetime
	// expires anything sitting idle, then we restore the configured
	// cadence. Per database/sql docs, in-flight conns aren't disturbed.
	prev := s.lifetime
	s.db.SetConnMaxLifetime(time.Nanosecond)
	s.db.SetConnMaxLifetime(prev)
	return s.db.PingContext(ctx)
}

// Migrate applies any pending schema migrations. Idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	if s.db == nil {
		return store.ErrNotOpen
	}
	_, err := migrations.Apply(ctx, s.db)
	return err
}

// SchemaVersion returns the highest applied migration version, or 0 if none.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	if s.db == nil {
		return 0, store.ErrNotOpen
	}
	return migrations.CurrentVersion(ctx, s.db)
}

// --- Method dispatch lives in companion files: ----------------------------
//
//   CreateTask + GetTask                       → tasks.go  (P2)
//   ListTasks + UpdateTask + Claim + Ready     → query.go  (P3)
//   Block + Unblock + UnblockAll + Deps        → deps.go   (P4)
//
// Raw passthrough stays here so the file remains the canonical "entry".

func (s *Store) QueryRaw(ctx context.Context, q string, args ...any) (store.Rows, error) {
	if s.db == nil {
		return nil, store.ErrNotOpen
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) ExecRaw(ctx context.Context, q string, args ...any) (store.Result, error) {
	if s.db == nil {
		return nil, store.ErrNotOpen
	}
	return s.db.ExecContext(ctx, q, args...)
}
