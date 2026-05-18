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
	"autosk/internal/daemon/agentnode"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/step"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// nodeStub mimics the agentnode.Runner contract: SendPrompt writes the
// seed once, WaitForAgentEnd returns when the test fires triggerExit.
type nodeStub struct {
	mu          sync.Mutex
	receivedSeed string
	promptCount  int

	exitOnce sync.Once
	doneCh   chan struct{}

	terminated atomic.Bool
	killed     atomic.Bool
}

func newNodeStub() *nodeStub {
	return &nodeStub{doneCh: make(chan struct{})}
}

// triggerExit lets the test simulate the Node child completing.
func (n *nodeStub) triggerExit() {
	n.exitOnce.Do(func() { close(n.doneCh) })
}

func (n *nodeStub) PID() int                                            { return 9999 }
func (n *nodeStub) Events() <-chan pi.Event {
	c := make(chan pi.Event)
	close(c)
	return c
}
func (n *nodeStub) GetState(_ context.Context) (pi.SessionInfo, error) { return pi.SessionInfo{}, nil }

func (n *nodeStub) SendPrompt(_ context.Context, payload string) error {
	n.mu.Lock()
	n.promptCount++
	n.receivedSeed = payload
	n.mu.Unlock()
	return nil
}

func (n *nodeStub) WaitForAgentEnd(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-n.doneCh:
		return nil
	}
}

func (n *nodeStub) Abort(_ context.Context) error {
	n.triggerExit()
	return nil
}
func (n *nodeStub) CloseStdin() error  { return nil }
func (n *nodeStub) Terminate() error   { n.terminated.Store(true); n.triggerExit(); return nil }
func (n *nodeStub) Kill() error        { n.killed.Store(true); n.triggerExit(); return nil }
func (n *nodeStub) Wait(_ context.Context, _ time.Duration) (int, error) {
	<-n.doneCh
	return 0, nil
}

// customFixture installs a custom-runner package and primes everything
// needed to spawn an executor run for it.
type customFixture struct {
	ts    *doltlite.Store
	reg   *pkgregistry.Registry
	deps  executor.Deps
	cfg   executor.Config
	wf    workflow.Workflow
	close func()
}

func newCustomFixture(t *testing.T) *customFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatal(err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		_ = ts.Close()
		t.Fatal(err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		_ = ts.Close()
		t.Fatal(err)
	}

	// Pre-place the runtime bootstrapper stub so RuntimeBootstrapPath
	// stat-checks pass in the agentnode runner (we use a stub factory
	// here so the path content doesn't matter; only its existence
	// would, but agentnode.Spawn isn't called via the stub).
	runtimeDir := filepath.Join(prefix, "node_modules", "@autosk", "agent-runtime", "dist")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "bootstrap.js"), []byte("// stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Install a custom-runner agent.
	name := "@autosk/custom-fixture"
	pkgDir := filepath.Join(prefix, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pj := map[string]any{
		"name":    name,
		"version": "1.0.0",
		"autosk":  map[string]any{"agent": map[string]any{"runner": "./agent.ts"}},
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "agent.ts"), []byte("export default async () => {};"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Install(ctx, name, "1.0.0"); err != nil {
		_ = ts.Close()
		t.Fatalf("install: %v", err)
	}

	ag := agent.New(ts.DB())
	if _, err := ag.Create(ctx, name, false); err != nil {
		_ = ts.Close()
		t.Fatalf("agent.Create: %v", err)
	}
	wfs := workflow.New(ts.DB(), ag)
	w, err := wfs.EnsureSingle(ctx, name)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("EnsureSingle: %v", err)
	}

	return &customFixture{
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
			IdleTimeout: 2 * time.Second,
		},
		wf:    w,
		close: func() { _ = ts.Close() },
	}
}

func (fx *customFixture) makeRun(t *testing.T, title string) (taskID, jobID string) {
	t.Helper()
	ctx := context.Background()
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		Title:         title,
		Status:        store.StatusInWorkflow,
		Priority:      2,
		WorkflowID:    fx.wf.ID,
		CurrentStepID: fx.wf.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{
		TaskID: tk.ID, StepID: fx.wf.Steps[0].ID, MaxCorrections: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk.ID, r.JobID
}

// TestCustomBranch_HappyPath: the stub Node child exits cleanly after
// emitting a `done` signal. The run advances and marks done.
func TestCustomBranch_HappyPath(t *testing.T) {
	fx := newCustomFixture(t)
	defer fx.close()
	ctx := context.Background()

	taskID, jobID := fx.makeRun(t, "Lint pass")

	node := newNodeStub()
	var seenOpts agentnode.Opts
	nodeFactory := func(_ context.Context, opts agentnode.Opts) (executor.PiRunner, error) {
		seenOpts = opts
		// Simulate the runner emitting a step_signal and then exiting.
		go func() {
			time.Sleep(5 * time.Millisecond)
			if _, err := fx.deps.Signals.Emit(ctx, taskID, "done"); err != nil {
				t.Errorf("Emit: %v", err)
			}
			node.triggerExit()
		}()
		return node, nil
	}

	exec := executor.New(fx.deps, nil, fx.cfg).WithNodeFactory(nodeFactory)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// agentnode.Opts should reference the resolved runner path + the
	// runtime bootstrap path.
	if !strings.HasSuffix(seenOpts.RunnerPath, "agent.ts") {
		t.Errorf("RunnerPath = %q", seenOpts.RunnerPath)
	}
	if seenOpts.PackageName != "@autosk/custom-fixture" {
		t.Errorf("PackageName = %q", seenOpts.PackageName)
	}
	if !strings.HasSuffix(seenOpts.BootstrapPath, filepath.Join("agent-runtime", "dist", "bootstrap.js")) {
		t.Errorf("BootstrapPath = %q", seenOpts.BootstrapPath)
	}

	// Seed was written as JSON.
	node.mu.Lock()
	seed := node.receivedSeed
	count := node.promptCount
	node.mu.Unlock()
	if count != 1 {
		t.Errorf("SendPrompt count = %d, want 1", count)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(seed)), &decoded); err != nil {
		t.Fatalf("seed not JSON: %v\nbody=%s", err, seed)
	}
	if decoded["schema_version"] == nil {
		t.Errorf("seed missing schema_version: %v", decoded)
	}
	if decoded["agent_name"] != "@autosk/custom-fixture" {
		t.Errorf("seed agent_name = %v", decoded["agent_name"])
	}
	if task, ok := decoded["task"].(map[string]any); !ok || task["id"] != taskID {
		t.Errorf("seed task.id mismatch: %v", decoded["task"])
	}
	if transitions, ok := decoded["transitions"].([]any); !ok || len(transitions) != 3 {
		t.Errorf("seed transitions: %v", decoded["transitions"])
	}

	// Task advanced to done.
	tk, _ := fx.ts.GetTask(ctx, taskID)
	if tk.Status != store.StatusDone {
		t.Fatalf("task.Status = %s, want done", tk.Status)
	}
}

// TestCustomBranch_NoSignalFailsImmediately verifies that when the
// custom runner exits without emitting a step_signal, the run fails
// with agent_did_not_emit_transition and no kickback is attempted
// (custom runners are single-shot).
func TestCustomBranch_NoSignalFailsImmediately(t *testing.T) {
	fx := newCustomFixture(t)
	defer fx.close()
	ctx := context.Background()

	_, jobID := fx.makeRun(t, "Forgetful")

	node := newNodeStub()
	nodeFactory := func(_ context.Context, _ agentnode.Opts) (executor.PiRunner, error) {
		// Exit without emitting a signal.
		go func() {
			time.Sleep(5 * time.Millisecond)
			node.triggerExit()
		}()
		return node, nil
	}
	exec := executor.New(fx.deps, nil, fx.cfg).WithNodeFactory(nodeFactory)
	err := exec.Run(ctx, jobID)
	if !errors.Is(err, executor.ErrAgentDidNotEmit) {
		t.Fatalf("want ErrAgentDidNotEmit, got %v", err)
	}
	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusFailed {
		t.Fatalf("run.Status = %s, want failed", run.Status)
	}
	// Only one prompt — no kickback.
	node.mu.Lock()
	count := node.promptCount
	node.mu.Unlock()
	if count != 1 {
		t.Errorf("SendPrompt count = %d, want 1 (no kickback for custom runners)", count)
	}
}
