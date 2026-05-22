// Package migrations holds embedded SQL migrations and a small applier.
//
// Files are named NNN_*.sql where NNN is a zero-padded integer version.
// They are applied in numerical order, each inside its own transaction, and
// recorded in the schema_migrations table.
package migrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed *.sql
var sqlFS embed.FS

// Migration is a single numbered migration file.
type Migration struct {
	Version int
	Name    string // filename without extension
	SQL     string
}

var fileRE = regexp.MustCompile(`^(\d+)_([^.]+)\.sql$`)

// All returns every embedded migration sorted by version.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(sqlFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration filename %q does not match NNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(sqlFS, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: v, Name: m[2], SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// EnsureTrackingTable creates schema_migrations if missing. Idempotent.
//
// We do this here (not in 001_init.sql) so the migration system can record
// 001's own application — chicken-and-egg.
func EnsureTrackingTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`)
	return err
}

// CurrentVersion returns the highest applied version, or 0 if none.
func CurrentVersion(ctx context.Context, db *sql.DB) (int, error) {
	if err := EnsureTrackingTable(ctx, db); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	row := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`)
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// ApplyOptions narrows what Apply / ApplyTo will run.
type ApplyOptions struct {
	// Until, when > 0, caps the highest migration version that will be
	// applied. Migrations with Version > Until are skipped. Until == 0
	// means "no cap" (apply every embedded migration).
	Until int
}

// Apply runs all pending migrations in order. Returns the number applied.
//
// Each migration runs in its own transaction. If a migration fails, that
// transaction is rolled back and Apply returns the error; earlier successful
// migrations remain applied.
func Apply(ctx context.Context, db *sql.DB) (int, error) {
	return ApplyWith(ctx, db, ApplyOptions{})
}

// ApplyWith is Apply with caller-provided options. It is the single
// source of truth for the migration loop; Apply is a thin wrapper that
// passes a zero ApplyOptions.
//
// Tests that need "replay migrations 001..N" semantics (e.g. to seed a
// pre-N fixture before running migration N+1) use ApplyOptions{Until: N}
// instead of re-implementing the loop — future hooks on applyOne
// (logging, dispatch points, schema_migrations columns) stay in sync
// automatically.
func ApplyWith(ctx context.Context, db *sql.DB, opts ApplyOptions) (int, error) {
	migs, err := All()
	if err != nil {
		return 0, err
	}
	current, err := CurrentVersion(ctx, db)
	if err != nil {
		return 0, err
	}
	var applied int
	for _, m := range migs {
		if m.Version <= current {
			continue
		}
		if opts.Until > 0 && m.Version > opts.Until {
			break
		}
		if err := applyOne(ctx, db, m); err != nil {
			return applied, fmt.Errorf("migration %03d_%s: %w", m.Version, m.Name, err)
		}
		applied++
		// 001 creates the agents table; seed the `human` row exactly once.
		if m.Version == 1 {
			if err := SeedHumanAgent(ctx, db); err != nil {
				return applied, fmt.Errorf("seed human agent: %w", err)
			}
		}
	}
	return applied, nil
}

// SeedHumanAgent inserts the canonical `human` agent if it is not already
// present. Idempotent: safe to call on every Apply.
func SeedHumanAgent(ctx context.Context, db *sql.DB) error {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE name = 'human'`).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id, err := newAgentID()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO agents(id, name, is_human, created_at) VALUES (?, 'human', 1, ?)`,
		id, time.Now().Unix())
	return err
}

// newAgentID generates a unique "ag-XXXX" id. The migrations package owns
// this small helper because the seed step is the only place where we mint
// an id without going through the (yet-unbuilt) agent store.
func newAgentID() (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ag-" + hex.EncodeToString(b[:]), nil
}

func applyOne(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Split on semicolons at the start of lines so we can run multi-statement
	// migrations. SQLite's driver only accepts one statement per Exec on
	// some paths; doltlite inherits that. This naive split is fine because
	// our migrations never embed `;` inside string literals.
	for _, stmt := range splitStatements(m.SQL) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(stmt), err)
		}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
		m.Version, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("record schema_migrations: %w", err)
	}
	return tx.Commit()
}

func splitStatements(s string) []string {
	// Strip line comments first so a trailing `;` inside a comment isn't a split.
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if idx := strings.Index(ln, "--"); idx >= 0 {
			ln = ln[:idx]
		}
		lines = append(lines, ln)
	}
	cleaned := strings.Join(lines, "\n")
	return strings.Split(cleaned, ";")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
