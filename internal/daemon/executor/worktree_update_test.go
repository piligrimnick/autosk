package executor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// TestRun_Worktree_AfterUpdateNoneToWorktree pins the WU2 daemon
// acceptance criterion: a task whose workflow flipped from
// isolation=none to isolation=worktree --force gets cwd=worktreePath
// and AUTOSK_DB on its NEXT step run. The flip itself is driven
// through workflow.Store.UpdateIsolation (the same path the CLI
// uses).
func TestRun_Worktree_AfterUpdateNoneToWorktree(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()

	gitRoot := setupGitRoot(t)
	t.Setenv("HOME", t.TempDir())
	fx.cfg.ProjectRoot = gitRoot
	fx.cfg.DBPath = filepath.Join(gitRoot, ".autosk", "db")

	// Start with isolation=none.
	body := `{
		"name": "wu-up",
		"first_step": "dev",
		"steps": {
			"dev": {
				"agent": {"name": "developer"},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := fx.deps.Workflows.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID string
	for _, s := range wf.Steps {
		if s.Name == "dev" {
			devID = s.ID
		}
	}

	// Enrol a task in the non-isolated workflow.
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		ID:            "ask-aaa001",
		Title:         "flip me up",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Flip the workflow via the shared store method (mirrors what
	// `autosk workflow update --isolation worktree --force` does).
	wtMgr := worktree.NewManager()
	if _, err := fx.deps.Workflows.UpdateIsolation(ctx, wf.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
		Force:       true,
		ProjectRoot: gitRoot,
		Worktrees:   wtMgr,
	}); err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	wtPath, _ := worktree.PathFor(gitRoot, tk.ID)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("post-flip worktree dir should exist: %v", statErr)
	}

	// Run the next step.
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: devID})
	if err != nil {
		t.Fatal(err)
	}
	var captured pi.Opts
	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "done"); err != nil {
				t.Errorf("Emit done: %v", err)
			}
		}
	}
	factory := func(_ context.Context, opts pi.Opts) (executor.PiRunner, error) {
		captured = opts
		return stub, nil
	}

	deps := fx.deps
	deps.Worktree = wtMgr
	ex := executor.New(deps, factory, fx.cfg)
	if err := ex.Run(ctx, run.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if captured.Cwd != wtPath {
		t.Errorf("post-flip cwd: got %q want %q", captured.Cwd, wtPath)
	}
	if !envHasKV(captured.Env, "AUTOSK_DB", fx.cfg.DBPath) {
		t.Errorf("post-flip env missing AUTOSK_DB=%q: %v", fx.cfg.DBPath, captured.Env)
	}
	// The terminal cleanup hook also fires; dir gone after Run.
	if _, statErr := os.Stat(wtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("worktree should be removed on terminal done: stat err=%v", statErr)
	}
}

// TestRun_Worktree_AfterUpdateWorktreeToNone pins the symmetric WU2
// daemon acceptance: a task whose workflow flipped from
// isolation=worktree to isolation=none --force runs with
// cwd=projectRoot (NOT the worktree path) on its next step. The
// leftover worktree directory is intentionally NOT removed by the
// flip itself.
func TestRun_Worktree_AfterUpdateWorktreeToNone(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()

	gitRoot := setupGitRoot(t)
	t.Setenv("HOME", t.TempDir())
	fx.cfg.ProjectRoot = gitRoot
	fx.cfg.DBPath = filepath.Join(gitRoot, ".autosk", "db")

	body := `{
		"name": "wu-down",
		"first_step": "dev",
		"isolation": "worktree",
		"steps": {
			"dev": {
				"agent": {"name": "developer"},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := fx.deps.Workflows.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID string
	for _, s := range wf.Steps {
		if s.Name == "dev" {
			devID = s.ID
		}
	}

	// Allocate a worktree (simulates `autosk enroll` on the still-isolated
	// workflow).
	wtMgr := worktree.NewManager()
	if _, err := wtMgr.Ensure(ctx, gitRoot, "ask-bbb002", ""); err != nil {
		t.Fatal(err)
	}
	wtPath, _ := worktree.PathFor(gitRoot, "ask-bbb002")
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("pre-flip worktree should exist: %v", statErr)
	}

	tk, err := fx.ts.CreateTask(ctx, store.Task{
		ID:            "ask-bbb002",
		Title:         "flip me down",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Flip down to isolation=none --force.
	if _, err := fx.deps.Workflows.UpdateIsolation(ctx, wf.Name, workflow.IsolationNone, workflow.UpdateIsolationOpts{
		Force:       true,
		ProjectRoot: gitRoot,
		Worktrees:   wtMgr,
	}); err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	// The leftover dir survives: the flip is intentionally a no-op on
	// disk.
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("worktree→none --force should NOT remove the dir: stat err=%v", statErr)
	}

	// Now the next step should run with cwd=projectRoot, NOT the
	// leftover worktree path.
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: devID})
	if err != nil {
		t.Fatal(err)
	}
	var captured pi.Opts
	stub := newStub()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			if _, err := fx.deps.Signals.Emit(ctx, tk.ID, "done"); err != nil {
				t.Errorf("Emit done: %v", err)
			}
		}
	}
	factory := func(_ context.Context, opts pi.Opts) (executor.PiRunner, error) {
		captured = opts
		return stub, nil
	}

	deps := fx.deps
	deps.Worktree = wtMgr
	ex := executor.New(deps, factory, fx.cfg)
	if err := ex.Run(ctx, run.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if captured.Cwd != gitRoot {
		t.Errorf("post-flip cwd: got %q want projectRoot %q", captured.Cwd, gitRoot)
	}
	// AUTOSK_DB is only set for isolated runs; the non-isolated path
	// doesn't add it. We don't assert its absence (the daemon may
	// have it in its env), only that the cwd is correct.

	// And the leftover dir is STILL present (the daemon's terminal
	// cleanup hook only fires when the workflow's current isolation
	// is worktree).
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("post-run leftover worktree should still exist: %v", statErr)
	}
}

// setupGitRoot creates a temporary directory, initialises it as a
// git repo with one commit, and returns the symlink-resolved path.
func setupGitRoot(t *testing.T) string {
	t.Helper()
	if _, err := lookupGit(); err != nil {
		t.Skip("git not installed; daemon update tests skipped")
	}
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	mustRunGit(t, dir, "init", "--initial-branch=main")
	mustRunGit(t, dir, "config", "user.email", "test@autosk.local")
	mustRunGit(t, dir, "config", "user.name", "autosk test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", "README.md")
	mustRunGit(t, dir, "commit", "-m", "init")
	return dir
}

// lookupGit is a small shim around exec.LookPath that the test file
// imports so we don't pull in `os/exec` at the top level (the helpers
// are already imported in worktree_test.go and we deliberately stay
// dependency-light here).
func lookupGit() (string, error) {
	if _, err := os.Stat("/usr/bin/git"); err == nil {
		return "/usr/bin/git", nil
	}
	if _, err := os.Stat("/usr/local/bin/git"); err == nil {
		return "/usr/local/bin/git", nil
	}
	return "", os.ErrNotExist
}
