package conformance

import (
	"context"
	"testing"

	"autosk/internal/store"
)

// testDeleteTaskRemovesRow verifies the basic happy path: a freshly
// created task can be deleted and a subsequent GetTask returns
// ErrNotFound. Used by the worktree-isolation rollback path in
// `autosk create` when allocating the worktree fails.
func testDeleteTaskRemovesRow(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()

	tk := mustCreate(t, s, "doomed", 2)
	if err := s.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := s.GetTask(ctx, tk.ID); err == nil {
		t.Fatalf("GetTask after delete should fail; got task")
	} else {
		AssertErrIs(t, err, store.ErrNotFound)
	}
}

// testDeleteTaskMissingReturnsNotFound asserts DeleteTask returns the
// store.ErrNotFound sentinel for unknown ids -- not a generic failure
// -- so callers can branch on it.
func testDeleteTaskMissingReturnsNotFound(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	err := s.DeleteTask(context.Background(), "ask-ffffff")
	AssertErrIs(t, err, store.ErrNotFound)
}

// testDeleteTaskCascadesDepsAndComments asserts the FK CASCADE
// declared across 001_init.sql + 002_daemon_runs.sql actually fires
// for every dependent row deleting a task transitively reaches:
//
//   - task_deps   (as both blocker and blocked) — 001
//   - comments    — 001
//   - daemon_runs — 002 (cascades from tasks)
//   - step_signals — 002 (cascades transitively through daemon_runs)
//
// This is the contract the create rollback path leans on -- a future
// schema change that drops any link in the chain trips this
// immediately.
func testDeleteTaskCascadesDepsAndComments(t *testing.T, f Factory) {
	s, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()

	a := mustCreate(t, s, "a", 2)
	b := mustCreate(t, s, "b", 2)
	c := mustCreate(t, s, "c", 2)
	// a blocks b; c blocks a. Deleting a should remove both edges.
	if err := s.Block(ctx, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Block(ctx, a.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	// Add a comment authored by the seeded human agent (the only
	// guaranteed agent id at conformance-suite time).
	aID := humanAgentID(t, s)
	if err := addCommentRaw(ctx, s, a.ID, aID, "doomed"); err != nil {
		t.Fatalf("add comment: %v", err)
	}
	// Wire up the daemon_runs / step_signals chain. This needs a
	// workflow + step + transition triple to satisfy the FKs.
	stepID, transID := seedRunGraph(t, s, aID)
	jobID := "job-delete-cascade"
	if err := addDaemonRunRaw(ctx, s, jobID, a.ID, stepID); err != nil {
		t.Fatalf("add daemon_runs: %v", err)
	}
	if err := addStepSignalRaw(ctx, s, jobID, a.ID, transID); err != nil {
		t.Fatalf("add step_signals: %v", err)
	}

	// Pre-delete sanity checks.
	if n := countRowsWhere(t, s, "task_deps", "blocker_id = ? OR blocked_id = ?", a.ID, a.ID); n != 2 {
		t.Fatalf("pre-delete task_deps count for %s: %d (want 2)", a.ID, n)
	}
	if n := countRowsWhere(t, s, "comments", "task_id = ?", a.ID); n != 1 {
		t.Fatalf("pre-delete comments count for %s: %d (want 1)", a.ID, n)
	}
	if n := countRowsWhere(t, s, "daemon_runs", "task_id = ?", a.ID); n != 1 {
		t.Fatalf("pre-delete daemon_runs count for %s: %d (want 1)", a.ID, n)
	}
	if n := countRowsWhere(t, s, "step_signals", "task_id = ?", a.ID); n != 1 {
		t.Fatalf("pre-delete step_signals count for %s: %d (want 1)", a.ID, n)
	}

	if err := s.DeleteTask(ctx, a.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	// CASCADE assertions: every dependent row referencing a should be gone.
	if n := countRowsWhere(t, s, "task_deps", "blocker_id = ? OR blocked_id = ?", a.ID, a.ID); n != 0 {
		t.Fatalf("post-delete task_deps count for %s: %d (want 0 -- FK CASCADE broken)", a.ID, n)
	}
	if n := countRowsWhere(t, s, "comments", "task_id = ?", a.ID); n != 0 {
		t.Fatalf("post-delete comments count for %s: %d (want 0 -- FK CASCADE broken)", a.ID, n)
	}
	if n := countRowsWhere(t, s, "daemon_runs", "task_id = ?", a.ID); n != 0 {
		t.Fatalf("post-delete daemon_runs count for %s: %d (want 0 -- FK CASCADE broken)", a.ID, n)
	}
	if n := countRowsWhere(t, s, "step_signals", "task_id = ?", a.ID); n != 0 {
		t.Fatalf("post-delete step_signals count for %s: %d (want 0 -- step_signals transitive CASCADE through daemon_runs broken)", a.ID, n)
	}
	// The other tasks survive untouched.
	if _, err := s.GetTask(ctx, b.ID); err != nil {
		t.Fatalf("b should survive: %v", err)
	}
	if _, err := s.GetTask(ctx, c.ID); err != nil {
		t.Fatalf("c should survive: %v", err)
	}
}

// seedRunGraph wires a minimal workflow + step + transition triple so
// daemon_runs / step_signals rows can be inserted by the cascade
// test. Returns (stepID, transitionID). The agentID parameter is the
// pre-seeded `human` agent's id (the FK target for steps.agent_id).
func seedRunGraph(t *testing.T, s store.Store, agentID string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	wfID := "wf-cascade-test"
	stepID := "st-cascade-test"
	if _, err := s.ExecRaw(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES (?, ?, '', ?, 0, strftime('%s','now'))`,
		wfID, "wf-cascade-test", stepID); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	if _, err := s.ExecRaw(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES (?, ?, ?, ?, 0)`,
		stepID, wfID, "do", agentID); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	res, err := s.ExecRaw(ctx,
		`INSERT INTO step_transitions(step_id, next_step_id, task_status, prompt_rule)
		 VALUES (?, NULL, 'done', 'cascade test')`, stepID)
	if err != nil {
		t.Fatalf("insert step_transitions: %v", err)
	}
	transID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return stepID, transID
}

// addDaemonRunRaw inserts a daemon_runs row with status='queued' so
// the CASCADE-on-tasks chain can be exercised by the conformance test.
func addDaemonRunRaw(ctx context.Context, s store.Store, jobID, taskID, stepID string) error {
	_, err := s.ExecRaw(ctx,
		`INSERT INTO daemon_runs(job_id, task_id, step_id, status, created_at)
		 VALUES (?, ?, ?, 'queued', strftime('%s','now'))`,
		jobID, taskID, stepID)
	return err
}

// addStepSignalRaw inserts a step_signals row referencing the
// daemon_runs row so the transitive CASCADE
// tasks -> daemon_runs -> step_signals can be asserted.
func addStepSignalRaw(ctx context.Context, s store.Store, runID, taskID string, transID int64) error {
	_, err := s.ExecRaw(ctx,
		`INSERT INTO step_signals(run_id, task_id, transition_id, created_at)
		 VALUES (?, ?, ?, strftime('%s','now'))`,
		runID, taskID, transID)
	return err
}

// humanAgentID returns the seeded human agent's id via QueryRaw. The
// conformance suite is backend-agnostic, so we can't reach into the
// agent.Store directly.
func humanAgentID(t *testing.T, s store.Store) string {
	t.Helper()
	rows, err := s.QueryRaw(context.Background(), `SELECT id FROM agents WHERE name='human'`)
	if err != nil {
		t.Fatalf("query human agent id: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no seeded human agent row")
	}
	var id string
	if err := rows.Scan(&id); err != nil {
		t.Fatalf("scan agent id: %v", err)
	}
	return id
}

// addCommentRaw inserts a comments row directly via ExecRaw so the
// suite doesn't depend on the comments package (which is layered above
// the store).
func addCommentRaw(ctx context.Context, s store.Store, taskID, authorID, text string) error {
	_, err := s.ExecRaw(ctx,
		`INSERT INTO comments(task_id, author_id, text, created_at) VALUES (?, ?, ?, strftime('%s','now'))`,
		taskID, authorID, text)
	return err
}

// countRowsWhere returns the count of rows in `table` matching the
// supplied WHERE clause + args. Used to assert FK CASCADE behaviour
// in a backend-agnostic way.
func countRowsWhere(t *testing.T, s store.Store, table, where string, args ...any) int {
	t.Helper()
	q := "SELECT COUNT(*) FROM " + table + " WHERE " + where
	rows, err := s.QueryRaw(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row from COUNT(*) query")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	return n
}
