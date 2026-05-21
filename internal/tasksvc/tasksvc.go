// Package tasksvc owns the human-driven status transitions on a task:
// Done, Cancel, Reopen, and SetStatus. It is the single place where
// both the CLI (`autosk done|cancel|reopen|update --status`) and the
// lazy TUI (`d`, `x`, `o`) execute these mutations, so that the two
// front ends cannot drift in behaviour.
//
// # Why a separate package
//
// Before this package existed, the CLI verbs in cmd/autosk/status_verbs.go
// cleared current_step_id on terminal status (done|cancel) so the
// CHECK invariant (status='work' ⇔ current_step_id IS NOT NULL)
// stayed satisfied, while lazy's
// datasource.Offline.UpdateStatus did a naive store.UpdateTask(...,
// {Status: &status}) and left current_step_id alone. A task paused in
// human with a non-null current_step_id (the typical
// "workflow kicked back to a human" shape) could be closed via
// `autosk done` but not via lazy's `d` hotkey — the CHECK fired and
// the row was silently rejected. This package collapses the two
// paths.
//
// # Scope
//
// Workflow-engine transitions (step_signal-driven, including
// autosk_step / workflow.step next) are NOT in scope here. Those go
// through internal/workflow.Service; tasksvc is the human override,
// matching the v0.2 plan §9 (humans can close their own tasks).
//
// # Side effects
//
//   - Terminal verbs (Done, Cancel) clear current_step_id so the
//     CHECK invariant (status='work' ⇔ current_step_id IS NOT NULL)
//     stays valid for tasks that were in human or work.
//   - On Done / Cancel, if Options.ProjectRoot is non-empty AND the
//     task ran under an isolated workflow, the per-task worktree
//     directory is removed best-effort. Failures are surfaced via
//     Options.Warn (default: stderr) and recorded as a comment on
//     the task under the seeded `human` agent so the breadcrumb
//     survives past the in-process call.
//   - Reopen rejects tasks that are not in {done, cancel}.
//   - SetStatus rejects status edits on work tasks: those
//     transitions belong to the workflow engine.
package tasksvc

//
//revive:disable:line-length-limit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"autosk/internal/agent"
	"autosk/internal/comments"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// Options controls terminal-cleanup behaviour for Done / Cancel.
//
// ProjectRoot is required to attempt worktree cleanup. Empty = skip;
// callers that don't know the project root (or want to defer cleanup
// to a higher layer) can simply pass an empty Options{}.
//
// Warn is invoked when worktree cleanup fails; the default writes to
// stderr. Callers in TUIs may route Warn through their flash channel.
type Options struct {
	ProjectRoot string
	Warn        func(format string, args ...any)
}

func (o Options) warnf(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}

// Done flips the task to status='done' and clears current_step_id.
// When opts.ProjectRoot is non-empty AND the task ran under an
// isolated workflow, the per-task worktree is also removed best-effort.
func Done(ctx context.Context, s store.Store, id string, opts Options) (store.Task, error) {
	return setTerminal(ctx, s, id, store.StatusDone, opts)
}

// Cancel flips the task to status='cancel' and clears current_step_id.
// Same worktree-cleanup semantics as Done.
func Cancel(ctx context.Context, s store.Store, id string, opts Options) (store.Task, error) {
	return setTerminal(ctx, s, id, store.StatusCancel, opts)
}

// Reopen flips a done|cancel task back to status='new'.
//
// Rejects tasks that aren't currently done|cancel — there is no
// well-defined "reopen" for a new / work / human task
// (use SetStatus or let the workflow engine drive it).
// Workflow_id is preserved for audit (plan §7).
func Reopen(ctx context.Context, s store.Store, id string) (store.Task, error) {
	cur, err := s.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, fmt.Errorf("task not found: %s", id)
		}
		return store.Task{}, err
	}
	if cur.Status != store.StatusDone && cur.Status != store.StatusCancel {
		return store.Task{}, fmt.Errorf("cannot reopen task in status %q (only done|cancel)", cur.Status)
	}
	newStatus := store.StatusNew
	empty := ""
	t, err := s.UpdateTask(ctx, id, store.TaskPatch{
		Status:        &newStatus,
		CurrentStepID: &empty,
	})
	if err != nil {
		return store.Task{}, err
	}
	return t, nil
}

// SetStatus is the generic field-update path (`autosk update --status`).
// Rejects work as a target AND as a source: workflow lifecycle
// belongs to the engine.
//
// Done / Cancelled targets are delegated to Done / Cancel so callers
// get current_step_id cleanup and worktree cleanup for free. A New
// target on a done|cancel task is delegated to Reopen. Any other
// shape (e.g. human) is a plain TaskPatch{Status: …}.
func SetStatus(ctx context.Context, s store.Store, id string, target store.Status, opts Options) (store.Task, error) {
	if !target.Valid() {
		return store.Task{}, fmt.Errorf("invalid status %q", target)
	}
	if target == store.StatusWork {
		return store.Task{}, errors.New("refusing to set status='work' directly; use `autosk enroll` or let the workflow engine advance it")
	}
	cur, err := s.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, fmt.Errorf("task not found: %s", id)
		}
		return store.Task{}, err
	}
	if cur.Status == store.StatusWork {
		return store.Task{}, fmt.Errorf("refusing to change status on a work task; use `autosk done|cancel` or let the workflow engine advance it")
	}
	switch target {
	case store.StatusDone:
		return Done(ctx, s, id, opts)
	case store.StatusCancel:
		return Cancel(ctx, s, id, opts)
	case store.StatusNew:
		// Reopen if coming from a terminal state; otherwise it's a no-op
		// status flip that still has to clear current_step_id when the
		// source was human with a step pointer (CHECK guard).
		if cur.Status == store.StatusDone || cur.Status == store.StatusCancel {
			return Reopen(ctx, s, id)
		}
	}
	patch := store.TaskPatch{Status: &target}
	// For non-terminal targets we still need to clear current_step_id
	// when the source had one — moving from human (which
	// allows a non-null step) to anything other than human
	// or work trips the CHECK otherwise.
	if cur.CurrentStepID != "" && target != store.StatusHuman {
		empty := ""
		patch.CurrentStepID = &empty
	}
	return s.UpdateTask(ctx, id, patch)
}

// setTerminal is the shared body for Done / Cancel.
func setTerminal(ctx context.Context, s store.Store, id string, target store.Status, opts Options) (store.Task, error) {
	empty := ""
	t, err := s.UpdateTask(ctx, id, store.TaskPatch{
		Status:        &target,
		CurrentStepID: &empty,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, fmt.Errorf("task not found: %s", id)
		}
		return store.Task{}, err
	}
	cleanupWorktreeOnTerminal(ctx, s, t, opts)
	return t, nil
}

// cleanupWorktreeOnTerminal removes the per-task worktree directory
// when a human-driven terminal verb closes a task that was running
// under an isolated workflow. Branch is preserved.
//
// Every silent-skip branch surfaces a warning via opts.Warn (default
// stderr): the caller has already committed the status flip, so a
// silent bailout here would leak the worktree with no breadcrumb.
//
// Failures additionally insert a comment on the task under the seeded
// `human` agent so the warning survives past the in-process call.
func cleanupWorktreeOnTerminal(ctx context.Context, s store.Store, t store.Task, opts Options) {
	if t.WorkflowID == "" {
		return
	}
	dl, ok := s.(*doltlite.Store)
	if !ok {
		return
	}
	ag := agent.New(dl.DB())
	wfs := workflow.New(dl.DB(), ag)
	wf, err := wfs.GetByID(ctx, t.WorkflowID)
	if err != nil {
		opts.warnf("worktree cleanup for %s skipped: load workflow %s: %v", t.ID, t.WorkflowID, err)
		return
	}
	if wf.Isolation != workflow.IsolationWorktree {
		return
	}
	if opts.ProjectRoot == "" {
		opts.warnf("worktree cleanup for %s skipped: no project root provided", t.ID)
		return
	}
	wtMgr := worktree.NewManager()
	_, werr := wtMgr.OnTerminal(ctx, opts.ProjectRoot, t.ID)
	if werr == nil {
		return
	}
	opts.warnf("worktree cleanup for %s failed: %v", t.ID, werr)
	// Surface the failure as an agent comment so it survives past
	// the call. Author under the seeded `human` agent — we don't
	// want a synthetic `autosk` agent row.
	//
	// Fresh derived ctx in case OnTerminal failed on deadline
	// exceedance under the caller's ctx — same shape as the
	// executor's breadcrumb path.
	crumbCtx, crumbCancel := context.WithTimeout(ctx, 5*time.Second)
	defer crumbCancel()
	cs := comments.New(dl.DB())
	if humanAg, aerr := ag.GetByName(crumbCtx, agent.HumanAgentName); aerr == nil {
		_, _ = cs.Add(crumbCtx, t.ID, humanAg.ID,
			fmt.Sprintf("worktree cleanup failed on %s: %v", t.Status, werr))
	}
}
