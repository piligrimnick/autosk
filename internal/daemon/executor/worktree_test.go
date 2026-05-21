package executor_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// makeIsolatedFixture stands up an exec fixture against a project root
// that's a real git repo, plus an isolated workflow `iso-feature` whose
// dev step transitions straight to done. Returns the workflow, the dev
// step id, and a cleanup hook.
func makeIsolatedFixture(t *testing.T) (*execFixture, workflow.Workflow, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; worktree-executor tests skipped")
	}
	fx := newExecFixture(t)
	// Replace ProjectRoot with a fresh git repo (the default fixture's
	// project root isn't a git repo).
	gitRoot := t.TempDir()
	gitRoot, _ = filepath.EvalSymlinks(gitRoot)
	mustRunGit(t, gitRoot, "init", "--initial-branch=main")
	mustRunGit(t, gitRoot, "config", "user.email", "test@autosk.local")
	mustRunGit(t, gitRoot, "config", "user.name", "autosk test")
	if err := os.WriteFile(filepath.Join(gitRoot, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, gitRoot, "add", "README.md")
	mustRunGit(t, gitRoot, "commit", "-m", "init")
	// Isolate $HOME so the worktree allocator lands somewhere t.Cleanup
	// can sweep.
	t.Setenv("HOME", t.TempDir())
	fx.cfg.ProjectRoot = gitRoot
	fx.cfg.DBPath = filepath.Join(gitRoot, ".autosk", "db")

	body := `{
		"name": "iso-feature",
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
	wf, err := fx.deps.Workflows.Create(context.Background(), def, false)
	if err != nil {
		t.Fatal(err)
	}
	var devID string
	for _, s := range wf.Steps {
		if s.Name == "dev" {
			devID = s.ID
		}
	}
	if devID == "" {
		t.Fatal("dev step id not resolved")
	}
	return fx, wf, devID
}

func mustRunGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestRun_Worktree_HappyPath_RemovesDirOnDone runs a fake-pi-backed
// step inside an isolated workflow that signals done; asserts the
// worktree is gone after Run returns and the branch survives.
func TestRun_Worktree_HappyPath_RemovesDirOnDone(t *testing.T) {
	fx, wf, devID := makeIsolatedFixture(t)
	defer fx.close()
	ctx := context.Background()

	// Pre-allocate the worktree (CLI normally does this on create/enroll).
	wtMgr := worktree.NewManager()
	if _, err := wtMgr.Ensure(ctx, fx.cfg.ProjectRoot, "as-iso01", ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	wtPath, _ := worktree.PathFor(fx.cfg.ProjectRoot, "as-iso01")

	tk, err := fx.ts.CreateTask(ctx, store.Task{
		ID:            "as-iso01",
		Title:         "isolated",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: devID})
	if err != nil {
		t.Fatal(err)
	}

	// Capture the pi.Opts that the executor passes so we can assert
	// cwd / Env reflect the worktree.
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
		t.Fatalf("executor cwd should be worktree path: got %q want %q", captured.Cwd, wtPath)
	}
	if !envHasKV(captured.Env, "AUTOSK_DB", fx.cfg.DBPath) {
		t.Fatalf("expected AUTOSK_DB=%q in env, got %v", fx.cfg.DBPath, captured.Env)
	}
	// Worktree directory should be gone after Run.
	if _, statErr := os.Stat(wtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree dir should be removed on terminal done, stat err=%v", statErr)
	}
	// Branch should survive.
	if out, err := runOut("git", "-C", fx.cfg.ProjectRoot, "rev-parse", "--verify", "refs/heads/autosk/as-iso01"); err != nil {
		t.Fatalf("branch should survive: %v: %s", err, out)
	}
}

// TestRun_Worktree_Missing_AutoRecover asserts that a missing
// worktree at run start is transparently re-allocated by the
// executor (Verify → ErrWorktreeMissing → Ensure → continue) so the
// task is NOT parked. The branch is the load-bearing piece and is
// preserved across terminal cleanup; re-creating the dir on the
// existing branch is safe.
func TestRun_Worktree_Missing_AutoRecover(t *testing.T) {
	fx, wf, devID := makeIsolatedFixture(t)
	defer fx.close()
	ctx := context.Background()

	// NO pre-allocation. The worktree will be missing when the
	// executor's pre-flight runs — but the executor must Ensure it
	// and proceed instead of failing the run.
	wtPath, _ := worktree.PathFor(fx.cfg.ProjectRoot, "as-iso02")
	tk, err := fx.ts.CreateTask(ctx, store.Task{
		ID:            "as-iso02",
		Title:         "missing wt",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	deps.Worktree = worktree.NewManager()
	ex := executor.New(deps, factory, fx.cfg)
	if err := ex.Run(ctx, run.JobID); err != nil {
		t.Fatalf("Run should auto-recover from missing worktree: %v", err)
	}
	if captured.Cwd != wtPath {
		t.Fatalf("executor cwd should be the (re-allocated) worktree path: got %q want %q", captured.Cwd, wtPath)
	}
	// Branch should exist (Ensure created it from HEAD).
	if out, err := runOut("git", "-C", fx.cfg.ProjectRoot, "rev-parse", "--verify", "refs/heads/autosk/as-iso02"); err != nil {
		t.Fatalf("branch should have been created by auto-Ensure: %v: %s", err, out)
	}
	// After a successful `done` transition the terminal cleanup hook
	// reaps the dir again — same contract as the happy-path test.
	if _, statErr := os.Stat(wtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree dir should be removed on terminal done, stat err=%v", statErr)
	}
	runRow, _ := fx.deps.Runs.GetRun(ctx, run.JobID)
	if runRow.Status != runstore.StatusDone {
		t.Fatalf("run.Status: %s (want done)", runRow.Status)
	}
	tkAfter, _ := fx.ts.GetTask(ctx, tk.ID)
	if tkAfter.Status != store.StatusDone {
		t.Fatalf("task should be done, got %s", tkAfter.Status)
	}
}

// TestRun_Worktree_Stranded_ParksTask asserts that a stranded
// worktree (directory exists at the canonical path but `.git` no
// longer resolves to the project's gitdir) still fails the run with
// worktree_stranded and parks the task — auto-recovery only covers
// the simple "directory absent" case.
func TestRun_Worktree_Stranded_ParksTask(t *testing.T) {
	fx, wf, devID := makeIsolatedFixture(t)
	defer fx.close()
	ctx := context.Background()

	// Plant a non-git directory at the expected worktree path. Verify
	// stat-succeeds, then `git rev-parse --git-common-dir` from inside
	// fails → ErrWorktreeStranded.
	wtPath, _ := worktree.PathFor(fx.cfg.ProjectRoot, "as-iso04")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	tk, err := fx.ts.CreateTask(ctx, store.Task{
		ID:            "as-iso04",
		Title:         "stranded wt",
		Status:        store.StatusWork,
		Priority:      2,
		WorkflowID:    wf.ID,
		CurrentStepID: devID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: devID})
	if err != nil {
		t.Fatal(err)
	}

	stub := newStub()
	deps := fx.deps
	deps.Worktree = worktree.NewManager()
	ex := executor.New(deps, stubFactory(stub), fx.cfg)
	err = ex.Run(ctx, run.JobID)
	if err == nil {
		t.Fatal("expected worktree_stranded failure")
	}
	if !strings.Contains(err.Error(), "worktree_stranded") {
		t.Fatalf("err %q does not mention worktree_stranded", err.Error())
	}
	runRow, _ := fx.deps.Runs.GetRun(ctx, run.JobID)
	if runRow.Status != runstore.StatusFailed {
		t.Fatalf("run.Status: %s (want failed)", runRow.Status)
	}
	if !strings.Contains(runRow.Error, "worktree_stranded") {
		t.Fatalf("run.Error: %q (want worktree_stranded)", runRow.Error)
	}
	tkAfter, _ := fx.ts.GetTask(ctx, tk.ID)
	if tkAfter.Status != store.StatusHuman {
		t.Fatalf("task should be parked, got %s", tkAfter.Status)
	}
}

// TestRun_Worktree_Concurrent_TwoTasksNoInterference is the WT6 soak
// test: two tasks in the same isolated workflow execute concurrently
// against the same project. Each captures its own cwd; the two paths
// MUST differ and the worktree dirs MUST be reaped independently.
func TestRun_Worktree_Concurrent_TwoTasksNoInterference(t *testing.T) {
	fx, wf, devID := makeIsolatedFixture(t)
	defer fx.close()
	ctx := context.Background()

	wtMgr := worktree.NewManager()
	if _, err := wtMgr.Ensure(ctx, fx.cfg.ProjectRoot, "as-cur01", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := wtMgr.Ensure(ctx, fx.cfg.ProjectRoot, "as-cur02", ""); err != nil {
		t.Fatal(err)
	}

	makeTaskRun := func(id string) (string, string) {
		tk, err := fx.ts.CreateTask(ctx, store.Task{
			ID:            id,
			Title:         "concurrent " + id,
			Status:        store.StatusWork,
			Priority:      2,
			WorkflowID:    wf.ID,
			CurrentStepID: devID,
		})
		if err != nil {
			t.Fatal(err)
		}
		r, err := fx.deps.Runs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: devID})
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID, r.JobID
	}

	idA, jobA := makeTaskRun("as-cur01")
	idB, jobB := makeTaskRun("as-cur02")

	// Per-job stubs that emit `done` on first prompt. We use a custom
	// factory that captures the cwd for each job in a map keyed by
	// job id (extracted from stubRunner pointer identity since pi.Opts
	// doesn't carry a job id).
	var (
		captureMu sync.Mutex
		cwdByJob  = map[string]string{}
	)
	makeStub := func(taskID, jobID string) *stubRunner {
		s := newStub()
		s.onPrompt = func(prompt string, attempt int) {
			if attempt == 1 {
				if _, err := fx.deps.Signals.Emit(ctx, taskID, "done"); err != nil {
					t.Errorf("Emit %s: %v", taskID, err)
				}
			}
		}
		return s
	}
	stubA := makeStub(idA, jobA)
	stubB := makeStub(idB, jobB)

	// Use the runner-pointer identity to demux which stub the factory
	// was just asked for.
	var nextStub func() (*stubRunner, string)
	{
		queue := []struct {
			s  *stubRunner
			id string
		}{
			{stubA, jobA},
			{stubB, jobB},
		}
		var qMu sync.Mutex
		nextStub = func() (*stubRunner, string) {
			qMu.Lock()
			defer qMu.Unlock()
			if len(queue) == 0 {
				return nil, ""
			}
			x := queue[0]
			queue = queue[1:]
			return x.s, x.id
		}
	}
	factory := func(_ context.Context, opts pi.Opts) (executor.PiRunner, error) {
		s, jobID := nextStub()
		if s == nil {
			t.Fatalf("factory called more times than expected")
		}
		captureMu.Lock()
		cwdByJob[jobID] = opts.Cwd
		captureMu.Unlock()
		return s, nil
	}

	deps := fx.deps
	deps.Worktree = wtMgr
	ex := executor.New(deps, factory, fx.cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() { defer wg.Done(); errA = ex.Run(ctx, jobA) }()
	go func() { defer wg.Done(); errB = ex.Run(ctx, jobB) }()
	wg.Wait()
	if errA != nil {
		t.Fatalf("Run A: %v", errA)
	}
	if errB != nil {
		t.Fatalf("Run B: %v", errB)
	}

	// Each run got its own cwd.
	captureMu.Lock()
	a, b := cwdByJob[jobA], cwdByJob[jobB]
	captureMu.Unlock()
	if a == "" || b == "" {
		t.Fatalf("missing capture: A=%q B=%q", a, b)
	}
	if a == b {
		t.Fatalf("two concurrent runs got the same cwd: %s", a)
	}
	// Both worktree directories gone after Run.
	for _, p := range []string{a, b} {
		if _, statErr := os.Stat(p); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("worktree %s should be removed, stat err=%v", p, statErr)
		}
	}
	// Both branches survive.
	for _, id := range []string{idA, idB} {
		if out, err := runOut("git", "-C", fx.cfg.ProjectRoot, "rev-parse", "--verify", "refs/heads/autosk/"+id); err != nil {
			t.Fatalf("branch for %s should survive: %v: %s", id, err, out)
		}
	}
}

// envHasKV returns true if env contains an entry exactly "KEY=value".
func envHasKV(env []string, key, value string) bool {
	want := key + "=" + value
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func runOut(name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	return c.CombinedOutput()
}
