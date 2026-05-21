package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// fixtureWF spins up a doltlite store with a feature-dev workflow.
type fixtureWF struct {
	s     *doltlite.Store
	ag    *agent.Store
	wfs   *workflow.Store
	wf    workflow.Workflow
	close func()
}

func newFixtureWF(t *testing.T) *fixtureWF {
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
			t.Fatalf("agent create %s: %v", name, err)
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
	return &fixtureWF{s: ts, ag: ag, wfs: wfs, wf: wf, close: func() { _ = ts.Close() }}
}

func TestCreateTask_WorkflowEnforcement(t *testing.T) {
	fx := newFixtureWF(t)
	defer fx.close()
	ctx := context.Background()

	// status=in_workflow without current_step_id ⇒ validation fails.
	_, err := fx.s.CreateTask(ctx, store.Task{
		Title:    "bad",
		Status:   store.StatusInWorkflow,
		Priority: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "in_workflow requires") {
		t.Fatalf("want in_workflow validation error, got %v", err)
	}

	// status=new with current_step_id set ⇒ validation fails.
	stepID := fx.wf.FirstStepID
	_, err = fx.s.CreateTask(ctx, store.Task{
		Title:         "bad",
		Status:        store.StatusNew,
		Priority:      2,
		CurrentStepID: stepID,
	})
	if err == nil || !strings.Contains(err.Error(), "must have current_step_id cleared") {
		t.Fatalf("want new-without-step error, got %v", err)
	}

	// Happy path: in_workflow + step + workflow.
	t1, err := fx.s.CreateTask(ctx, store.Task{
		Title:         "good",
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    fx.wf.ID,
		CurrentStepID: stepID,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if t1.Status != store.StatusInWorkflow || t1.WorkflowID != fx.wf.ID || t1.CurrentStepID != stepID {
		t.Fatalf("round-trip: %+v", t1)
	}
}

func TestUpdate_RejectsStatusOnInWorkflow(t *testing.T) {
	fx := newFixtureWF(t)
	defer fx.close()
	ctx := context.Background()
	t1 := mustCreateInWorkflow(t, fx, "X")

	// Attempting to flip status via UpdateTask + CHECK constraint fails
	// because we don't clear current_step_id. (The CLI layer also rejects
	// this; here we exercise the store-level guardrails.)
	done := store.StatusDone
	_, err := fx.s.UpdateTask(ctx, t1.ID, store.TaskPatch{Status: &done})
	if err == nil {
		t.Fatal("expected CHECK constraint failure")
	}
}

func TestReopen_ClearsStep_PreservesWorkflowID(t *testing.T) {
	fx := newFixtureWF(t)
	defer fx.close()
	ctx := context.Background()
	t1 := mustCreateInWorkflow(t, fx, "X")

	// Manually take the task to done, clearing step (the verb does this).
	done := store.StatusDone
	empty := ""
	if _, err := fx.s.UpdateTask(ctx, t1.ID, store.TaskPatch{
		Status:        &done,
		CurrentStepID: &empty,
	}); err != nil {
		t.Fatal(err)
	}

	// Reopen flow (mirrors the CLI verb): status=new + clear step.
	newSt := store.StatusNew
	out, err := fx.s.UpdateTask(ctx, t1.ID, store.TaskPatch{
		Status:        &newSt,
		CurrentStepID: &empty,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != store.StatusNew {
		t.Fatalf("status: %s", out.Status)
	}
	if out.CurrentStepID != "" {
		t.Fatalf("current_step_id should be cleared, got %q", out.CurrentStepID)
	}
	if out.WorkflowID != fx.wf.ID {
		t.Fatalf("workflow_id should be preserved for audit, got %q", out.WorkflowID)
	}
}

func TestSyntheticWorkflow_CreatedOnDemand(t *testing.T) {
	fx := newFixtureWF(t)
	defer fx.close()
	ctx := context.Background()

	// First call mints single:developer.
	w1, err := fx.wfs.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if !w1.IsSynthetic || w1.Name != "single:developer" {
		t.Fatalf("synthetic: %+v", w1)
	}
	if len(w1.Steps) != 1 || len(w1.Steps[0].Transitions) != 3 {
		t.Fatalf("shape: steps=%d", len(w1.Steps))
	}

	// Now create a task --agent developer (simulated via store ops).
	t1, err := fx.s.CreateTask(ctx, store.Task{
		Title:         "go",
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    w1.ID,
		CurrentStepID: w1.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fx.s.GetTask(ctx, t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkflowID != w1.ID || got.CurrentStepID != w1.Steps[0].ID {
		t.Fatalf("got: %+v", got)
	}
}

func mustCreateInWorkflow(t *testing.T, fx *fixtureWF, title string) store.Task {
	t.Helper()
	t1, err := fx.s.CreateTask(context.Background(), store.Task{
		Title:         title,
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    fx.wf.ID,
		CurrentStepID: fx.wf.FirstStepID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return t1
}
