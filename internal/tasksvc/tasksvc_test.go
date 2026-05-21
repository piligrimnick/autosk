package tasksvc_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/tasksvc"
)

func newFx(t *testing.T) (*doltlite.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s := doltlite.New()
	if err := s.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		t.Fatalf("migrate: %v", err)
	}
	return s, func() { _ = s.Close() }
}

// seedHumanFeedback creates a task and forces it into the
// "paused-mid-workflow" shape: status=human_feedback with a
// non-null current_step_id. The shape that used to trip the
// CHECK in 001_init.sql when the terminal verb didn't clear
// current_step_id.
func seedHumanFeedback(t *testing.T, s *doltlite.Store) string {
	t.Helper()
	ctx := context.Background()
	tk, err := s.CreateTask(ctx, store.Task{Title: "x", Priority: 2, Status: store.StatusNew})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO agents(id, name, is_human, created_at) VALUES ('ag-1', 'tester', 0, 0)`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO workflows(id, name, first_step_id, created_at) VALUES ('wf-1', 'tst', 'st-1', 0)`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES ('st-1', 'wf-1', 'first', 'ag-1', 0)`); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	wf, st := "wf-1", "st-1"
	hf := store.StatusHumanFeedback
	if _, err := s.UpdateTask(ctx, tk.ID, store.TaskPatch{WorkflowID: &wf, CurrentStepID: &st, Status: &hf}); err != nil {
		t.Fatalf("seed human_feedback: %v", err)
	}
	return tk.ID
}

func TestDone_ClearsCurrentStepOnHumanFeedback(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()
	id := seedHumanFeedback(t, s)

	got, err := tasksvc.Done(ctx, s, id, tasksvc.Options{})
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("status: got %q want done", got.Status)
	}
	if got.CurrentStepID != "" {
		t.Fatalf("current_step_id should be cleared, got %q", got.CurrentStepID)
	}
}

func TestCancel_ClearsCurrentStepOnHumanFeedback(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()
	id := seedHumanFeedback(t, s)

	got, err := tasksvc.Cancel(ctx, s, id, tasksvc.Options{})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.Status != store.StatusCancelled {
		t.Fatalf("status: got %q want cancelled", got.Status)
	}
	if got.CurrentStepID != "" {
		t.Fatalf("current_step_id should be cleared, got %q", got.CurrentStepID)
	}
}

func TestReopen_RejectsNonTerminal(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()
	id := seedHumanFeedback(t, s)

	_, err := tasksvc.Reopen(ctx, s, id)
	if err == nil {
		t.Fatalf("reopen on human_feedback should fail")
	}
	if !strings.Contains(err.Error(), "cannot reopen") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReopen_FromDone(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()
	id := seedHumanFeedback(t, s)
	if _, err := tasksvc.Done(ctx, s, id, tasksvc.Options{}); err != nil {
		t.Fatalf("done: %v", err)
	}

	got, err := tasksvc.Reopen(ctx, s, id)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got.Status != store.StatusNew {
		t.Fatalf("status: got %q want new", got.Status)
	}
	if got.WorkflowID == "" {
		t.Fatalf("workflow_id should be preserved on reopen for audit")
	}
}

func TestSetStatus_RejectsInWorkflowTarget(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()
	tk, _ := s.CreateTask(ctx, store.Task{Title: "x", Priority: 2, Status: store.StatusNew})

	_, err := tasksvc.SetStatus(ctx, s, tk.ID, store.StatusInWorkflow, tasksvc.Options{})
	if err == nil {
		t.Fatalf("setting status='in_workflow' should be rejected")
	}
}

func TestSetStatus_RoutesToDone(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()
	id := seedHumanFeedback(t, s)

	got, err := tasksvc.SetStatus(ctx, s, id, store.StatusDone, tasksvc.Options{})
	if err != nil {
		t.Fatalf("SetStatus(done): %v", err)
	}
	if got.Status != store.StatusDone || got.CurrentStepID != "" {
		t.Fatalf("Done path not taken: %+v", got)
	}
}

func TestDone_NotFound(t *testing.T) {
	ctx := context.Background()
	s, closeFn := newFx(t)
	defer closeFn()

	_, err := tasksvc.Done(ctx, s, "as-doesnotexist", tasksvc.Options{})
	if err == nil {
		t.Fatalf("Done on missing id should fail")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}
