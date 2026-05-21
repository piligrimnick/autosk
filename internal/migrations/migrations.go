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

// ErrSchemaV1Unsupported is returned when a v0.1-shaped database is opened
// with a v0.2 binary. See docs/plans/20260517-Workflows-Plan.md §7.
var ErrSchemaV1Unsupported = errors.New("schema_v1_unsupported: this database was created by autosk v0.1; wipe .autosk/db and re-init")

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
//
// Before running anything, Apply checks for v0.1 leftovers and refuses if
// it finds them — there is no migration path from v0.1 to v0.2.
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
	if err := CheckV1(ctx, db); err != nil {
		return 0, err
	}
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

// CheckV1 returns ErrSchemaV1Unsupported when the database carries v0.1
// shape markers — schema_migrations rows present but no `agents` table.
// On a brand-new DB (no tables at all) it returns nil. On a v0.2 DB it
// also returns nil.
func CheckV1(ctx context.Context, db *sql.DB) error {
	// schema_migrations may or may not exist yet. If it doesn't, the DB is
	// fresh and there's nothing to refuse.
	var migName sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&migName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe schema_migrations: %w", err)
	}
	// Has migrations been applied?
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if n == 0 {
		return nil // empty tracking table, DB is fresh-ish
	}
	// v0.2 always creates the `agents` table in migration 001. If at least
	// one migration is recorded but `agents` is missing, this is a v0.1 DB.
	var agentsName sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='agents'`).Scan(&agentsName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSchemaV1Unsupported
	}
	if err != nil {
		return fmt.Errorf("probe agents: %w", err)
	}
	return nil
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
	if hasFKOffDirective(m.SQL) {
		return applyOneFKOff(ctx, db, m)
	}
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

// fkOffDirective is the leading-comment directive that asks applyOne to
// run the migration with `PRAGMA foreign_keys=OFF`. It MUST be applied
// outside any transaction (SQLite requirement) and is needed by any
// migration that rebuilds a table with incoming CASCADE FKs — the
// canonical "DROP TABLE old" inside the rebuild would otherwise cascade
// and wipe dependent rows. See 006_short_statuses.sql for the first
// consumer.
const fkOffDirective = "autosk-migration: foreign_keys_off"

// hasFKOffDirective reports whether the migration body opens with the
// `-- autosk-migration: foreign_keys_off` directive. The directive must
// appear in a leading comment line; once a non-comment SQL statement is
// seen the scan stops so future directives can't be hidden mid-file.
func hasFKOffDirective(s string) bool {
	for _, ln := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "--") {
			return false
		}
		if strings.Contains(trimmed, fkOffDirective) {
			return true
		}
	}
	return false
}

// applyOneFKOff runs a migration with `PRAGMA foreign_keys=OFF`. The
// PRAGMA must live outside any transaction (SQLite enforces this), so
// we pin a dedicated connection from the pool for the whole sequence:
//
//  1. acquire *sql.Conn
//  2. PRAGMA foreign_keys = OFF
//  3. BEGIN tx
//  4. exec migration statements
//  5. PRAGMA foreign_key_check — abort on any violation
//  6. record schema_migrations row
//  7. COMMIT
//  8. PRAGMA foreign_keys = ON
//  9. release the conn
//
// doltlite pins SetMaxOpenConns(1) so the entire pool funnels through
// one connection regardless, but acquiring an explicit *sql.Conn keeps
// the contract robust against future pool-size changes and against
// callers that pass a vanilla database/sql handle (e.g. tests).
func applyOneFKOff(ctx context.Context, db *sql.DB, m Migration) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	// Restore FK enforcement on the way out. The connection is about to
	// be returned to the pool (or closed by defer) and doltlite's pool
	// rotates connections within DefaultConnLifetime anyway, but flipping
	// the pragma back keeps the immediate post-migration window safe.
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range splitStatements(m.SQL) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(stmt), err)
		}
	}

	// Verify no dangling FK references slipped through the rebuild
	// before we commit; the alternative is silent breakage the moment
	// FK enforcement is re-enabled.
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	var violations []string
	for rows.Next() {
		var (
			table  sql.NullString
			rowid  sql.NullInt64
			parent sql.NullString
			fkid   sql.NullInt64
		)
		if scanErr := rows.Scan(&table, &rowid, &parent, &fkid); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("foreign_key_check scan: %w", scanErr)
		}
		violations = append(violations,
			fmt.Sprintf("table=%s rowid=%d parent=%s fkid=%d",
				table.String, rowid.Int64, parent.String, fkid.Int64))
	}
	if rerr := rows.Err(); rerr != nil {
		_ = rows.Close()
		return fmt.Errorf("foreign_key_check rows: %w", rerr)
	}
	_ = rows.Close()
	if len(violations) > 0 {
		return fmt.Errorf("foreign_key_check failed: %s", strings.Join(violations, "; "))
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
		m.Version, time.Now().Unix()); err != nil {
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
