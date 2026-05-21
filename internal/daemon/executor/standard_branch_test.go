package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// observingRunner captures the pi.Opts passed to the factory so we can
// assert on the standard-branch spawn arguments.
type observingRunner struct {
	*stubRunner
	opts pi.Opts
}

// stubFactoryWithObserve returns a factory + a pointer to the captured
// opts so a test can inspect them after Run completes.
func stubFactoryWithObserve(stub *stubRunner) (executor.Factory, *pi.Opts) {
	var got pi.Opts
	var mu sync.Mutex
	f := func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		mu.Lock()
		got = opts
		mu.Unlock()
		return stub, nil
	}
	return f, &got
}

// TestStandardBranch_BuildsPiOptsFromPackage verifies that for a
// package without a runner, the executor spawns pi with all the
// fields from the resolved PackageConfig (model, thinking,
// extra_args, pi_extensions, pi_skills).
func TestStandardBranch_BuildsPiOptsFromPackage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ts := doltlite.New()
	if err := ts.Open(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if err := ts.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}
	// Install a "fancy" agent with extensions and skills.
	name := "fancy"
	pkgDir := filepath.Join(prefix, "node_modules", name)
	if err := os.MkdirAll(filepath.Join(pkgDir, "ext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pkgDir, "skill-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ext/one.ts", "ext/two.ts", "skill-x/SKILL.md"} {
		if err := os.WriteFile(filepath.Join(pkgDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pj := map[string]any{
		"name":    name,
		"version": "0.1.0",
		"autosk": map[string]any{"agent": map[string]any{
			"model":         "sonnet:high",
			"thinking":      "high",
			"first_message": "You are fancy.",
			"extra_args":    []string{"--no-tool", "web_fetch"},
			"pi_extensions": []string{"./ext/one.ts", "./ext/two.ts"},
			"pi_skills":     []string{"./skill-x"},
		}},
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Install(ctx, name, "0.1.0"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	ag := agent.New(ts.DB())
	if _, err := ag.Create(ctx, name, false); err != nil {
		t.Fatalf("agent.Create: %v", err)
	}
	wfs := workflow.New(ts.DB(), ag)
	w, err := wfs.EnsureSingle(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := ts.CreateTask(ctx, store.Task{
		Title:         "Do",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    w.ID,
		CurrentStepID: w.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs := runstore.New(ts.DB())
	r, err := runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: w.Steps[0].ID})
	if err != nil {
		t.Fatal(err)
	}

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		// Emit done so the run completes after one turn.
		_, _ = step.New(ts.DB()).Emit(ctx, tk.ID, "done")
	}
	factory, gotOpts := stubFactoryWithObserve(stub)

	exec := executor.New(executor.Deps{
		Runs:      runs,
		Tasks:     ts,
		Agents:    ag,
		Workflows: wfs,
		Comments:  comments.New(ts.DB()),
		Signals:   step.New(ts.DB()),
		Packages:  reg,
	}, factory, executor.Config{
		ProjectRoot: dir,
		Grace:       100 * time.Millisecond,
		IdleTimeout: 5 * time.Second,
	})
	if err := exec.Run(ctx, r.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotOpts.Model != "sonnet:high" || gotOpts.Thinking != "high" {
		t.Errorf("model/thinking: %+v", *gotOpts)
	}
	// Expect: --no-tool web_fetch -e <abs>/ext/one.ts -e <abs>/ext/two.ts --skill <abs>/skill-x
	want := []string{
		"--no-tool", "web_fetch",
		"-e", filepath.Join(pkgDir, "ext", "one.ts"),
		"-e", filepath.Join(pkgDir, "ext", "two.ts"),
		"--skill", filepath.Join(pkgDir, "skill-x"),
	}
	if len(gotOpts.ExtraArgs) != len(want) {
		t.Fatalf("ExtraArgs len=%d want %d: %v", len(gotOpts.ExtraArgs), len(want), gotOpts.ExtraArgs)
	}
	for i := range want {
		if gotOpts.ExtraArgs[i] != want[i] {
			t.Errorf("ExtraArgs[%d] = %q, want %q", i, gotOpts.ExtraArgs[i], want[i])
		}
	}
	// The system prompt should be the first line(s) of the rendered prompt.
	stub.mu.Lock()
	prompt := strings.Join(stub.prompts, "\n---\n")
	stub.mu.Unlock()
	if !strings.HasPrefix(prompt, "You are fancy.") {
		end := 80
		if end > len(prompt) {
			end = len(prompt)
		}
		t.Errorf("rendered prompt should start with package first_message; got:\n%s", prompt[:end])
	}
	if !strings.Contains(prompt, tk.ID) {
		t.Errorf("rendered prompt missing task id %q", tk.ID)
	}

}

// TestStandardBranch_StepAgentParamsOverridePackage verifies that
// per-step agent.params override the package's defaults end-to-end:
// the spawned pi.Opts carry the overridden model/thinking/extra_args
// and the prompt's first line is the overridden first_message.
func TestStandardBranch_StepAgentParamsOverridePackage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if err := ts.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}
	name := "@autogent/generic"
	// Package defaults: model=sonnet:medium, thinking=low, single arg.
	installStubPackageWith(t, reg, name, map[string]any{
		"model":         "sonnet:medium",
		"thinking":      "low",
		"first_message": "PACKAGE DEFAULT MESSAGE",
		"extra_args":    []string{"--from-pkg"},
	})

	ag := agent.New(ts.DB())
	if _, err := ag.Create(ctx, name, false); err != nil {
		t.Fatalf("agent.Create: %v\n", err)
	}

	// Author a workflow whose single step overrides the package defaults.
	wfBody := fmt.Sprintf(`{
		"name": "override-wf",
		"first_step": "do",
		"steps": {
			"do": {
				"agent": {
					"name": %q,
					"params": {
						"model": "claude-sonnet-4-6",
						"thinking": "high",
						"first_message": "OVERRIDE MESSAGE",
						"extra_args": ["--override-arg", "value"]
					}
				},
				"next_steps": [{"task_status": "done", "prompt_rule": "complete"}]
			}
		}}`, name)
	def, err := workflow.ParseReader(strings.NewReader(wfBody))
	if err != nil {
		t.Fatal(err)
	}
	wfs := workflow.New(ts.DB(), ag)
	w, err := wfs.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}

	tk, err := ts.CreateTask(ctx, store.Task{
		Title:         "Generic task",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    w.ID,
		CurrentStepID: w.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs := runstore.New(ts.DB())
	r, err := runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: w.Steps[0].ID})
	if err != nil {
		t.Fatal(err)
	}

	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		_, _ = step.New(ts.DB()).Emit(ctx, tk.ID, "done")
	}
	factory, gotOpts := stubFactoryWithObserve(stub)

	exec := executor.New(executor.Deps{
		Runs: runs, Tasks: ts, Agents: ag, Workflows: wfs,
		Comments: comments.New(ts.DB()), Signals: step.New(ts.DB()),
		Packages: reg,
	}, factory, executor.Config{
		ProjectRoot: dir,
		Grace:       100 * time.Millisecond,
		IdleTimeout: 5 * time.Second,
	})
	if err := exec.Run(ctx, r.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotOpts.Model != "claude-sonnet-4-6" {
		t.Errorf("model not overridden: %q", gotOpts.Model)
	}
	if gotOpts.Thinking != "high" {
		t.Errorf("thinking not overridden: %q", gotOpts.Thinking)
	}
	wantArgs := []string{"--override-arg", "value"}
	if len(gotOpts.ExtraArgs) != len(wantArgs) {
		t.Fatalf("extra_args not replaced: %v", gotOpts.ExtraArgs)
	}
	for i := range wantArgs {
		if gotOpts.ExtraArgs[i] != wantArgs[i] {
			t.Errorf("extra_args[%d]=%q want %q", i, gotOpts.ExtraArgs[i], wantArgs[i])
		}
	}
	stub.mu.Lock()
	prompt := strings.Join(stub.prompts, "\n---\n")
	stub.mu.Unlock()
	if !strings.HasPrefix(prompt, "OVERRIDE MESSAGE") {
		t.Errorf("first_message not overridden; prompt starts with:\n%s", prompt[:min(80, len(prompt))])
	}
	if strings.Contains(prompt, "PACKAGE DEFAULT MESSAGE") {
		t.Errorf("package default first_message leaked into the prompt")
	}
}

// TestStandardBranch_StepParamsOnRunnerPackageFails verifies that an
// override block targeting a custom-runner package is rejected with
// `agent_config_invalid` at executor time.
func TestStandardBranch_StepParamsOnRunnerPackageFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if err := ts.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(fakeNpm{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}

	// Install a package that declares a (stub) runner so PackageConfig.Runner != "".
	name := "customy"
	pkgDir := filepath.Join(prefix, "node_modules", name)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "runner.js"), []byte("export default async()=>{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pj := map[string]any{
		"name":    name,
		"version": "0.0.1",
		"autosk":  map[string]any{"agent": map[string]any{"runner": "./runner.js"}},
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Install(ctx, name, "0.0.1"); err != nil {
		t.Fatal(err)
	}

	ag := agent.New(ts.DB())
	if _, err := ag.Create(ctx, name, false); err != nil {
		t.Fatal(err)
	}
	wfBody := fmt.Sprintf(`{
		"name": "runner-override-wf",
		"first_step": "do",
		"steps": {
			"do": {
				"agent": {
					"name": %q,
					"params": { "model": "sonnet:high" }
				},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`, name)
	def, err := workflow.ParseReader(strings.NewReader(wfBody))
	if err != nil {
		t.Fatal(err)
	}
	wfs := workflow.New(ts.DB(), ag)
	w, err := wfs.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := ts.CreateTask(ctx, store.Task{
		Title:         "Bad override",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    w.ID,
		CurrentStepID: w.Steps[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs := runstore.New(ts.DB())
	r, err := runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: w.Steps[0].ID})
	if err != nil {
		t.Fatal(err)
	}

	// The executor should fail before spawning anything; the factory
	// would panic if called.
	noSpawn := executor.Factory(func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		t.Fatal("factory must not be invoked when params target a custom-runner package")
		return nil, nil
	})
	exec := executor.New(executor.Deps{
		Runs: runs, Tasks: ts, Agents: ag, Workflows: wfs,
		Comments: comments.New(ts.DB()), Signals: step.New(ts.DB()),
		Packages: reg,
	}, noSpawn, executor.Config{
		ProjectRoot: dir,
		Grace:       100 * time.Millisecond,
		IdleTimeout: 5 * time.Second,
	})
	err = exec.Run(ctx, r.JobID)
	if err == nil || !strings.Contains(err.Error(), "agent_config_invalid") {
		t.Fatalf("want agent_config_invalid, got %v", err)
	}
	run, _ := runs.GetRun(ctx, r.JobID)
	if !strings.Contains(run.Error, "agent_config_invalid") {
		t.Errorf("run.Error: %q", run.Error)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
