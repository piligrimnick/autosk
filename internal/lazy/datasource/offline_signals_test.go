package datasource_test

import (
	"context"
	"testing"
	"time"

	"autosk/internal/daemon/runstore"
)

// TestOffline_SignalsScopedByJobID is the regression test for the
// design plan §5.5 contract: the Inspector "Signals" tab must be
// scoped to ONE run (jobID), not the whole task. A kickback loop
// (multiple runs of the same task, each emitting a step_next signal)
// previously leaked all runs' signals into every tab.
//
// Setup: two runs of the same task, each with its own signal. Assert
// Signals(jobA) returns only A's signal and Signals(jobB) returns
// only B's. Back-compat: Signals(taskID) returns both.
func TestOffline_SignalsScopedByJobID(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	db := ts.DB()

	// Seed a workflow + step so the daemon_run + step_signal FKs pass.
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at) VALUES ('wf-1','t','','st-1',0,?)`, now); err != nil {
		t.Fatalf("seed wf: %v", err)
	}
	var humanID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agents WHERE name='human'`).Scan(&humanID); err != nil {
		t.Fatalf("read human: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-1','wf-1','do',?,0)`, humanID); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO step_transitions(step_id, task_status, prompt_rule) VALUES ('st-1', 'done', '')`); err != nil {
		t.Fatalf("seed transition: %v", err)
	}
	var transID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM step_transitions WHERE step_id='st-1'`).Scan(&transID); err != nil {
		t.Fatalf("read transition: %v", err)
	}

	// One task with two daemon_runs.
	taskID, err := ds.CreateTask(ctx, "kickback", "", 1)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	rs := runstore.New(db)
	r1, err := rs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: "st-1"})
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	r2, err := rs.CreateRun(ctx, runstore.NewRun{TaskID: taskID, StepID: "st-1"})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}

	// One signal each.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO step_signals(run_id, task_id, transition_id, created_at) VALUES (?,?,?,?)`,
		r1.JobID, taskID, transID, now); err != nil {
		t.Fatalf("signal1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO step_signals(run_id, task_id, transition_id, created_at) VALUES (?,?,?,?)`,
		r2.JobID, taskID, transID, now+1); err != nil {
		t.Fatalf("signal2: %v", err)
	}

	// Signals(jobID) → exactly one row, with run_id == jobID.
	sigs1, err := ds.Signals(ctx, r1.JobID)
	if err != nil {
		t.Fatalf("Signals(r1): %v", err)
	}
	if len(sigs1) != 1 {
		t.Fatalf("Signals(r1) returned %d rows, want 1", len(sigs1))
	}
	if sigs1[0].JobID != r1.JobID {
		t.Fatalf("Signals(r1)[0].JobID=%q want %q", sigs1[0].JobID, r1.JobID)
	}

	sigs2, err := ds.Signals(ctx, r2.JobID)
	if err != nil {
		t.Fatalf("Signals(r2): %v", err)
	}
	if len(sigs2) != 1 {
		t.Fatalf("Signals(r2) returned %d rows, want 1", len(sigs2))
	}
	if sigs2[0].JobID != r2.JobID {
		t.Fatalf("Signals(r2)[0].JobID=%q want %q", sigs2[0].JobID, r2.JobID)
	}

	// SignalsForTask is the dashboard's task-scoped lookup; it returns
	// every signal across all runs of the task.
	all, err := ds.SignalsForTask(ctx, taskID)
	if err != nil {
		t.Fatalf("SignalsForTask(taskID): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("SignalsForTask(taskID) returned %d, want 2 (jobs=%s,%s)",
			len(all), r1.JobID, r2.JobID)
	}

	// And calling Signals(taskID) returns nothing — the prefix-sniff
	// back-compat branch is gone; ss.run_id = '<taskID>' matches no rows.
	none, err := ds.Signals(ctx, taskID)
	if err != nil {
		t.Fatalf("Signals(taskID): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("Signals(taskID) returned %d rows, want 0 (task-scoped lookups now use SignalsForTask)", len(none))
	}
}
