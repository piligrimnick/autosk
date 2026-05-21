package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/comments"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/step"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// fakeNpm satisfies pkgregistry.NpmRunner by materialising on-disk
// shapes directly. Mirror of the pkgregistry test fake; kept private
// here so we don't expose test infrastructure across packages.
type fakeNpm struct {
	installFn func(prefix, spec string) error
}

func (f fakeNpm) Install(_ context.Context, prefix, spec string) error {
	if f.installFn != nil {
		return f.installFn(prefix, spec)
	}
	return nil
}

func (f fakeNpm) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

func installStubPackage(t *testing.T, reg *pkgregistry.Registry, name string) {
	t.Helper()
	installStubPackageWith(t, reg, name, map[string]any{
		"model":         "sonnet:high",
		"thinking":      "high",
		"first_message": "You are the " + name + " agent.",
	})
}

func installStubPackageWith(t *testing.T, reg *pkgregistry.Registry, name string, autoskAgent map[string]any) {
	t.Helper()
	pj := map[string]any{
		"name":    name,
		"version": "0.0.1",
		"autosk":  map[string]any{"agent": autoskAgent},
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	dir := filepath.Join(reg.Prefix(), "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), body, 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if _, err := reg.Install(context.Background(), name, "0.0.1"); err != nil {
		t.Fatalf("reg.Install %s: %v", name, err)
	}
}

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

func (r *stubRunner) PID() int                { return 4242 }
func (r *stubRunner) Events() <-chan pi.Event { return r.events }
func (r *stubRunner) GetState(ctx context.Context) (pi.SessionInfo, error) {
	return pi.SessionInfo{SessionID: "sess-stub", SessionFile: "/tmp/stub.jsonl"}, nil
}

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
	ts    *doltlite.Store
	reg   *pkgregistry.Registry
	deps  executor.Deps
	cfg   executor.Config
	wf    workflow.Workflow
	close func()
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

	// Set up an isolated packages prefix and install stub packages for
	// every agent the example workflow references.
	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		_ = ts.Close()
		t.Fatalf("pkgregistry.Open: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		_ = ts.Close()
		t.Fatalf("EnsurePrefix: %v", err)
	}
	for _, name := range []string{"developer", "code-reviewer", "task-validator"} {
		installStubPackage(t, reg, name)
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

	return &execFixture{
		ts:  ts,
		reg: reg,
		deps: executor.Deps{
			Runs:      runstore.New(ts.DB()),
			Tasks:     ts,
			Agents:    ag,
			Workflows: wfs,
			Comments:  comments.New(ts.DB()),
			Signals:   step.New(ts.DB()),
			Packages:  reg,
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
		Status:        store.StatusWork,
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
	if tk.Status != store.StatusWork {
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

// TestRun_TaskStatusHuman verifies the executor parks the task in
// human and preserves current_step_id.
func TestRun_TaskStatusHuman(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "Validate X", "validator")

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, taskID, "human"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tk, _ := fx.ts.GetTask(ctx, taskID)
	if tk.Status != store.StatusHuman {
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
		Status:        store.StatusWork,
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
	// Uninstall the developer package so the executor can't resolve it.
	if err := fx.reg.Uninstall(context.Background(), "developer"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
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
	// Failure parking: the task should have moved to human so
	// the poller stops re-picking it.
	tk, _ := fx.ts.GetTask(context.Background(), taskID)
	if tk.Status != store.StatusHuman {
		t.Fatalf("task should be parked to human, got %s", tk.Status)
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
	if tk.Status != store.StatusHuman {
		t.Fatalf("task should be parked, got %s", tk.Status)
	}
}

// TestAdvanceTask_CapExceeded_ParksOnTargetStep verifies the visit-cap
// enforcement path. We prime metadata.step_visits[review] at the cap,
// drive a `dev` run that signals `--to review`, and assert:
//   - daemon_runs.status='failed'
//   - daemon_runs.error starts with `step_max_visits_exceeded:`
//   - tasks.status='human' (parked)
//   - tasks.current_step_id is the TARGET step (review), so a bare
//     `autosk resume <id>` (no --to) lands on the right step once the
//     human resets visits; the source step is fully done.
//   - tasks.metadata.step_visits[review] is unchanged (NOT incremented)
func TestAdvanceTask_CapExceeded_ParksOnTargetStep(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()

	// Build a 2-step workflow with review capped at 1 visit. We can't
	// retrofit max_visits onto the fixture workflow, so create a fresh
	// one here. The executor only reads from the same wfStore so the
	// new workflow lives alongside `feature-dev`.
	cappedBody := `{
		"name": "capped",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},     "max_visits": 5, "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": 1, "next_steps": [{"step": "dev",    "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(cappedBody))
	if err != nil {
		t.Fatal(err)
	}
	cappedWF, err := fx.deps.Workflows.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID, reviewID string
	for _, s := range cappedWF.Steps {
		switch s.Name {
		case "dev":
			devID = s.ID
		case "review":
			reviewID = s.ID
		}
	}

	// Create a task on dev with step_visits[review] already at the cap.
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         "At the cap",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    cappedWF.ID,
		CurrentStepID: devID,
		Metadata: map[string]any{
			"step_visits": map[string]any{reviewID: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{
		TaskID: tk.ID, StepID: devID, MaxCorrections: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "review"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	err = exec.Run(ctx, run.JobID)
	if err == nil {
		t.Fatal("expected MaxVisitsExceededError")
	}
	var mve workflow.MaxVisitsExceededError
	if !errors.As(err, &mve) {
		t.Fatalf("err type %T: %v (want wrapped MaxVisitsExceededError)", err, err)
	}

	runRow, _ := fx.deps.Runs.GetRun(ctx, run.JobID)
	if runRow.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s (want failed)", runRow.Status)
	}
	if !strings.HasPrefix(runRow.Error, "step_max_visits_exceeded:") {
		t.Fatalf("run.Error: %q (want step_max_visits_exceeded: prefix)", runRow.Error)
	}
	tkAfter, _ := fx.ts.GetTask(ctx, tk.ID)
	if tkAfter.Status != store.StatusHuman {
		t.Fatalf("task parked? got status %s", tkAfter.Status)
	}
	if tkAfter.CurrentStepID != reviewID {
		t.Fatalf("current_step_id should land on the TARGET step (review=%s), got %s (source dev=%s)",
			reviewID, tkAfter.CurrentStepID, devID)
	}
	sv := tkAfter.Metadata["step_visits"].(map[string]any)
	// JSON round-trip widens int → float64.
	if v, _ := sv[reviewID].(float64); int(v) != 1 {
		t.Fatalf("review counter should be UNCHANGED at 1, got %v (md=%+v)", sv[reviewID], tkAfter.Metadata)
	}
}

// TestAdvanceTask_GenericAdvanceError_ParksOnTargetStep verifies that
// the target-step parking applies to ANY error inside EnterStep, not
// just the cap. We inject a sentinel error from a fake taskStore's
// UpdateMetadataAndPatch and assert the task lands on the target
// step — same intent-routing as the cap case.
func TestAdvanceTask_GenericAdvanceError_ParksOnTargetStep(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()

	wfBody := `{
		"name": "genericfail",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},     "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "next_steps": [{"step": "dev",    "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(wfBody))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := fx.deps.Workflows.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID, reviewID string
	for _, s := range wf.Steps {
		switch s.Name {
		case "dev":
			devID = s.ID
		case "review":
			reviewID = s.ID
		}
	}
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         "Generic advance error",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{
		TaskID: tk.ID, StepID: devID, MaxCorrections: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wrap the task store so EnterStep's UpdateMetadataAndPatch fails
	// with a sentinel; every other call passes through.
	sentinel := errors.New("sentinel: simulated advance failure")
	deps := fx.deps
	deps.Tasks = &failingTaskStore{TaskStore: fx.ts, failOnUpdateMD: sentinel}

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "review"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(deps, stubFactory(stub), fx.cfg)
	err = exec.Run(ctx, run.JobID)
	if err == nil {
		t.Fatal("expected a generic advance error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err %T: %v (want unwraps to sentinel)", err, err)
	}
	runRow, _ := fx.deps.Runs.GetRun(ctx, run.JobID)
	if runRow.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s (want failed)", runRow.Status)
	}
	if !strings.HasPrefix(runRow.Error, "advance task: ") {
		t.Fatalf("run.Error: %q (want `advance task: ...` prefix)", runRow.Error)
	}
	tkAfter, _ := fx.ts.GetTask(ctx, tk.ID)
	if tkAfter.Status != store.StatusHuman {
		t.Fatalf("task parked? got status %s", tkAfter.Status)
	}
	if tkAfter.CurrentStepID != reviewID {
		t.Fatalf("current_step_id should land on target (review=%s), got %s (source dev=%s)",
			reviewID, tkAfter.CurrentStepID, devID)
	}
}

// TestAdvanceTask_RunnerCrash_PreservesSourceStep is the regression
// guard for the other failure modes: when the runner dies BEFORE the
// agent emits `step next`, the target step is unknown so the task
// must stay parked on the source step — the only safe landing.
func TestAdvanceTask_RunnerCrash_PreservesSourceStep(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "crash before signal", "dev")

	// Stub fails SendPrompt immediately, triggering handleRunError
	// BEFORE advanceTask gets a chance to compute a target.
	stub := &sendPromptFailStub{stubRunner: newStub(), sendErr: errors.New("boom: pi died")}
	exec := executor.New(fx.deps, sendFailingFactory(stub), fx.cfg)
	err := exec.Run(ctx, jobID)
	if err == nil {
		t.Fatal("expected runner error")
	}
	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s (want failed)", run.Status)
	}
	tk, _ := fx.ts.GetTask(ctx, taskID)
	if tk.Status != store.StatusHuman {
		t.Fatalf("task parked? got status %s", tk.Status)
	}
	dev, err := fx.deps.Workflows.FindStepByName(ctx, fx.wf.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if tk.CurrentStepID != dev.ID {
		t.Fatalf("current_step_id should be PRESERVED as source (dev=%s), got %s",
			dev.ID, tk.CurrentStepID)
	}
}

// failingTaskStore wraps a real TaskStore and forces UpdateMetadataAndPatch
// to return a sentinel error so we can exercise the generic-error
// branch of advanceTask.
type failingTaskStore struct {
	executor.TaskStore
	failOnUpdateMD error
}

func (f *failingTaskStore) UpdateMetadataAndPatch(ctx context.Context, id string, fn func(m map[string]any) error, p store.TaskPatch) (store.Task, error) {
	if f.failOnUpdateMD != nil {
		return store.Task{}, f.failOnUpdateMD
	}
	return f.TaskStore.UpdateMetadataAndPatch(ctx, id, fn, p)
}

// sendPromptFailStub makes the first SendPrompt return an error, which
// drops the executor into handleRunError before advanceTask runs.
type sendPromptFailStub struct {
	*stubRunner
	sendErr error
}

func (s *sendPromptFailStub) SendPrompt(ctx context.Context, m string) error {
	return s.sendErr
}

// sendFailingFactory wraps a *sendPromptFailStub into the
// executor.Factory signature.
func sendFailingFactory(stub *sendPromptFailStub) executor.Factory {
	return func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		return stub, nil
	}
}

// TestRun_AdvanceBumpsVisitCounter verifies the happy-path counter
// increment on a successful sibling-step transition.
func TestRun_AdvanceBumpsVisitCounter(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()

	cappedBody := `{
		"name": "countme",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},     "max_visits": 5, "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": 5, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, _ := workflow.ParseReader(strings.NewReader(cappedBody))
	wf, err := fx.deps.Workflows.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID, reviewID string
	for _, s := range wf.Steps {
		if s.Name == "dev" {
			devID = s.ID
		}
		if s.Name == "review" {
			reviewID = s.ID
		}
	}
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         "counter",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{
		TaskID: tk.ID, StepID: devID, MaxCorrections: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "review"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, run.JobID); err != nil {
		t.Fatal(err)
	}
	tkAfter, _ := fx.ts.GetTask(ctx, tk.ID)
	if tkAfter.CurrentStepID != reviewID {
		t.Fatalf("current_step_id: %s (want review %s)", tkAfter.CurrentStepID, reviewID)
	}
	sv := tkAfter.Metadata["step_visits"].(map[string]any)
	if v, _ := sv[reviewID].(float64); int(v) != 1 {
		t.Fatalf("review counter should be 1, got %v", sv[reviewID])
	}
}

// TestRun_AdvanceCapFires_OperatorRaceNoClobber drives the cap path
// concurrently with a human-side `done` (the operator races the
// executor while it is failing the run). The expected outcome is that
// parkTaskOnFailure's "only park if still work" guard prevents
// the executor from clobbering the operator's terminal status.
//
// We synchronise via the stub's onPrompt callback: emit the cap-fire
// signal, then flip the task to `done` BEFORE returning. By the time
// the executor calls advanceTask, the task is already terminal; the
// subsequent failTerminal path marks the run failed but skips the
// parking step because the task is no longer work.
func TestRun_AdvanceCapFires_OperatorRaceNoClobber(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()

	cappedBody := `{
		"name": "raced",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},     "max_visits": 5, "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": 1, "next_steps": [{"step": "dev",    "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(cappedBody))
	if err != nil {
		t.Fatal(err)
	}
	cappedWF, err := fx.deps.Workflows.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID, reviewID string
	for _, s := range cappedWF.Steps {
		switch s.Name {
		case "dev":
			devID = s.ID
		case "review":
			reviewID = s.ID
		}
	}

	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         "Operator races executor",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    cappedWF.ID,
		CurrentStepID: devID,
		Metadata: map[string]any{
			"step_visits": map[string]any{reviewID: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{
		TaskID: tk.ID, StepID: devID, MaxCorrections: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt != 1 {
			return
		}
		if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "review"); err != nil {
			t.Errorf("Emit: %v", err)
			return
		}
		// Race the executor: while it's still in WaitForAgentEnd /
		// shutdown, mark the task done from the operator side. The cap
		// will fire in advanceTask afterwards; parkTaskOnFailure must
		// then short-circuit because the task is no longer work.
		doneStatus := store.StatusDone
		emptyStep := ""
		if _, err := fx.ts.UpdateTask(ctx, tk.ID, store.TaskPatch{
			Status:        &doneStatus,
			CurrentStepID: &emptyStep,
		}); err != nil {
			t.Errorf("operator UpdateTask: %v", err)
		}
	}

	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	err = exec.Run(ctx, run.JobID)
	if err == nil {
		t.Fatal("expected cap-fire error")
	}
	var mve workflow.MaxVisitsExceededError
	if !errors.As(err, &mve) {
		t.Fatalf("err type %T: %v (want wrapped MaxVisitsExceededError)", err, err)
	}

	// The run is failed, with the expected error...
	runRow, _ := fx.deps.Runs.GetRun(ctx, run.JobID)
	if runRow.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s (want failed)", runRow.Status)
	}
	if !strings.HasPrefix(runRow.Error, "step_max_visits_exceeded:") {
		t.Fatalf("run.Error: %q", runRow.Error)
	}
	// ...but the task is the operator's terminal status, NOT parked.
	tkAfter, _ := fx.ts.GetTask(ctx, tk.ID)
	if tkAfter.Status != store.StatusDone {
		t.Fatalf("task should be the operator's choice (done), got %s", tkAfter.Status)
	}
	if tkAfter.CurrentStepID != "" {
		t.Errorf("current_step_id should be cleared by the operator's done patch, got %q", tkAfter.CurrentStepID)
	}
	// And the cap-side increment must NOT have landed (the EnterStep tx
	// rolled back when the cap fired).
	sv := tkAfter.Metadata["step_visits"].(map[string]any)
	if v, _ := sv[reviewID].(float64); int(v) != 1 {
		t.Fatalf("review counter mutated despite cap-fire: %v", sv[reviewID])
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
