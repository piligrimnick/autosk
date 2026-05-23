package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"autosk/internal/store"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// ---- fake worktree.Manager ----------------------------------------------

// fakeMgr is a worktree.Manager test stub for UpdateIsolation. It
// records every Ensure / OnTerminal call so the test can assert the
// per-task ordering, and it lets the test inject a failure on a
// specific task to exercise the rollback path.
type fakeMgr struct {
	mu              sync.Mutex
	ensureCalls     []string // task ids in call order
	onTerminalCalls []string
	failOnTask      string // when non-empty, Ensure errors on this task
}

func (m *fakeMgr) Ensure(ctx context.Context, projectRoot, taskID, baseRef string) (worktree.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCalls = append(m.ensureCalls, taskID)
	if m.failOnTask != "" && taskID == m.failOnTask {
		return worktree.Result{}, fmt.Errorf("synthetic ensure failure: %s", taskID)
	}
	path, _ := worktree.PathFor(projectRoot, taskID)
	return worktree.Result{
		Path:   path,
		Branch: worktree.BranchFor(taskID),
	}, nil
}

func (m *fakeMgr) OnTerminal(ctx context.Context, projectRoot, taskID string) (worktree.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onTerminalCalls = append(m.onTerminalCalls, taskID)
	path, _ := worktree.PathFor(projectRoot, taskID)
	return worktree.Result{Path: path, Branch: worktree.BranchFor(taskID), Existed: true}, nil
}

func (m *fakeMgr) Verify(ctx context.Context, projectRoot, taskID string) error { return nil }

// ---- fixture helpers ----------------------------------------------------

// newWorkflowWithIsolation creates a non-synthetic workflow with the
// given isolation mode and returns the materialised row.
func newWorkflowWithIsolation(t *testing.T, wf *workflow.Store, name string, mode workflow.IsolationMode) workflow.Workflow {
	t.Helper()
	body := fmt.Sprintf(`{
		"name": %q,
		"first_step": "dev",
		"isolation": %q,
		"steps": {
			"dev": {"agent": {"name": "developer"}, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`, name, string(mode))
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseReader: %v", err)
	}
	w, err := wf.Create(context.Background(), def, false)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return w
}

// ---- tests --------------------------------------------------------------

// TestUpdateIsolation_NoopReturnsZeroSideEffects pins that setting
// the same value is a no-op: Noop=true, no Ensure / OnTerminal calls,
// no DB write side-effects.
func TestUpdateIsolation_NoopReturnsZeroSideEffects(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "no-op", workflow.IsolationNone)
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationNone, workflow.UpdateIsolationOpts{
		Worktrees: fake,
	})
	if err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	if !rep.Noop {
		t.Fatalf("Noop: false (want true) report=%+v", rep)
	}
	if len(fake.ensureCalls) != 0 || len(fake.onTerminalCalls) != 0 {
		t.Fatalf("no-op should not call manager: ensures=%v terminals=%v",
			fake.ensureCalls, fake.onTerminalCalls)
	}
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationNone {
		t.Fatalf("post-noop isolation: %q (want none)", got.Isolation)
	}
}

// TestUpdateIsolation_NoneToWorktree_NoTasks: column flip succeeds
// without --force, no Ensure calls.
func TestUpdateIsolation_NoneToWorktree_NoTasks(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "flip-nowf", workflow.IsolationNone)
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
		Worktrees: fake,
	})
	if err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	if rep.Noop {
		t.Fatalf("Noop unexpectedly true: %+v", rep)
	}
	if rep.From != workflow.IsolationNone || rep.To != workflow.IsolationWorktree {
		t.Fatalf("From/To: %q→%q", rep.From, rep.To)
	}
	if len(fake.ensureCalls) != 0 {
		t.Fatalf("no tasks: Ensure must not fire: %v", fake.ensureCalls)
	}
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationWorktree {
		t.Fatalf("post-flip isolation: %q", got.Isolation)
	}
}

// TestUpdateIsolation_RefusesWithNonTerminal_NoForce: refuses with
// the offending ids in the report and ErrNonTerminalTasks sentinel.
func TestUpdateIsolation_RefusesWithNonTerminal_NoForce(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "guarded", workflow.IsolationNone)
	tk1, _ := dl.CreateTask(ctx, store.Task{Title: "t1", Status: store.StatusNew, Priority: 2})
	tk2, _ := dl.CreateTask(ctx, store.Task{Title: "t2", Status: store.StatusHuman, Priority: 2,
		CurrentStepID: w.Steps[0].ID})
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id = ?`, w.ID, tk1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id = ?`, w.ID, tk2.ID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
		Worktrees: fake,
	})
	if !errors.Is(err, workflow.ErrNonTerminalTasks) {
		t.Fatalf("want ErrNonTerminalTasks, got %v", err)
	}
	if len(rep.NonTerminalTasks) != 2 {
		t.Fatalf("NonTerminalTasks: %v", rep.NonTerminalTasks)
	}
	if len(fake.ensureCalls) != 0 {
		t.Fatalf("Ensure must not fire on refusal: %v", fake.ensureCalls)
	}
	// Column unchanged.
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationNone {
		t.Fatalf("post-refusal isolation: %q (want unchanged)", got.Isolation)
	}
}

// TestUpdateIsolation_NoneToWorktree_Force_HappyPath: --force on a
// workflow with non-terminal tasks calls Ensure for each and flips
// the column.
func TestUpdateIsolation_NoneToWorktree_Force_HappyPath(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "force-up", workflow.IsolationNone)
	tk1, _ := dl.CreateTask(ctx, store.Task{Title: "t1", Status: store.StatusNew, Priority: 2})
	tk2, _ := dl.CreateTask(ctx, store.Task{Title: "t2", Status: store.StatusNew, Priority: 2})
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id IN (?, ?)`, w.ID, tk1.ID, tk2.ID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
		Force:       true,
		ProjectRoot: "/tmp/fake-project",
		Worktrees:   fake,
	})
	if err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	if len(fake.ensureCalls) != 2 {
		t.Fatalf("Ensure calls: %v (want 2)", fake.ensureCalls)
	}
	if len(rep.EnsuredTasks) != 2 {
		t.Fatalf("EnsuredTasks: %v", rep.EnsuredTasks)
	}
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationWorktree {
		t.Fatalf("post-flip isolation: %q", got.Isolation)
	}
}

// TestUpdateIsolation_NoneToWorktree_Force_AtomicRollback: a mid-run
// Ensure failure unwinds every prior Ensure via OnTerminal AND leaves
// the column unchanged.
func TestUpdateIsolation_NoneToWorktree_Force_AtomicRollback(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "rollback", workflow.IsolationNone)
	var tids []string
	for i := 0; i < 3; i++ {
		tk, _ := dl.CreateTask(ctx, store.Task{
			Title: fmt.Sprintf("t%d", i), Status: store.StatusNew, Priority: 2,
		})
		tids = append(tids, tk.ID)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ?`, w.ID); err != nil {
		t.Fatal(err)
	}
	// Order is deterministic (id ASC); fail on the third task so two
	// prior Ensures must roll back.
	fake := &fakeMgr{failOnTask: tids[len(tids)-1]}
	// Sort tids so we know exactly which one is last in the listNonTerminal
	// query.
	sortedTIDs := append([]string(nil), tids...)
	// store.CreateTask mints lexicographically increasing-ish ids;
	// sort the slice to mirror the SELECT ... ORDER BY id ASC.
	for i := 1; i < len(sortedTIDs); i++ {
		for j := i; j > 0 && sortedTIDs[j-1] > sortedTIDs[j]; j-- {
			sortedTIDs[j-1], sortedTIDs[j] = sortedTIDs[j], sortedTIDs[j-1]
		}
	}
	fake.failOnTask = sortedTIDs[2]

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
		Force:       true,
		ProjectRoot: "/tmp/fake-project",
		Worktrees:   fake,
	})
	if !errors.Is(err, workflow.ErrEnsureFailed) {
		t.Fatalf("want ErrEnsureFailed, got %v", err)
	}
	if rep.FailedTask != fake.failOnTask {
		t.Fatalf("FailedTask: %q (want %q)", rep.FailedTask, fake.failOnTask)
	}
	if len(rep.RolledBackEnsures) != 2 {
		t.Fatalf("RolledBackEnsures: %v (want 2)", rep.RolledBackEnsures)
	}
	if len(fake.onTerminalCalls) != 2 {
		t.Fatalf("OnTerminal calls: %v (want 2 rollbacks)", fake.onTerminalCalls)
	}
	// Column unchanged.
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationNone {
		t.Fatalf("post-rollback isolation: %q (want unchanged none)", got.Isolation)
	}
}

// TestUpdateIsolation_WorktreeToNone_NoTasks: column flip succeeds
// without --force, no leftover list.
func TestUpdateIsolation_WorktreeToNone_NoTasks(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "down-clean", workflow.IsolationWorktree)
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationNone, workflow.UpdateIsolationOpts{
		Worktrees: fake,
	})
	if err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	if len(rep.LeftoverWorktrees) != 0 {
		t.Fatalf("LeftoverWorktrees: %v (want empty)", rep.LeftoverWorktrees)
	}
	if len(fake.onTerminalCalls) != 0 {
		t.Fatalf("OnTerminal must not fire on worktree→none: %v", fake.onTerminalCalls)
	}
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationNone {
		t.Fatalf("post-flip isolation: %q", got.Isolation)
	}
}

// TestUpdateIsolation_WorktreeToNone_Force_ReportsLeftovers: flip
// happens; leftover paths are surfaced; the manager is NOT called
// for any cleanup (worktrees deliberately survive).
func TestUpdateIsolation_WorktreeToNone_Force_ReportsLeftovers(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "down-leftovers", workflow.IsolationWorktree)
	tk, _ := dl.CreateTask(ctx, store.Task{Title: "t", Status: store.StatusNew, Priority: 2})
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id = ?`, w.ID, tk.ID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationNone, workflow.UpdateIsolationOpts{
		Force:       true,
		ProjectRoot: "/tmp/fake-project",
		Worktrees:   fake,
	})
	if err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	if len(rep.LeftoverWorktrees) != 1 || rep.LeftoverWorktrees[0].TaskID != tk.ID {
		t.Fatalf("LeftoverWorktrees: %v", rep.LeftoverWorktrees)
	}
	if len(fake.onTerminalCalls) != 0 {
		t.Fatalf("OnTerminal must NOT fire on worktree→none --force: %v", fake.onTerminalCalls)
	}
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationNone {
		t.Fatalf("post-flip isolation: %q", got.Isolation)
	}
}

// TestUpdateIsolation_DryRun_NoMutation: no Ensure, no column flip,
// no leftover OnTerminal regardless of Force.
func TestUpdateIsolation_DryRun_NoMutation(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "dry", workflow.IsolationNone)
	tk, _ := dl.CreateTask(ctx, store.Task{Title: "t", Status: store.StatusNew, Priority: 2})
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id = ?`, w.ID, tk.ID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMgr{}

	rep, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
		Force:       true,
		DryRun:      true,
		ProjectRoot: "/tmp/fake-project",
		Worktrees:   fake,
	})
	if err != nil {
		t.Fatalf("UpdateIsolation: %v", err)
	}
	if !rep.DryRun {
		t.Fatalf("DryRun: false (want true)")
	}
	if len(fake.ensureCalls) != 0 {
		t.Fatalf("DryRun: Ensure must not fire: %v", fake.ensureCalls)
	}
	if len(rep.EnsuredTasks) != 1 {
		t.Fatalf("DryRun should still PLAN: EnsuredTasks=%v", rep.EnsuredTasks)
	}
	got, _ := wf.GetByName(ctx, w.Name)
	if got.Isolation != workflow.IsolationNone {
		t.Fatalf("DryRun: isolation changed (want unchanged): %q", got.Isolation)
	}
}

// TestUpdateIsolation_SyntheticAlwaysRejected pins that synthetic
// rows refuse the flip regardless of --force.
func TestUpdateIsolation_SyntheticAlwaysRejected(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	s, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		_, err := wf.UpdateIsolation(ctx, s.Name, workflow.IsolationWorktree, workflow.UpdateIsolationOpts{
			Force: force,
		})
		if !errors.Is(err, workflow.ErrSyntheticImmutable) {
			t.Errorf("force=%v: want ErrSyntheticImmutable, got %v", force, err)
		}
	}
}

// TestUpdateIsolation_NotFound returns ErrNotFound for unknown names.
func TestUpdateIsolation_NotFound(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	_, err := wf.UpdateIsolation(ctx, "no-such-workflow", workflow.IsolationWorktree,
		workflow.UpdateIsolationOpts{})
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestUpdateIsolation_InvalidMode rejects garbage target values.
func TestUpdateIsolation_InvalidMode(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w := newWorkflowWithIsolation(t, wf, "bad-mode", workflow.IsolationNone)
	_, err := wf.UpdateIsolation(ctx, w.Name, workflow.IsolationMode("garbage"), workflow.UpdateIsolationOpts{})
	if err == nil || !strings.Contains(err.Error(), "invalid isolation mode") {
		t.Fatalf("want invalid mode error, got %v", err)
	}
}
