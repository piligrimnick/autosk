package datasource_test

import (
	"context"
	"strings"
	"testing"

	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// seedHumanFeedbackTask creates a task and forces it into
// status='human_feedback' with a non-null current_step_id — the
// canonical "workflow kicked back to a human" shape that exercises
// the CHECK in 001_init.sql. Returns the task id.
func seedHumanFeedbackTask(t *testing.T, off *datasource.Offline, dl *doltlite.Store) string {
	t.Helper()
	ctx := context.Background()
	id, err := off.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`INSERT INTO agents(id, name, is_human, created_at) VALUES ('ag-1', 'tester', 0, 0)`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`INSERT INTO workflows(id, name, first_step_id, created_at) VALUES ('wf-1', 'tst', 'st-1', 0)`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-1', 'wf-1', 'first', 'ag-1', 0)`); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	wf, st := "wf-1", "st-1"
	hf := store.StatusHumanFeedback
	if _, err := dl.UpdateTask(ctx, id, store.TaskPatch{WorkflowID: &wf, CurrentStepID: &st, Status: &hf}); err != nil {
		t.Fatalf("seed human_feedback: %v", err)
	}
	return id
}

// TestOffline_UpdateStatus_DoneClearsCurrentStep is the regression for
// "lazy can't mark `as-4de8` done": a task in human_feedback with a
// non-null current_step_id used to trip the CHECK in 001_init.sql when
// lazy's UpdateStatus did a naive {Status: &StatusDone} patch. Since
// the refactor to internal/tasksvc the CLI and lazy take the same code
// path, so the terminal verb also clears current_step_id.
func TestOffline_UpdateStatus_DoneClearsCurrentStep(t *testing.T) {
	ctx := context.Background()
	off, dl, closeFn := newOfflineFx(t)
	defer closeFn()
	id := seedHumanFeedbackTask(t, off, dl)

	if err := off.UpdateStatus(ctx, id, store.StatusDone); err != nil {
		t.Fatalf("UpdateStatus(done) on human_feedback task: %v", err)
	}
	got, err := off.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("status: got %q want done", got.Status)
	}
	if got.CurrentStepID != "" {
		t.Fatalf("current_step_id should be cleared on done, got %q", got.CurrentStepID)
	}
}

// TestOffline_UpdateStatus_CancelClearsCurrentStep mirrors the Done
// regression for the cancel path.
func TestOffline_UpdateStatus_CancelClearsCurrentStep(t *testing.T) {
	ctx := context.Background()
	off, dl, closeFn := newOfflineFx(t)
	defer closeFn()
	id := seedHumanFeedbackTask(t, off, dl)

	if err := off.UpdateStatus(ctx, id, store.StatusCancelled); err != nil {
		t.Fatalf("UpdateStatus(cancelled): %v", err)
	}
	got, _ := off.GetTask(ctx, id)
	if got.Status != store.StatusCancelled {
		t.Fatalf("status: got %q want cancelled", got.Status)
	}
	if got.CurrentStepID != "" {
		t.Fatalf("current_step_id should be cleared on cancel, got %q", got.CurrentStepID)
	}
}

// TestOffline_UpdateStatus_ReopenPrecondition documents that lazy's
// `o` hotkey, like `autosk reopen`, refuses non-terminal sources.
// Without the tasksvc routing, lazy used to silently flip status=new
// on any task — losing workflow context with no error.
func TestOffline_UpdateStatus_ReopenPrecondition(t *testing.T) {
	ctx := context.Background()
	off, dl, closeFn := newOfflineFx(t)
	defer closeFn()
	id := seedHumanFeedbackTask(t, off, dl)

	err := off.UpdateStatus(ctx, id, store.StatusNew)
	if err == nil {
		t.Fatalf("reopen on human_feedback task should fail, got nil")
	}
	if !strings.Contains(err.Error(), "cannot reopen") {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := off.GetTask(ctx, id)
	if got.Status != store.StatusHumanFeedback {
		t.Fatalf("status mutated: %q", got.Status)
	}
}

// TestOffline_UpdateStatus_ReopenFromTerminal: the happy path. A done
// task can be reopened, current_step_id is cleared, workflow_id is
// preserved.
func TestOffline_UpdateStatus_ReopenFromTerminal(t *testing.T) {
	ctx := context.Background()
	off, dl, closeFn := newOfflineFx(t)
	defer closeFn()
	id := seedHumanFeedbackTask(t, off, dl)

	if err := off.UpdateStatus(ctx, id, store.StatusDone); err != nil {
		t.Fatalf("done: %v", err)
	}
	if err := off.UpdateStatus(ctx, id, store.StatusNew); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ := off.GetTask(ctx, id)
	if got.Status != store.StatusNew {
		t.Fatalf("status: got %q want new", got.Status)
	}
	if got.CurrentStepID != "" {
		t.Fatalf("current_step_id should remain cleared, got %q", got.CurrentStepID)
	}
	if got.WorkflowID == "" {
		t.Fatalf("workflow_id should be preserved on reopen (audit)")
	}
}

// TestOffline_UpdateStatus_RejectsInWorkflow: lazy must refuse to set
// status='in_workflow' (or to change status away from in_workflow via
// this path). Same rule the CLI's `update --status` enforces.
func TestOffline_UpdateStatus_RejectsInWorkflow(t *testing.T) {
	ctx := context.Background()
	off, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := off.CreateTask(ctx, "x", "", 2)
	if err := off.UpdateStatus(ctx, id, store.StatusInWorkflow); err == nil {
		t.Fatalf("setting status='in_workflow' should be rejected")
	}
}
