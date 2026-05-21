package migrations_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // libsqlite3-backed driver (Makefile sets the tag)

	"autosk/internal/migrations"
)

// TestApply006_ShortStatuses_RewritesAndPreservesDeps replays migrations
// 001..005 on a fresh DB, seeds rows that carry every legacy status
// spelling plus dependent rows (task_deps, comments, daemon_runs,
// step_signals), then runs migration 006 and asserts:
//
//   - every legacy status value (in_workflow, human_feedback, cancelled)
//     is rewritten to the new short form,
//   - the new CHECK constraints reject the legacy spellings,
//   - PRAGMA foreign_key_check is clean post-migration,
//   - dependent rows (CASCADE FKs) survive the table rebuild -- the FK-off
//     directive in 006 is the whole point of this test.
func TestApply006_ShortStatuses_RewritesAndPreservesDeps(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "schema.db")
	db := openDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })

	// Apply 001-005 only (not 006); we'll seed in the old vocabulary
	// and then apply 006 to verify the rewrite. The fixture must
	// reflect the v0.2 (pre-006) shape, so we run every migration
	// that ships with version < 6 by temporarily walking the embed
	// list.
	applyUpTo(t, ctx, db, 5)

	seedPreV6Fixture(t, ctx, db)

	// Apply 006 (and any later migrations).
	if _, err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("Apply(006): %v", err)
	}

	// Schema must reject the legacy spellings post-migration.
	for _, q := range []string{
		`UPDATE tasks SET status='in_workflow' WHERE id='t-work'`,
		`UPDATE tasks SET status='human_feedback' WHERE id='t-human'`,
		`UPDATE tasks SET status='cancelled' WHERE id='t-cancel'`,
		`INSERT INTO step_transitions(step_id, task_status, prompt_rule) VALUES('s-a','human_feedback','.')`,
		`INSERT INTO daemon_runs(job_id, task_id, step_id, status, created_at) VALUES('job-x','t-work','s-a','cancelled', 1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err == nil {
			t.Errorf("legacy spelling accepted by CHECK: %s", q)
		}
	}

	// Status rewrites: every persisted column must hold only new values.
	gotTaskStatuses := scanDistinct(t, db, `SELECT DISTINCT status FROM tasks`)
	wantTask := []string{"cancel", "done", "human", "new", "work"}
	if !equalSorted(gotTaskStatuses, wantTask) {
		t.Errorf("tasks.status post-migration: got %v want %v", gotTaskStatuses, wantTask)
	}
	gotStepStatuses := scanDistinct(t, db,
		`SELECT DISTINCT task_status FROM step_transitions WHERE task_status IS NOT NULL`)
	wantStep := []string{"cancel", "done", "human"}
	if !equalSorted(gotStepStatuses, wantStep) {
		t.Errorf("step_transitions.task_status post-migration: got %v want %v",
			gotStepStatuses, wantStep)
	}
	gotRunStatuses := scanDistinct(t, db, `SELECT DISTINCT status FROM daemon_runs`)
	wantRun := []string{"cancel", "queued"} // we seeded one of each
	if !equalSorted(gotRunStatuses, wantRun) {
		t.Errorf("daemon_runs.status post-migration: got %v want %v",
			gotRunStatuses, wantRun)
	}

	// Dependent rows survived the table rebuild (FK-off is the load-
	// bearing detail).
	mustCount(t, db, `SELECT COUNT(*) FROM task_deps`, 1)
	mustCount(t, db, `SELECT COUNT(*) FROM comments`, 1)
	mustCount(t, db, `SELECT COUNT(*) FROM daemon_runs`, 2)
	mustCount(t, db, `SELECT COUNT(*) FROM step_signals`, 1)

	// PRAGMA foreign_key_check must be empty.
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a row post-migration")
	}

	// schema_migrations advanced to >= 6.
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("SELECT max(version): %v", err)
	}
	if !v.Valid || v.Int64 < 6 {
		t.Fatalf("schema_migrations max(version)=%v, want >= 6", v.Int64)
	}
}

// TestApply007_TaskIDFormat_RewritesAndPreservesDeps replays migrations
// 001..006 on a fresh DB, seeds legacy `as-XXXX` task ids plus dependent
// rows across every FK that references tasks.id, then runs migration
// 007 and asserts:
//
//   - every legacy task id is rewritten to the `ask-00XXXX` shape,
//   - every dependent row (task_deps, comments, daemon_runs,
//     step_signals) carries the rewritten id and still joins to the
//     migrated task row,
//   - PRAGMA foreign_key_check is clean post-migration,
//   - no row still carries the legacy `as-` prefix.
//
// The FK-off directive in 007 is the load-bearing detail: with FKs on,
// updating tasks.id without ON UPDATE CASCADE would orphan every
// dependent row. This test pins that contract.
func TestApply007_TaskIDFormat_RewritesAndPreservesDeps(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "schema.db")
	db := openDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })

	applyUpTo(t, ctx, db, 6)
	seedPreV7Fixture(t, ctx, db)

	if _, err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("Apply(007): %v", err)
	}

	// No `as-` task ids survive.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE id LIKE 'as-%'`).Scan(&n); err != nil {
		t.Fatalf("count as- tasks: %v", err)
	}
	if n != 0 {
		t.Errorf("tasks.id still carries %d as- rows post-007", n)
	}

	// Every legacy task is now ask-00<old-4hex>.
	for _, want := range []string{"ask-00a1b2", "ask-00c3d4", "ask-00ffff"} {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT id FROM tasks WHERE id = ?`, want).Scan(&got); err != nil {
			t.Errorf("missing migrated task %q: %v", want, err)
		}
	}

	// Every dependent column carries the rewritten id. Cross-check the
	// counts to make sure the SQL UPDATEs found every row and didn't
	// drop any (FK enforcement was off, so a silent CASCADE wipe would
	// have been catastrophic).
	mustCount(t, db, `SELECT COUNT(*) FROM tasks      WHERE id LIKE 'ask-00%'`, 3)
	mustCount(t, db, `SELECT COUNT(*) FROM task_deps  WHERE blocker_id LIKE 'ask-00%' OR blocked_id LIKE 'ask-00%'`, 1)
	mustCount(t, db, `SELECT COUNT(*) FROM comments   WHERE task_id LIKE 'ask-00%'`, 1)
	mustCount(t, db, `SELECT COUNT(*) FROM daemon_runs WHERE task_id LIKE 'ask-00%'`, 1)
	mustCount(t, db, `SELECT COUNT(*) FROM step_signals WHERE task_id LIKE 'ask-00%'`, 1)

	// And no dependent column still carries an `as-` prefix.
	for _, q := range []string{
		`SELECT COUNT(*) FROM task_deps   WHERE blocker_id LIKE 'as-%' OR blocked_id LIKE 'as-%'`,
		`SELECT COUNT(*) FROM comments    WHERE task_id    LIKE 'as-%'`,
		`SELECT COUNT(*) FROM daemon_runs WHERE task_id    LIKE 'as-%'`,
		`SELECT COUNT(*) FROM step_signals WHERE task_id   LIKE 'as-%'`,
	} {
		mustCount(t, db, q, 0)
	}

	// FK invariant: dependent rows actually join to a task row.
	mustCount(t, db,
		`SELECT COUNT(*) FROM comments c JOIN tasks t ON c.task_id = t.id`, 1)
	mustCount(t, db,
		`SELECT COUNT(*) FROM daemon_runs dr JOIN tasks t ON dr.task_id = t.id`, 1)
	mustCount(t, db,
		`SELECT COUNT(*) FROM step_signals ss JOIN tasks t ON ss.task_id = t.id`, 1)

	// PRAGMA foreign_key_check must be empty.
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent sql.NullString
		var rowid, fkid sql.NullInt64
		_ = rows.Scan(&table, &rowid, &parent, &fkid)
		t.Fatalf("foreign_key_check returned a row post-007: table=%s rowid=%d parent=%s fkid=%d",
			table.String, rowid.Int64, parent.String, fkid.Int64)
	}

	// schema_migrations advanced to >= 7.
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("SELECT max(version): %v", err)
	}
	if !v.Valid || v.Int64 < 7 {
		t.Fatalf("schema_migrations max(version)=%v, want >= 7", v.Int64)
	}
}

// TestApply007_IdempotentOnFreshDB pins acceptance criterion #2 from
// the task description: a brand-new database (no `as-` rows to
// rewrite) must apply migration 007 cleanly. This guards against a
// future tweak that, for instance, expects at least one matching row.
func TestApply007_IdempotentOnFreshDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db := openDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := migrations.Apply(ctx, db); err != nil {
		t.Fatalf("Apply on empty DB: %v", err)
	}
	// No tasks at all, and certainly no `as-` rows.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE id LIKE 'as-%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("empty DB grew %d as- rows after Apply", n)
	}
	// And the schema is at the latest version (>= 7).
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("select max(version): %v", err)
	}
	if !v.Valid || v.Int64 < 7 {
		t.Fatalf("max(version)=%v, want >= 7", v.Int64)
	}
}

// seedPreV7Fixture inserts a small graph of `as-XXXX` task ids plus
// dependent rows on every FK pointed at tasks.id. Schema is the
// post-006 shape (status enum already short-form). The migration
// rewrites every id to `ask-00XXXX` and the asserts in the test
// verify the rewrite reached every row.
func seedPreV7Fixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", firstLineOfQuery(q), err)
		}
	}

	exec(`INSERT INTO agents(id, name, is_human, created_at) VALUES('ag-d','dev',0,0)`)
	exec(`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
	       VALUES('wf-a','feature-x','','s-a',0,0)`)
	exec(`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES('s-a','wf-a','dev','ag-d',0)`)
	exec(`INSERT INTO step_transitions(step_id, task_status, prompt_rule) VALUES('s-a','done','r')`)

	// Three legacy tasks: one work (with current_step), one new, one done.
	exec(`INSERT INTO tasks(id, title, status, priority, workflow_id, current_step_id, created_at, updated_at)
	       VALUES('as-a1b2','w','work',2,'wf-a','s-a',1,1)`)
	exec(`INSERT INTO tasks(id, title, status, priority, created_at, updated_at)
	       VALUES('as-c3d4','n','new',2,1,1)`)
	exec(`INSERT INTO tasks(id, title, status, priority, created_at, updated_at)
	       VALUES('as-ffff','d','done',2,1,1)`)

	// One row per dependent table, all referencing as-a1b2 (the work task).
	exec(`INSERT INTO task_deps(blocker_id, blocked_id, kind) VALUES('as-c3d4','as-a1b2','blocks')`)
	exec(`INSERT INTO comments(task_id, author_id, text, created_at)
	       VALUES('as-a1b2','ag-d','hi',1)`)
	exec(`INSERT INTO daemon_runs(job_id, task_id, step_id, status, created_at)
	       VALUES('job-q','as-a1b2','s-a','queued',1)`)
	exec(`INSERT INTO step_signals(run_id, task_id, transition_id, created_at)
	       VALUES('job-q','as-a1b2',1,1)`)
}

// openDB opens a libsqlite3-backed sqlite3 db. We bypass doltlite here
// because we need foreign-key enforcement to be enabled by default (006
// disables it for the duration of the migration); the doltlite store
// would otherwise wrap the connection with single-writer machinery
// irrelevant to the migration unit test.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	return db
}

// applyUpTo applies every migration with version <= upTo via the public
// migrations.ApplyWith entry point. We go through the canonical runner
// (not a re-implemented loop) so future hooks on applyOne (logging,
// dispatch points, schema_migrations columns) stay covered by this test
// without anyone needing to edit it.
func applyUpTo(t *testing.T, ctx context.Context, db *sql.DB, upTo int) {
	t.Helper()
	if _, err := migrations.ApplyWith(ctx, db, migrations.ApplyOptions{Until: upTo}); err != nil {
		t.Fatalf("ApplyWith(Until=%d): %v", upTo, err)
	}
}

// seedPreV6Fixture inserts one row per pre-006 status spelling plus
// dependent rows referencing those tasks via CASCADE FKs. The CHECK
// constraints in pre-006 schema accept these values; the migration
// rewrites them.
func seedPreV6Fixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", firstLineOfQuery(q), err)
		}
	}
	// Workflow + step so we can park a task on a real step pointer
	// and seed step_transitions / daemon_runs / step_signals.
	exec(`INSERT INTO agents(id, name, is_human, created_at) VALUES('ag-d','dev',0,0)`)
	exec(`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		   VALUES('wf-a','feature-x','',  's-a', 0, 0)`)
	exec(`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES('s-a','wf-a','dev','ag-d',0)`)
	// One transition in each legacy task_status form so the rebuild
	// has rows to rewrite, plus a sibling-step transition that
	// crosses the rebuild unchanged.
	exec(`INSERT INTO step_transitions(step_id, task_status, prompt_rule) VALUES('s-a','human_feedback','rule-h')`)
	exec(`INSERT INTO step_transitions(step_id, task_status, prompt_rule) VALUES('s-a','done','rule-d')`)
	exec(`INSERT INTO step_transitions(step_id, task_status, prompt_rule) VALUES('s-a','cancelled','rule-c')`)
	// Sibling-step transition; task_status is NULL.
	exec(`INSERT INTO step_transitions(step_id, next_step_id, prompt_rule) VALUES('s-a','s-a','rule-self')`)

	// One task per legacy status. in_workflow needs a current_step_id
	// (CHECK invariant); the others must NOT have one (also CHECK).
	exec(`INSERT INTO tasks(id, title, status, priority, workflow_id, current_step_id, created_at, updated_at)
		   VALUES('t-work','w','in_workflow',  2,'wf-a','s-a',1,1)`)
	exec(`INSERT INTO tasks(id, title, status, priority, workflow_id, current_step_id, created_at, updated_at)
		   VALUES('t-human','h','human_feedback',2,'wf-a','s-a',1,1)`)
	exec(`INSERT INTO tasks(id, title, status, priority, created_at, updated_at)
		   VALUES('t-cancel','c','cancelled',2,1,1)`)
	exec(`INSERT INTO tasks(id, title, status, priority, created_at, updated_at)
		   VALUES('t-new','n','new',2,1,1)`)
	exec(`INSERT INTO tasks(id, title, status, priority, created_at, updated_at)
		   VALUES('t-done','d','done',2,1,1)`)

	// Cross-references that the FK-off rebuild must NOT cascade-wipe.
	exec(`INSERT INTO task_deps(blocker_id, blocked_id, kind) VALUES('t-new','t-work','blocks')`)
	exec(`INSERT INTO comments(task_id, author_id, text, created_at) VALUES('t-work','ag-d','hi',1)`)
	// One queued daemon_runs row plus one cancelled one so the rewrite
	// has work to do.
	exec(`INSERT INTO daemon_runs(job_id, task_id, step_id, status, created_at)
		   VALUES('job-q','t-work','s-a','queued',1)`)
	exec(`INSERT INTO daemon_runs(job_id, task_id, step_id, status, created_at)
		   VALUES('job-c','t-cancel','s-a','cancelled',1)`)
	// step_signals references daemon_runs(job_id) ON DELETE CASCADE --
	// the FK-off recipe is the whole reason this row must survive.
	exec(`INSERT INTO step_signals(run_id, task_id, transition_id, created_at)
		   VALUES('job-q','t-work',2,1)`)
}

// --- tiny helpers ---------------------------------------------------------

func scanDistinct(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

func mustCount(t *testing.T, db *sql.DB, q string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	if n != want {
		t.Errorf("%s: got %d, want %d", q, n, want)
	}
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstLineOfQuery returns the first non-empty line of a SQL string for
// error-message context. We only need it for the fixture-seed helper;
// the migration runner is no longer reimplemented here.
func firstLineOfQuery(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(ln); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
