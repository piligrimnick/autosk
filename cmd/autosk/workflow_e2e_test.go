package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/comments"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/poller"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/step"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// e2eFakeNpm is a no-op npm runner used by the in-process e2e stack.
// installStubPkg writes the on-disk shape directly.
type e2eFakeNpm struct{}

func (e2eFakeNpm) Install(_ context.Context, prefix, spec string) error  { return nil }
func (e2eFakeNpm) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

func installStubPkg(t *testing.T, reg *pkgregistry.Registry, name string) {
	t.Helper()
	pj := map[string]any{
		"name":    name,
		"version": "0.0.1",
		"autosk": map[string]any{"agent": map[string]any{
			"first_message": "You are the " + name + " agent.",
		}},
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

// e2eStack wires the real workflow engine in-process for the W9
// acceptance scenarios. It does not spawn `autosk daemon serve` — the
// goal is to exercise the runstore/poller/scheduler/executor stack
// against real DB rows with a scripted fake pi factory.
type e2eStack struct {
	ts        *doltlite.Store
	signals   *step.Store
	runs      *runstore.Store
	wfs       *workflow.Store
	ag        *agent.Store
	reg       *pkgregistry.Registry
	scheduler *scheduler.Scheduler
	poller    *poller.Poller
	projDir   string
	close     func()
}

// scriptedPi is a per-spawn fake pi.Runner. It emits an agent_end after a
// short delay; what it does inside SendPrompt is controlled by `onPrompt`.
type scriptedPi struct {
	turnEnds chan struct{}
	onPrompt func(prompt string)
	closed   atomic.Bool
}

func (r *scriptedPi) PID() int                                            { return 4242 }
func (r *scriptedPi) Events() <-chan pi.Event                             { return nil }
func (r *scriptedPi) GetState(ctx context.Context) (pi.SessionInfo, error) { return pi.SessionInfo{SessionID: "sess-e2e", SessionFile: "/tmp/e2e.jsonl"}, nil }
func (r *scriptedPi) SendPrompt(ctx context.Context, m string) error {
	if r.onPrompt != nil {
		r.onPrompt(m)
	}
	go func() {
		time.Sleep(2 * time.Millisecond)
		select {
		case r.turnEnds <- struct{}{}:
		default:
		}
	}()
	return nil
}
func (r *scriptedPi) WaitForAgentEnd(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.turnEnds:
		return nil
	}
}
func (r *scriptedPi) Abort(ctx context.Context) error { return nil }
func (r *scriptedPi) CloseStdin() error               { r.closed.Store(true); return nil }
func (r *scriptedPi) Terminate() error                { return nil }
func (r *scriptedPi) Kill() error                     { return nil }
func (r *scriptedPi) Wait(ctx context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

func newE2EStack(t *testing.T) *e2eStack {
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

	// Bring up an isolated packages prefix and install stub packages.
	reg, err := pkgregistry.Open(filepath.Join(dir, "packages"), pkgregistry.WithNpm(e2eFakeNpm{}))
	if err != nil {
		_ = ts.Close()
		t.Fatalf("pkgregistry.Open: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		_ = ts.Close()
		t.Fatalf("EnsurePrefix: %v", err)
	}
	for _, name := range []string{"developer", "code-reviewer", "task-validator"} {
		installStubPkg(t, reg, name)
		if _, err := ag.Create(ctx, name, false); err != nil {
			_ = ts.Close()
			t.Fatalf("agent: %v", err)
		}
	}
	wfs := workflow.New(ts.DB(), ag)

	runs := runstore.New(ts.DB())
	sigs := step.New(ts.DB())
	return &e2eStack{
		ts: ts, signals: sigs, runs: runs, wfs: wfs, ag: ag, reg: reg,
		projDir: dir,
		close:   func() { _ = ts.Close() },
	}
}

// startEngine wires executor + scheduler + poller using the given
// scripted factory. Returns a teardown.
func (s *e2eStack) startEngine(t *testing.T, factory executor.Factory) func() {
	t.Helper()
	ctx := context.Background()
	exec := executor.New(executor.Deps{
		Runs:      s.runs,
		Tasks:     s.ts,
		Agents:    s.ag,
		Workflows: s.wfs,
		Comments:  comments.New(s.ts.DB()),
		Signals:   s.signals,
		Packages:  s.reg,
	}, factory, executor.Config{
		ProjectRoot: s.projDir,
		Grace:       100 * time.Millisecond,
		IdleTimeout: 5 * time.Second,
	})
	sched := scheduler.New(s.runs, scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
		return exec.Run(ctx, jobID)
	}), scheduler.Config{Workers: 1})
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	p := poller.New(s.ts.DB(), s.runs, sched, poller.Config{Interval: 75 * time.Millisecond})
	if err := p.Start(ctx); err != nil {
		_ = sched.Stop(ctx)
		t.Fatal(err)
	}
	s.scheduler = sched
	s.poller = p
	return func() {
		gctx, gc := context.WithTimeout(context.Background(), 5*time.Second)
		defer gc()
		_ = p.Stop(gctx)
		_ = sched.Stop(gctx)
	}
}

// waitForTaskStatus polls the task until its status matches `want` or
// the timeout fires.
func (s *e2eStack) waitForTaskStatus(t *testing.T, taskID string, want store.Status, timeout time.Duration) store.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tk, err := s.ts.GetTask(context.Background(), taskID)
		if err == nil && tk.Status == want {
			return tk
		}
		time.Sleep(40 * time.Millisecond)
	}
	tk, _ := s.ts.GetTask(context.Background(), taskID)
	t.Fatalf("task %s never reached status %q (current: %+v)", taskID, want, tk)
	return store.Task{}
}

// waitForRunStatus polls until at least one daemon_runs row for taskID
// matches the given status, or the timeout fires.
func (s *e2eStack) waitForRunStatus(t *testing.T, taskID string, want runstore.RunStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rs, err := s.runs.ListRuns(context.Background(), runstore.RunFilter{TaskID: taskID})
		if err == nil {
			for _, r := range rs {
				if r.Status == want {
					return
				}
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	rs, _ := s.runs.ListRuns(context.Background(), runstore.RunFilter{TaskID: taskID})
	t.Fatalf("task %s never had a run reach status %q (rows=%+v)", taskID, want, rs)
}

// waitForCurrentStep polls until the task's current step name matches.
func (s *e2eStack) waitForCurrentStep(t *testing.T, taskID, stepName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tk, err := s.ts.GetTask(context.Background(), taskID)
		if err == nil && tk.CurrentStepID != "" {
			st, err := s.wfs.FindStepByID(context.Background(), tk.CurrentStepID)
			if err == nil && st.Name == stepName {
				return
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	tk, _ := s.ts.GetTask(context.Background(), taskID)
	t.Fatalf("task %s never reached step %q (status=%s, current_step_id=%s)", taskID, stepName, tk.Status, tk.CurrentStepID)
}

// scriptedFactory produces stub runners. The per-spawn behaviour is driven
// by `decide`: given the task id and current step name, return the
// transition target. Returning "" means "do nothing" (the executor will
// then kick back).
func scriptedFactory(t *testing.T, stack *e2eStack, decide func(taskID, stepName string) string) executor.Factory {
	var mu sync.Mutex
	return func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		r := &scriptedPi{turnEnds: make(chan struct{}, 4)}
		r.onPrompt = func(prompt string) {
			mu.Lock()
			defer mu.Unlock()
			// Find the active run for any task and emit a signal. We pick
			// up the running run row directly so we don't have to thread
			// the task id through the factory.
			rs, err := stack.runs.ListRuns(ctx, runstore.RunFilter{
				Statuses: []runstore.RunStatus{runstore.StatusRunning},
			})
			if err != nil || len(rs) == 0 {
				return
			}
			run := rs[0]
			st, err := stack.wfs.FindStepByID(ctx, run.StepID)
			if err != nil {
				return
			}
			target := decide(run.TaskID, st.Name)
			if target == "" {
				return // simulate the agent stopping without `step next` → kickback
			}
			if _, err := stack.signals.Emit(ctx, run.TaskID, target); err != nil {
				t.Errorf("scripted Emit(%s, %s): %v", run.TaskID, target, err)
			}
		}
		return r, nil
	}
}

// TestE2E_FeatureDev_HumanFeedback walks dev → review → validator and
// parks at human_feedback. Then we resume back to the validator step and
// verify the engine re-spawns and the task can be advanced again.
func TestE2E_FeatureDev_HumanFeedback(t *testing.T) {
	stack := newE2EStack(t)
	defer stack.close()
	ctx := context.Background()

	def, err := workflow.ParseFile("../../docs/notes/workflow-example.json")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := stack.wfs.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}

	// Initial transitions: dev→review, review→validator, validator→human_feedback.
	// After resume (back to validator), we use a different decision rule that
	// re-routes validator → human_feedback again so we can keep ratcheting.
	var phase atomic.Int32
	decide := func(taskID, stepName string) string {
		_ = phase.Load()
		switch stepName {
		case "dev":
			return "review"
		case "review":
			return "validator"
		case "validator":
			return "human_feedback"
		}
		return ""
	}
	teardown := stack.startEngine(t, scriptedFactory(t, stack, decide))
	defer teardown()

	// Create task in feature-dev at step "dev".
	tk, err := stack.ts.CreateTask(ctx, store.Task{
		Title:         "Implement auth",
		Status:        store.StatusInWorkflow,
		Priority:      1,
		WorkflowID:    wf.ID,
		CurrentStepID: wf.FirstStepID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Walk to human_feedback.
	stack.waitForTaskStatus(t, tk.ID, store.StatusHumanFeedback, 5*time.Second)

	// Check current_step is preserved as "validator" (not cleared).
	post, _ := stack.ts.GetTask(ctx, tk.ID)
	if post.CurrentStepID == "" {
		t.Fatal("current_step_id should be preserved on human_feedback")
	}
	val, err := stack.wfs.FindStepByID(ctx, post.CurrentStepID)
	if err != nil {
		t.Fatal(err)
	}
	if val.Name != "validator" {
		t.Fatalf("expected current step to be 'validator', got %q", val.Name)
	}

	// Resume: flip status back to in_workflow at the same step. The
	// poller picks it up again.
	stRu := store.StatusInWorkflow
	if _, err := stack.ts.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: &stRu}); err != nil {
		t.Fatal(err)
	}
	// After resume, the scripted agent will pick "human_feedback" again
	// (same decide map). Wait until we observe a second human_feedback
	// transition. The simplest check: count daemon_runs rows for the task.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rs, _ := stack.runs.ListRuns(ctx, runstore.RunFilter{TaskID: tk.ID})
		if len(rs) >= 4 {
			// 3 from initial walk (dev, review, validator) + 1 from resume.
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	rs, _ := stack.runs.ListRuns(ctx, runstore.RunFilter{TaskID: tk.ID})
	if len(rs) < 4 {
		t.Fatalf("expected at least 4 daemon_runs rows after resume; got %d", len(rs))
	}
	// And the task should be back at human_feedback.
	stack.waitForTaskStatus(t, tk.ID, store.StatusHumanFeedback, 3*time.Second)
}

// TestE2E_SingleAgent_Done covers the `--agent NAME` shorthand: the task
// joins a synthetic single:<name> workflow at step "do" and the (scripted)
// agent immediately emits `--to done`.
func TestE2E_SingleAgent_Done(t *testing.T) {
	stack := newE2EStack(t)
	defer stack.close()
	ctx := context.Background()

	syn, err := stack.wfs.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}

	decide := func(taskID, stepName string) string {
		if stepName == "do" {
			return "done"
		}
		return ""
	}
	teardown := stack.startEngine(t, scriptedFactory(t, stack, decide))
	defer teardown()

	tk, err := stack.ts.CreateTask(ctx, store.Task{
		Title:         "Bump version",
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    syn.ID,
		CurrentStepID: syn.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := stack.waitForTaskStatus(t, tk.ID, store.StatusDone, 5*time.Second)
	if got.CurrentStepID != "" {
		t.Fatalf("done task should have current_step_id cleared, got %q", got.CurrentStepID)
	}
	if got.WorkflowID != syn.ID {
		t.Fatalf("workflow_id should be preserved for audit, got %q", got.WorkflowID)
	}

	// The executor flips the task to done BEFORE marking the run done
	// (advanceTask runs first, then runstore.MarkDone). Poll the run row
	// here too so we don't race with the trailing MarkDone call.
	stack.waitForRunStatus(t, tk.ID, runstore.StatusDone, 2*time.Second)
	rs, _ := stack.runs.ListRuns(ctx, runstore.RunFilter{TaskID: tk.ID})
	if len(rs) != 1 {
		t.Fatalf("expected 1 run row, got %d", len(rs))
	}
}

// TestE2E_Kickback_FailsAfterMax verifies the executor's kickback loop
// in an end-to-end flow: the scripted agent never emits a signal, so
// after max_corrections+1 turns the run is marked failed.
func TestE2E_Kickback_FailsAfterMax(t *testing.T) {
	stack := newE2EStack(t)
	defer stack.close()
	ctx := context.Background()

	syn, err := stack.wfs.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	decide := func(taskID, stepName string) string { return "" } // never signal
	teardown := stack.startEngine(t, scriptedFactory(t, stack, decide))
	defer teardown()

	tk, err := stack.ts.CreateTask(ctx, store.Task{
		Title:         "Will fail",
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    syn.ID,
		CurrentStepID: syn.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for at least one daemon_runs row to reach `failed`.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rs, _ := stack.runs.ListRuns(ctx, runstore.RunFilter{TaskID: tk.ID})
		if len(rs) > 0 && rs[0].Status == runstore.StatusFailed {
			if rs[0].Error == executor.ErrAgentDidNotEmit.Error() {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	rs, _ := stack.runs.ListRuns(ctx, runstore.RunFilter{TaskID: tk.ID})
	t.Fatalf("kickback path did not produce a failed run with the expected error; rows=%d %+v", len(rs), rs)
}
