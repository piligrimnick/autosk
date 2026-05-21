package step_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/daemon/runstore"
	"autosk/internal/step"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

type sigFixture struct {
	ts     *doltlite.Store
	wfs    *workflow.Store
	runs   *runstore.Store
	sigs   *step.Store
	wf     workflow.Workflow
	taskID string
	runID  string
	close  func()
}

func newSigFixture(t *testing.T) *sigFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	ag := agent.New(ts.DB())
	for _, name := range []string{"developer", "code-reviewer", "task-validator"} {
		if _, err := ag.Create(ctx, name, false); err != nil {
			_ = ts.Close()
			t.Fatalf("agent: %v", err)
		}
	}
	wfs := workflow.New(ts.DB(), ag)
	def, err := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if err != nil {
		_ = ts.Close()
		t.Fatalf("ParseFile: %v", err)
	}
	wf, err := wfs.Create(ctx, def, false)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("workflow Create: %v", err)
	}
	tk, err := ts.CreateTask(ctx, store.Task{
		Title:         "x",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: wf.FirstStepID,
	})
	if err != nil {
		_ = ts.Close()
		t.Fatalf("task: %v", err)
	}
	rs := runstore.New(ts.DB())
	r, err := rs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: wf.FirstStepID})
	if err != nil {
		_ = ts.Close()
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := rs.MarkRunning(ctx, r.JobID, 0); err != nil {
		_ = ts.Close()
		t.Fatalf("MarkRunning: %v", err)
	}
	return &sigFixture{
		ts:     ts,
		wfs:    wfs,
		runs:   rs,
		sigs:   step.New(ts.DB()),
		wf:     wf,
		taskID: tk.ID,
		runID:  r.JobID,
		close:  func() { _ = ts.Close() },
	}
}

func TestEmit_SiblingStep(t *testing.T) {
	fx := newSigFixture(t)
	defer fx.close()
	e, err := fx.sigs.Emit(context.Background(), fx.taskID, "review")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if e.RunID != fx.runID {
		t.Errorf("run_id: %q", e.RunID)
	}
	if e.NextStepName != "review" {
		t.Errorf("next_step_name: %q", e.NextStepName)
	}
	if e.TaskStatus != "" {
		t.Errorf("task_status: %q (expected empty)", e.TaskStatus)
	}
	// Verify GetForRun returns the same row.
	got, err := fx.sigs.GetForRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("GetForRun: %v", err)
	}
	if got.TransitionID != e.TransitionID {
		t.Errorf("round-trip transition_id mismatch")
	}
}

func TestEmit_TaskStatusFromValidator(t *testing.T) {
	fx := newSigFixture(t)
	defer fx.close()
	// Move the task's current step to "validator" (which has task_status
	// human) so we can exercise the task-status path.
	validatorStep, err := fx.wfs.FindStepByName(context.Background(), fx.wf.ID, "validator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.ts.UpdateTask(context.Background(), fx.taskID, store.TaskPatch{
		CurrentStepID: &validatorStep.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// Re-create a run row pointing at the new step.
	r2, err := fx.runs.CreateRun(context.Background(), runstore.NewRun{
		TaskID: fx.taskID, StepID: validatorStep.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mark the old run done so Emit picks the new one as "active".
	_, _ = fx.runs.MarkDone(context.Background(), fx.runID, 0, nil)
	if _, err := fx.runs.MarkRunning(context.Background(), r2.JobID, 0); err != nil {
		t.Fatal(err)
	}

	e, err := fx.sigs.Emit(context.Background(), fx.taskID, "human")
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if e.TaskStatus != "human" {
		t.Errorf("task_status: %q", e.TaskStatus)
	}
	if e.NextStepName != "" {
		t.Errorf("next_step_name should be empty for task_status, got %q", e.NextStepName)
	}
}

func TestEmit_RejectsUnknownTarget(t *testing.T) {
	fx := newSigFixture(t)
	defer fx.close()
	_, err := fx.sigs.Emit(context.Background(), fx.taskID, "totally_unknown")
	if !errors.Is(err, step.ErrUnknownTarget) {
		t.Fatalf("want ErrUnknownTarget, got %v", err)
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("error should list valid targets, got %v", err)
	}
}

func TestEmit_DoubleCall_Rejected(t *testing.T) {
	fx := newSigFixture(t)
	defer fx.close()
	if _, err := fx.sigs.Emit(context.Background(), fx.taskID, "review"); err != nil {
		t.Fatal(err)
	}
	_, err := fx.sigs.Emit(context.Background(), fx.taskID, "review")
	if !errors.Is(err, step.ErrAlreadyEmitted) {
		t.Fatalf("want ErrAlreadyEmitted, got %v", err)
	}
}

func TestEmit_NoActiveRun(t *testing.T) {
	fx := newSigFixture(t)
	defer fx.close()
	// Move the only running row to terminal.
	if _, err := fx.runs.MarkDone(context.Background(), fx.runID, 0, nil); err != nil {
		t.Fatal(err)
	}
	_, err := fx.sigs.Emit(context.Background(), fx.taskID, "review")
	if !errors.Is(err, step.ErrNoActiveRun) {
		t.Fatalf("want ErrNoActiveRun, got %v", err)
	}
}
