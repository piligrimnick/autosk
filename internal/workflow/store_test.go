package workflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

func newWFFixture(t *testing.T) (*workflow.Store, *agent.Store, *doltlite.Store, func()) {
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
			t.Fatalf("create agent %s: %v", name, err)
		}
	}
	return workflow.New(ts.DB(), ag), ag, ts, func() { _ = ts.Close() }
}

func TestCreate_RoundTripExample(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()

	def, err := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.Name != "feature-dev" {
		t.Errorf("name: %q", w.Name)
	}
	if len(w.Steps) != 3 {
		t.Fatalf("steps: %d", len(w.Steps))
	}
	if w.Steps[0].Name != "dev" || w.Steps[1].Name != "review" || w.Steps[2].Name != "validator" {
		t.Errorf("step order: %v", stepNames(w.Steps))
	}
	if w.FirstStepID != w.Steps[0].ID {
		t.Errorf("first_step_id != steps[0].id")
	}
	// Validator has one sibling and one task_status transition.
	val := w.Steps[2]
	if len(val.Transitions) != 2 {
		t.Fatalf("validator transitions: %d", len(val.Transitions))
	}
	if val.Transitions[0].NextStepName != "dev" || val.Transitions[0].IsTaskStatus() {
		t.Errorf("transition 0: %+v", val.Transitions[0])
	}
	if val.Transitions[1].TaskStatus != "human_feedback" {
		t.Errorf("transition 1 task_status: %q", val.Transitions[1].TaskStatus)
	}
}

func TestCreate_DuplicateNameRejected(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatal(err)
	}
	_, err := wf.Create(ctx, def, false)
	if !errors.Is(err, workflow.ErrAlreadyExist) {
		t.Fatalf("want ErrAlreadyExist, got %v", err)
	}
}

func TestCreate_AgentMissingFailsValidate(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	body := `{
		"name": "bad", "first_step": "a",
		"steps": {"a": {"agent": "nobody", "next_steps": [{"task_status":"done","prompt_rule":"."}]}}
	}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Create(ctx, def, false)
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("want agent-not-found error, got %v", err)
	}
}

func TestList_HidesSyntheticByDefault(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()

	// Real workflow.
	def, _ := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatal(err)
	}
	// Synthetic.
	if _, err := wf.EnsureSingle(ctx, "developer"); err != nil {
		t.Fatal(err)
	}

	visible, err := wf.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].Name != "feature-dev" {
		t.Fatalf("visible list: %v", workflowNames(visible))
	}
	all, err := wf.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all list len: %d", len(all))
	}
}

func TestEnsureSingle_Idempotent(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w1, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if w1.ID != w2.ID {
		t.Fatalf("not idempotent: %s vs %s", w1.ID, w2.ID)
	}
	if !w1.IsSynthetic {
		t.Fatal("synthetic flag not set")
	}
	if len(w1.Steps) != 1 || w1.Steps[0].Name != "do" {
		t.Fatalf("synthetic shape: %+v", stepNames(w1.Steps))
	}
	if len(w1.Steps[0].Transitions) != 3 {
		t.Fatalf("expected 3 transitions, got %d", len(w1.Steps[0].Transitions))
	}
}

func TestEnsureSingle_ConcurrentCreate(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	const N = 4
	results := make(chan workflow.Workflow, N)
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := wf.EnsureSingle(ctx, "developer")
			if err != nil {
				errs <- err
				return
			}
			results <- w
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	if len(errs) != 0 {
		for e := range errs {
			t.Errorf("concurrent EnsureSingle returned error: %v", e)
		}
	}
	var id string
	for w := range results {
		if id == "" {
			id = w.ID
		} else if w.ID != id {
			t.Fatalf("different ids from concurrent ensure: %s vs %s", id, w.ID)
		}
	}
}

func TestDelete_RefusesInUse(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/notes/workflow-example.json")
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	// Add a task that points at the workflow.
	tk, err := dl.CreateTask(ctx, store.Task{Title: "ref", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id = ?`, w.ID, tk.ID); err != nil {
		t.Fatal(err)
	}
	err = wf.Delete(ctx, w.Name)
	if !errors.Is(err, workflow.ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
}

func TestDelete_Ok(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatal(err)
	}
	if err := wf.Delete(ctx, "feature-dev"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := wf.GetByName(ctx, "feature-dev")
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("post-delete GetByName: %v", err)
	}
}

func TestFindStepByName(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/notes/workflow-example.json")
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := wf.FindStepByName(ctx, w.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "review" || st.AgentName != "code-reviewer" {
		t.Fatalf("unexpected step: %+v", st)
	}
	if len(st.Transitions) != 2 {
		t.Fatalf("transitions: %d", len(st.Transitions))
	}
	_, err = wf.FindStepByName(ctx, w.ID, "nope")
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func stepNames(steps []workflow.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func workflowNames(ws []workflow.Workflow) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}
