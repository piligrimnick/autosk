package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/comments"
	"autosk/internal/render"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// newDoneCmd / newCancelCmd: direct status setters. They bypass the
// workflow engine (see plan P9): humans can close their own tasks via
// these verbs, and they also clear current_step_id so the CHECK in
// 001_init.sql stays satisfied.
func newDoneCmd() *cobra.Command {
	return statusSetterCmd("done", "Mark a task done", store.StatusDone, nil)
}

func newCancelCmd() *cobra.Command {
	return statusSetterCmd("cancel", "Mark a task cancelled", store.StatusCancelled, nil)
}

// newReopenCmd: status → new, but only from done|cancelled. Also clears
// current_step_id so the task's CHECK constraint stays valid; workflow_id
// is preserved for audit (plan §7).
func newReopenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reopen <id>",
		Short: "Reopen a done/cancelled task (→ new)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			cur, err := s.GetTask(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if cur.Status != store.StatusDone && cur.Status != store.StatusCancelled {
				return fmt.Errorf("cannot reopen task in status %q (only done|cancelled)", cur.Status)
			}
			newStatus := store.StatusNew
			empty := ""
			t, err := s.UpdateTask(cmd.Context(), args[0], store.TaskPatch{
				Status:        &newStatus,
				CurrentStepID: &empty,
			})
			if err != nil {
				return err
			}
			commitWrite(cmd.Context(), s, "reopen "+args[0])
			return emit(t)
		},
	}
}

// statusSetterCmd builds a `<verb> <id>` command that sets status.
// If allowedFrom is non-nil, the current status must be a key in the map.
//
// Terminal verbs (done/cancelled) also clear current_step_id so the SQL
// CHECK on `tasks` stays satisfied when the task was in a workflow.
func statusSetterCmd(use, short string, target store.Status, allowedFrom map[store.Status]bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			if allowedFrom != nil {
				cur, err := s.GetTask(cmd.Context(), args[0])
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("task not found: %s", args[0])
					}
					return err
				}
				if !allowedFrom[cur.Status] {
					return fmt.Errorf("cannot %s task in status %q (use `autosk update <id> --status ...` to force)", use, cur.Status)
				}
			}

			patch := store.TaskPatch{Status: &target}
			if target == store.StatusDone || target == store.StatusCancelled {
				empty := ""
				patch.CurrentStepID = &empty
			}
			t, err := s.UpdateTask(cmd.Context(), args[0], patch)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			commitWrite(cmd.Context(), s, use+" "+args[0])
			// Cleanup the per-task worktree on terminal status. Best-
			// effort: failures are surfaced as a warning + agent
			// comment so the status flip is never masked by a stuck
			// `git worktree remove`.
			if target == store.StatusDone || target == store.StatusCancelled {
				cleanupWorktreeOnTerminal(cmd.Context(), s, t)
			}
			return emit(t)
		},
	}
}

// cleanupWorktreeOnTerminal removes the per-task worktree directory
// when a CLI verb closes a task that was running under an isolated
// workflow. Branch is preserved. All failures are surfaced as a
// warning + agent comment — a stuck `git worktree remove` must never
// mask a successful done/cancel.
//
// Every silent-skip branch surfaces a stderr warning: the function's
// callers (`autosk done` / `cancel`) have already committed the
// status flip, so a silent bailout here would leak the worktree with
// no breadcrumb. The user always gets some signal.
func cleanupWorktreeOnTerminal(ctx context.Context, s store.Store, t store.Task) {
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
		// We don't know whether the workflow was isolated. Surface the
		// lookup failure on stderr so a transient DB blip doesn't
		// silently leak the worktree.
		fmt.Fprintf(os.Stderr, "warning: worktree cleanup for %s skipped: load workflow %s: %v\n",
			t.ID, t.WorkflowID, err)
		return
	}
	if wf.Isolation != workflow.IsolationWorktree {
		return
	}
	root, perr := projectRootFromCwd()
	if perr != nil {
		// Symmetric to the wfs.GetByID branch: don't silently leak the
		// worktree just because cwd resolution wobbled (moved cwd,
		// broken symlink, EACCES on .autosk parent, …).
		fmt.Fprintf(os.Stderr, "warning: worktree cleanup for %s skipped: resolve project root: %v\n",
			t.ID, perr)
		return
	}
	wtMgr := worktree.NewManager()
	_, werr := wtMgr.OnTerminal(ctx, root, t.ID)
	if werr == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: worktree cleanup for %s failed: %v\n", t.ID, werr)
	// Surface the failure as an agent comment so it survives
	// past the CLI invocation. Author the comment under the
	// seeded `human` agent — we don't want a synthetic
	// `autosk` agent row (the package resolver would reject
	// EnsureByName on unknown names).
	//
	// The comment write uses a fresh derived ctx in case the
	// OnTerminal failure was a deadline exceedance under the
	// caller's ctx — same shape as the executor's breadcrumb
	// path. cmd.Context() already respects Ctrl+C, so callers
	// that walk away mid-cleanup still get torn down.
	crumbCtx, crumbCancel := context.WithTimeout(ctx, 5*time.Second)
	defer crumbCancel()
	cs := comments.New(dl.DB())
	if humanAg, aerr := ag.GetByName(crumbCtx, agent.HumanAgentName); aerr == nil {
		_, _ = cs.Add(crumbCtx, t.ID, humanAg.ID,
			fmt.Sprintf("worktree cleanup failed on %s: %v", t.Status, werr))
	}
}

// newUpdateCmd is the generic field-update verb.
func newUpdateCmd() *cobra.Command {
	var (
		title       string
		description string
		statusFlag  string
		priority    int
	)
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update one or more fields on a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			var patch store.TaskPatch
			if cmd.Flags().Changed("title") {
				patch.Title = &title
			}
			if cmd.Flags().Changed("description") {
				patch.Description = &description
			}
			if cmd.Flags().Changed("status") {
				st := store.Status(statusFlag)
				if !st.Valid() {
					return fmt.Errorf("invalid status %q (valid: new, in_workflow, human_feedback, done, cancelled)", statusFlag)
				}
				// Reject status edits on workflow-tracked tasks: those
				// transitions belong to the daemon / step engine (§10 risks).
				cur, gerr := s.GetTask(cmd.Context(), args[0])
				if gerr != nil {
					if errors.Is(gerr, store.ErrNotFound) {
						return fmt.Errorf("task not found: %s", args[0])
					}
					return gerr
				}
				if cur.Status == store.StatusInWorkflow {
					return fmt.Errorf("refusing to --status on an `in_workflow` task; use `autosk done|cancel` or let the workflow engine advance it")
				}
				patch.Status = &st
			}
			if cmd.Flags().Changed("priority") {
				patch.Priority = &priority
			}
			if patch.IsEmpty() {
				return errors.New("nothing to update (pass --title/--description/--status/--priority)")
			}

			t, err := s.UpdateTask(cmd.Context(), args[0], patch)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			commitWrite(cmd.Context(), s, "update "+args[0])
			return emit(t)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "new description")
	cmd.Flags().StringVar(&statusFlag, "status", "", "new status (new|in_workflow|human_feedback|done|cancelled)")
	cmd.Flags().IntVarP(&priority, "priority", "p", 0, "new priority (0..3)")
	return cmd
}

// emit prints a single task post-mutation. Honors --json and --quiet.
func emit(t store.Task) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t)
	}
	return render.Task(os.Stdout, t)
}
