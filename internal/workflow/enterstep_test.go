package workflow_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"autosk/internal/meta"
	"autosk/internal/store"
	"autosk/internal/workflow"
)

// buildCappedWorkflow installs a small two-step workflow with the given
// max_visits on `dev`/`review` for EnterStep tests. Returns the
// materialised workflow (so callers can index steps by name) plus the
// dl + wfs handles from the shared fixture.
func buildCappedWorkflow(t *testing.T, devCap, reviewCap int) (workflow.Workflow, *workflow.Store, *cleanupFixture) {
	t.Helper()
	wf, _, dl, done := newWFFixture(t)
	body := `{
		"name": "caps",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},    "max_visits": ` + strconv.Itoa(devCap) + `,    "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": ` + strconv.Itoa(reviewCap) + `, "next_steps": [{"step": "dev",    "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		done()
		t.Fatal(err)
	}
	w, err := wf.Create(context.Background(), def, false)
	if err != nil {
		done()
		t.Fatal(err)
	}
	return w, wf, &cleanupFixture{dl: dl, close: done}
}

// cleanupFixture bundles the doltlite store + the close func returned
// by newWFFixture so tests can use a single defer.
type cleanupFixture struct {
	dl    storeWrapper
	close func()
}

// storeWrapper is the subset of *doltlite.Store the EnterStep tests use.
// Pulled out so we don't reach into the conformance fixture.
type storeWrapper interface {
	CreateTask(ctx context.Context, t store.Task) (store.Task, error)
	GetTask(ctx context.Context, id string) (store.Task, error)
	UpdateTask(ctx context.Context, id string, p store.TaskPatch) (store.Task, error)
	UpdateMetadata(ctx context.Context, id string, fn func(m map[string]any) error) (map[string]any, bool, error)
	UpdateMetadataAndPatch(ctx context.Context, id string, fn func(m map[string]any) error, p store.TaskPatch) (store.Task, error)
}

func newCappedTask(t *testing.T, dl storeWrapper, w workflow.Workflow, stepID string) store.Task {
	t.Helper()
	tk, err := dl.CreateTask(context.Background(), store.Task{
		Title:         "Capped",
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    w.ID,
		CurrentStepID: stepID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func stepIDByName(w workflow.Workflow, name string) string {
	for _, s := range w.Steps {
		if s.Name == name {
			return s.ID
		}
	}
	return ""
}

func TestEnterStep_HappyPath_BumpsCounter(t *testing.T) {
	w, wfs, fx := buildCappedWorkflow(t, 3, 3)
	defer fx.close()
	ctx := context.Background()
	devID := stepIDByName(w, "dev")
	reviewID := stepIDByName(w, "review")

	// Seed: task created in_workflow at dev (no counter recorded yet —
	// the test exercises EnterStep directly, not the create path).
	tk := newCappedTask(t, fx.dl, w, devID)

	if err := workflow.EnterStep(ctx, fx.dl, wfs, workflow.EnterStepInput{
		TaskID: tk.ID, StepID: reviewID,
	}); err != nil {
		t.Fatalf("EnterStep: %v", err)
	}
	got, err := fx.dl.GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentStepID != reviewID {
		t.Fatalf("current_step_id: %s (want %s)", got.CurrentStepID, reviewID)
	}
	if got.Status != store.StatusInWorkflow {
		t.Fatalf("status: %s", got.Status)
	}
	if meta.GetStepVisits(got.Metadata)[reviewID] != 1 {
		t.Fatalf("review counter: %d (want 1); md=%+v", meta.GetStepVisits(got.Metadata)[reviewID], got.Metadata)
	}
}

func TestEnterStep_CapFires_NoMutation(t *testing.T) {
	w, wfs, fx := buildCappedWorkflow(t, 2, 1)
	defer fx.close()
	ctx := context.Background()
	devID := stepIDByName(w, "dev")
	reviewID := stepIDByName(w, "review")
	tk := newCappedTask(t, fx.dl, w, devID)

	// First entry into review consumes the one allowance.
	if err := workflow.EnterStep(ctx, fx.dl, wfs, workflow.EnterStepInput{
		TaskID: tk.ID, StepID: reviewID,
	}); err != nil {
		t.Fatalf("first EnterStep: %v", err)
	}
	// Second entry must fail with MaxVisitsExceededError.
	err := workflow.EnterStep(ctx, fx.dl, wfs, workflow.EnterStepInput{
		TaskID: tk.ID, StepID: reviewID,
	})
	if err == nil {
		t.Fatal("expected MaxVisitsExceededError, got nil")
	}
	var mve workflow.MaxVisitsExceededError
	if !errors.As(err, &mve) {
		t.Fatalf("err type %T (want MaxVisitsExceededError): %v", err, err)
	}
	if mve.StepName != "review" || mve.Max != 1 || mve.Visits != 1 {
		t.Errorf("unexpected fields: %+v", mve)
	}
	// No mutation: counter still 1, current step still review (set by
	// the successful first call).
	got, _ := fx.dl.GetTask(ctx, tk.ID)
	if meta.GetStepVisits(got.Metadata)[reviewID] != 1 {
		t.Fatalf("counter mutated on cap fire: %d", meta.GetStepVisits(got.Metadata)[reviewID])
	}
}

func TestEnterStep_UnlimitedBumpsForever(t *testing.T) {
	w, wfs, fx := buildCappedWorkflow(t, 0, 0)
	defer fx.close()
	ctx := context.Background()
	devID := stepIDByName(w, "dev")
	reviewID := stepIDByName(w, "review")
	tk := newCappedTask(t, fx.dl, w, devID)

	for i := 0; i < 7; i++ {
		if err := workflow.EnterStep(ctx, fx.dl, wfs, workflow.EnterStepInput{
			TaskID: tk.ID, StepID: reviewID,
		}); err != nil {
			t.Fatalf("EnterStep iter %d: %v", i, err)
		}
	}
	got, _ := fx.dl.GetTask(ctx, tk.ID)
	if meta.GetStepVisits(got.Metadata)[reviewID] != 7 {
		t.Fatalf("counter: %d (want 7)", meta.GetStepVisits(got.Metadata)[reviewID])
	}
}

// TestEnterStep_ConcurrentSerialised drives several goroutines through
// EnterStep against a cap of 1 and verifies that exactly one wins.
// Doltlite is single-writer (SetMaxOpenConns(1)) so this exercises the
// serialisation guarantee, not parallel writes per se.
func TestEnterStep_ConcurrentSerialised(t *testing.T) {
	w, wfs, fx := buildCappedWorkflow(t, 2, 1)
	defer fx.close()
	ctx := context.Background()
	devID := stepIDByName(w, "dev")
	reviewID := stepIDByName(w, "review")
	tk := newCappedTask(t, fx.dl, w, devID)

	const N = 4
	var successes, capErrs int32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := workflow.EnterStep(ctx, fx.dl, wfs, workflow.EnterStepInput{
				TaskID: tk.ID, StepID: reviewID,
			})
			if err == nil {
				atomic.AddInt32(&successes, 1)
				return
			}
			var mve workflow.MaxVisitsExceededError
			if errors.As(err, &mve) {
				atomic.AddInt32(&capErrs, 1)
				return
			}
			t.Errorf("unexpected error: %v", err)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&successes); got != 1 {
		t.Fatalf("expected exactly one successful EnterStep (cap=1), got %d successes / %d cap errors",
			got, atomic.LoadInt32(&capErrs))
	}
	if got := atomic.LoadInt32(&capErrs); got != N-1 {
		t.Fatalf("expected %d cap errors, got %d", N-1, got)
	}
	// The persisted counter must be exactly 1.
	got, _ := fx.dl.GetTask(ctx, tk.ID)
	if meta.GetStepVisits(got.Metadata)[reviewID] != 1 {
		t.Fatalf("counter after race: %d", meta.GetStepVisits(got.Metadata)[reviewID])
	}
}

// TestEnterStep_StampsWorkflowID covers the enroll / create paths where
// the caller asks EnterStep to set workflow_id in the same transaction.
func TestEnterStep_StampsWorkflowID(t *testing.T) {
	w, wfs, fx := buildCappedWorkflow(t, 2, 2)
	defer fx.close()
	ctx := context.Background()
	devID := stepIDByName(w, "dev")

	// Create a brand-new (status=new, no workflow_id) task.
	tk, err := fx.dl.CreateTask(ctx, store.Task{
		Title:    "Fresh",
		Status:   store.StatusNew,
		Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.EnterStep(ctx, fx.dl, wfs, workflow.EnterStepInput{
		TaskID: tk.ID, StepID: devID, WorkflowID: w.ID,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := fx.dl.GetTask(ctx, tk.ID)
	if got.WorkflowID != w.ID {
		t.Fatalf("workflow_id not stamped: %q vs %q", got.WorkflowID, w.ID)
	}
	if got.Status != store.StatusInWorkflow {
		t.Fatalf("status: %s", got.Status)
	}
	if meta.GetStepVisits(got.Metadata)[devID] != 1 {
		t.Fatalf("counter: %d", meta.GetStepVisits(got.Metadata)[devID])
	}
}

// TestMapEnterStepError covers the shared friendly-error helper.
// The hint must:
//   - render a clean one-line user message (no duplication of the
//     wrapped error's own Error() text);
//   - include the concrete `autosk metadata reset-visits <task-id> --step
//     <step>` copy-paste so the operator can act without grepping docs;
//   - keep the typed MaxVisitsExceededError reachable via errors.As so
//     CLI / lazy / executor code can still introspect the reason behind
//     the hint.
//
// nil-in/nil-out and non-cap pass-through are tested too — those are
// the only other branches.
func TestMapEnterStepError(t *testing.T) {
	if got := workflow.MapEnterStepError("as-1234", nil); got != nil {
		t.Fatalf("nil-in must be nil-out, got %v", got)
	}
	plain := errors.New("boom")
	if got := workflow.MapEnterStepError("as-1234", plain); got != plain {
		t.Fatalf("non-cap error must pass through verbatim, got %v", got)
	}
	mve := workflow.MaxVisitsExceededError{
		StepID: "st-abcd", StepName: "review", Visits: 2, Max: 2,
	}
	out := workflow.MapEnterStepError("as-1234", mve)
	if out == nil {
		t.Fatal("cap error must produce a hint")
	}
	// The visible message must NOT embed mve.Error(): that's the whole
	// point of decoupling the message from the chain. If the helper
	// ever regresses to `fmt.Errorf("...: %w", mve)`, the test will
	// catch it because the rendered string will contain the typed
	// prefix.
	if strings.Contains(out.Error(), "step_max_visits_exceeded") {
		t.Fatalf("hint must not embed wrapped Error() text, got %q", out.Error())
	}
	if !strings.Contains(out.Error(), `step "review"`) {
		t.Fatalf("hint must mention the step name, got %q", out.Error())
	}
	if !strings.Contains(out.Error(), "max_visits=2") {
		t.Fatalf("hint must mention the cap, got %q", out.Error())
	}
	if !strings.Contains(out.Error(), "autosk metadata reset-visits as-1234 --step review") {
		t.Fatalf("hint must include the copy-pasteable reset command, got %q", out.Error())
	}
	if !strings.Contains(out.Error(), "resume to a different step") {
		t.Fatalf("hint must mention the resume-to-different-step alternative, got %q", out.Error())
	}
	// errors.As must still find the typed reason through the wrapper.
	var got workflow.MaxVisitsExceededError
	if !errors.As(out, &got) {
		t.Fatalf("hint must keep MaxVisitsExceededError in the chain; out=%T %v", out, out)
	}
	if got.StepID != "st-abcd" || got.Max != 2 {
		t.Fatalf("typed fields lost: %+v", got)
	}
	// And the typed wrapper itself must surface via errors.As, so
	// callers can render special UI when they detect a hint.
	var hint *workflow.EnterStepHint
	if !errors.As(out, &hint) {
		t.Fatalf("hint must be an *EnterStepHint, got %T", out)
	}
}
