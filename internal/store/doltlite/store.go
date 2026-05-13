// Package doltlite implements the store.Store interface backed by the
// embedded doltlite engine (a SQLite fork with a prolly-tree storage layer
// providing Dolt-style version control).
//
// We link against libdoltlite via the libsqlite3 build tag of mattn/go-sqlite3;
// see the project Makefile for the required CGO env vars.
//
// IMPORTANT: doltlite is single-writer. We force SetMaxOpenConns(1) so all
// queries share one connection — necessary for branch state and the file lock.
package doltlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "github.com/mattn/go-sqlite3" // SQLite driver, linked to libdoltlite

	"autosk/internal/migrations"
	"autosk/internal/store"
)

// Store is the doltlite implementation of store.Store.
type Store struct {
	db   *sql.DB
	path string
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
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("doltlite open: %w", err)
	}
	// Single connection — Dolt branch state and write lock live here.
	db.SetMaxOpenConns(1)

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
