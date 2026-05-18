package executor_test

import (
	"context"
	"encoding/json"
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
		Status:        store.StatusInWorkflow,
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
