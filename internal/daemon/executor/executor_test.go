package executor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/comments"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/step"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// ---- stub pi runner ------------------------------------------------------

// stubRunner emits an agent_end every time SendPrompt is called. A
// callback `onPrompt` may be set to record / react to each prompt.
type stubRunner struct {
	events     chan pi.Event
	turnEnds   chan struct{}
	exitCode   int
	prompts    []string
	mu         sync.Mutex
	terminated atomic.Bool
	closed     atomic.Bool
	onPrompt   func(prompt string, attempt int)
}

func newStub() *stubRunner {
	return &stubRunner{
		events:   make(chan pi.Event, 8),
		turnEnds: make(chan struct{}, 8),
		exitCode: 0,
	}
}

func (r *stubRunner) PID() int                                            { return 4242 }
func (r *stubRunner) Events() <-chan pi.Event                             { return r.events }
func (r *stubRunner) GetState(ctx context.Context) (pi.SessionInfo, error) { return pi.SessionInfo{SessionID: "sess-stub", SessionFile: "/tmp/stub.jsonl"}, nil }

func (r *stubRunner) SendPrompt(ctx context.Context, m string) error {
	r.mu.Lock()
	r.prompts = append(r.prompts, m)
	cb := r.onPrompt
	attempt := len(r.prompts)
	r.mu.Unlock()
	if cb != nil {
		cb(m, attempt)
	}
	// Schedule an agent_end shortly so the executor moves on.
	go func() {
		time.Sleep(5 * time.Millisecond)
		select {
		case r.turnEnds <- struct{}{}:
		default:
		}
	}()
	return nil
}

func (r *stubRunner) WaitForAgentEnd(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.turnEnds:
		return nil
	}
}

func (r *stubRunner) Abort(ctx context.Context) error { return nil }
func (r *stubRunner) CloseStdin() error               { r.closed.Store(true); return nil }
func (r *stubRunner) Terminate() error                { r.terminated.Store(true); return nil }
func (r *stubRunner) Kill() error                     { return nil }
func (r *stubRunner) Wait(ctx context.Context, _ time.Duration) (int, error) {
	return r.exitCode, nil
}

func stubFactory(r *stubRunner) executor.Factory {
	return func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		return r, nil
	}
}

// ---- shared fixture ------------------------------------------------------

type execFixture struct {
	ts      *doltlite.Store
	deps    executor.Deps
	cfg     executor.Config
	wf      workflow.Workflow
	close   func()
}

func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ts := doltlite.New()
	if err := ts.Open(ctx, dbPath); err != nil {
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
			t.Fatalf("agent %s: %v", name, err)
		}
	}
	wfs := workflow.New(ts.DB(), ag)
	def, err := workflow.ParseFile("../../../docs/notes/workflow-example.json")
	if err != nil {
		_ = ts.Close()
		t.Fatalf("ParseFile: %v", err)
	}
	wf, err := wfs.Create(ctx, def, false)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("Workflow Create: %v", err)
	}

	// Stub agent config files for every step's agent.
	agentsDir := filepath.Join(dir, ".autosk", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		_ = ts.Close()
		t.Fatalf("mkdir agents: %v", err)
	}
	for _, name := range []string{"developer", "code-reviewer", "task-validator"} {
		body := "model = \"sonnet:high\"\nthinking = \"high\"\nsystem_prompt = \"You are the " + name + " agent.\"\n"
		if err := os.WriteFile(filepath.Join(agentsDir, name+".toml"), []byte(body), 0o644); err != nil {
			_ = ts.Close()
			t.Fatalf("write agent config: %v", err)
		}
	}

	return &execFixture{
		ts: ts,
		deps: executor.Deps{
			Runs:      runstore.New(ts.DB()),
			Tasks:     ts,
			Agents:    ag,
			Workflows: wfs,
			Comments:  comments.New(ts.DB()),
			Signals:   step.New(ts.DB()),
		},
		cfg: executor.Config{
			ProjectRoot: dir,
			Grace:       100 * time.Millisecond,
			IdleTimeout: 5 * time.Second,
		},
		wf:    wf,
		close: func() { _ = ts.Close() },
	}
}

func (fx *execFixture) makeRun(t *testing.T, taskTitle, stepName string) (taskID, jobID string) {
	t.Helper()
	ctx := context.Background()
	// Resolve step id from the workflow.
	var stepID string
	for _, s := range fx.wf.Steps {
		if s.Name == stepName {
			stepID = s.ID
			break
		}
	}
	if stepID == "" {
		t.Fatalf("step %q not found in fixture wf", stepName)
	}
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         taskTitle,
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    fx.wf.ID,
		CurrentStepID: stepID,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{
		TaskID: tk.ID, StepID: stepID, MaxCorrections: 2,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return tk.ID, r.JobID
}

// ---- tests ---------------------------------------------------------------

// TestRun_AdvancesOnValidSignal verifies the happy path: the agent emits
// `step next --to review` on its first turn, the executor records run
// done and advances the task's current_step to the review step.
func TestRun_AdvancesOnValidSignal(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "Implement X", "dev")

	stub := newStub()
	// On first prompt, emit the signal so when WaitForAgentEnd returns,
	// step_signals has a row.
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, taskID, "review"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run row: done, with the transition recorded.
	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusDone {
		t.Fatalf("run.Status: %s (want done)", run.Status)
	}
	if run.TransitionID == nil || *run.TransitionID == 0 {
		t.Errorf("transition_id not recorded: %v", run.TransitionID)
	}
	// Task advanced to review.
	tk, _ := fx.ts.GetTask(ctx, taskID)
	if tk.Status != store.StatusInWorkflow {
		t.Fatalf("task.Status: %s", tk.Status)
	}
	rev, err := fx.deps.Workflows.FindStepByName(ctx, fx.wf.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if tk.CurrentStepID != rev.ID {
		t.Fatalf("current_step_id: %s, want %s", tk.CurrentStepID, rev.ID)
	}
}

// TestRun_TaskStatusHumanFeedback verifies the executor parks the task in
// human_feedback and preserves current_step_id.
func TestRun_TaskStatusHumanFeedback(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "Validate X", "validator")

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, taskID, "human_feedback"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tk, _ := fx.ts.GetTask(ctx, taskID)
	if tk.Status != store.StatusHumanFeedback {
		t.Fatalf("task.Status: %s", tk.Status)
	}
	val, _ := fx.deps.Workflows.FindStepByName(ctx, fx.wf.ID, "validator")
	if tk.CurrentStepID != val.ID {
		t.Fatalf("current_step_id should be preserved: %s vs %s", tk.CurrentStepID, val.ID)
	}
}

// TestRun_KickbackThenFail: no signal across max_corrections+1 turns →
// failed with agent_did_not_emit_transition.
func TestRun_KickbackThenFail(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	_, jobID := fx.makeRun(t, "Stubborn", "dev")

	stub := newStub() // never emits a signal
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	err := exec.Run(ctx, jobID)
	if !errors.Is(err, executor.ErrAgentDidNotEmit) {
		t.Fatalf("want ErrAgentDidNotEmit, got %v", err)
	}
	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s", run.Status)
	}
	if run.Error == "" || run.Error != executor.ErrAgentDidNotEmit.Error() {
		t.Fatalf("run.Error: %q", run.Error)
	}
	// max_corrections=2 → initial + 2 kickbacks = 3 prompts.
	stub.mu.Lock()
	n := len(stub.prompts)
	stub.mu.Unlock()
	if n != 3 {
		t.Fatalf("prompts sent: %d (want 3 = initial + 2 kickbacks)", n)
	}
}

// TestRun_TerminalDoneClearsStep verifies that a `--to done` transition
// flips the task to done and clears current_step_id; workflow_id is kept
// for audit.
func TestRun_TerminalDoneClearsStep(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	// Use the synthetic single:dev path so we have a "done" task_status
	// transition on the first step.
	syn, err := fx.deps.Workflows.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	stepID := syn.Steps[0].ID
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         "Bump version",
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    syn.ID,
		CurrentStepID: stepID,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: stepID})
	if err != nil {
		t.Fatal(err)
	}

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "done"); err != nil {
				t.Errorf("Emit done: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, r.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := fx.ts.GetTask(ctx, tk.ID)
	if got.Status != store.StatusDone {
		t.Fatalf("task.Status: %s", got.Status)
	}
	if got.CurrentStepID != "" {
		t.Fatalf("current_step_id should be cleared, got %q", got.CurrentStepID)
	}
	if got.WorkflowID != syn.ID {
		t.Fatalf("workflow_id should be preserved, got %q", got.WorkflowID)
	}
}

// TestRun_MissingAgentConfig surfaces a clean failure.
func TestRun_MissingAgentConfig(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	// Remove the developer config so the executor can't load it.
	_ = os.Remove(filepath.Join(fx.cfg.ProjectRoot, ".autosk", "agents", "developer.toml"))
	taskID, jobID := fx.makeRun(t, "Bad", "dev")

	stub := newStub()
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	err := exec.Run(context.Background(), jobID)
	if err == nil {
		t.Fatal("expected agent_config_invalid error")
	}
	run, _ := fx.deps.Runs.GetRun(context.Background(), jobID)
	if run.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s", run.Status)
	}
	if !contains(run.Error, "agent_config_invalid") {
		t.Errorf("run.Error: %q (expected agent_config_invalid)", run.Error)
	}
	// Failure parking: the task should have moved to human_feedback so
	// the poller stops re-picking it.
	tk, _ := fx.ts.GetTask(context.Background(), taskID)
	if tk.Status != store.StatusHumanFeedback {
		t.Fatalf("task should be parked to human_feedback, got %s", tk.Status)
	}
	if tk.CurrentStepID == "" {
		t.Fatalf("current_step_id should be preserved on park (so resume works)")
	}
}

// TestRun_KickbackThenFail_ParksTask: when the kickback budget is
// exhausted, the task should also be parked (so the poller doesn't
// resurrect it on the next tick).
func TestRun_KickbackThenFail_ParksTask(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "StuckLoop", "dev")

	stub := newStub() // never emits a signal
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err == nil {
		t.Fatal("expected ErrAgentDidNotEmit")
	}
	tk, _ := fx.ts.GetTask(ctx, taskID)
	if tk.Status != store.StatusHumanFeedback {
		t.Fatalf("task should be parked, got %s", tk.Status)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && haystack != "" && needle != "" && (haystack == needle || indexOf(haystack, needle) >= 0)
}

// indexOf is a tiny strings.Index replacement so this file stays import-light.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
