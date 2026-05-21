package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/render"
	"autosk/internal/store"
	"autosk/internal/tasksvc"
)

// newDoneCmd / newCancelCmd / newReopenCmd / newUpdateCmd: thin
// adapters over the internal/tasksvc package. tasksvc owns the actual
// store mutations (status patch + current_step_id cleanup + best-effort
// worktree cleanup); the cobra layer here only wires the CLI surface
// (args, dolt-commit audit, output rendering).
//
// The lazy TUI calls the same tasksvc verbs through
// internal/lazy/datasource so `autosk done <id>` and `d` in lazy go
// through identical code paths.
func newDoneCmd() *cobra.Command {
	return statusSetterCmd("done", "Mark a task done", store.StatusDone)
}

func newCancelCmd() *cobra.Command {
	return statusSetterCmd("cancel", "Mark a task cancelled", store.StatusCancelled)
}

// newReopenCmd: status → new, but only from done|cancelled. tasksvc.Reopen
// enforces the precondition AND clears current_step_id so the CHECK in
// 001_init.sql stays satisfied; workflow_id is preserved for audit (plan §7).
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
			t, err := tasksvc.Reopen(cmd.Context(), s, args[0])
			if err != nil {
				return err
			}
			commitWrite(cmd.Context(), s, "reopen "+args[0])
			return emit(t)
		},
	}
}

// statusSetterCmd builds a `<verb> <id>` command that sets status via
// tasksvc. Terminal verbs (done/cancelled) also get the worktree
// cleanup from tasksvc when the task ran under an isolated workflow.
func statusSetterCmd(use, short string, target store.Status) *cobra.Command {
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

			opts := tasksvc.Options{
				Warn: func(format string, a ...any) {
					fmt.Fprintf(os.Stderr, "warning: "+format+"\n", a...)
				},
			}
			if root, perr := projectRootFromCwd(); perr == nil {
				opts.ProjectRoot = root
			} else {
				// Symmetric to tasksvc's other skip branches: don't
				// silently leak the worktree just because cwd
				// resolution wobbled. Surface the lookup failure on
				// stderr — the status flip below still proceeds.
				fmt.Fprintf(os.Stderr, "warning: worktree cleanup for %s skipped: resolve project root: %v\n", args[0], perr)
			}

			var t store.Task
			switch target {
			case store.StatusDone:
				t, err = tasksvc.Done(cmd.Context(), s, args[0], opts)
			case store.StatusCancelled:
				t, err = tasksvc.Cancel(cmd.Context(), s, args[0], opts)
			default:
				return fmt.Errorf("statusSetterCmd: unsupported target %q", target)
			}
			if err != nil {
				return err
			}
			commitWrite(cmd.Context(), s, use+" "+args[0])
			return emit(t)
		},
	}
}

// newUpdateCmd is the generic field-update verb. Status edits flow
// through tasksvc.SetStatus so the same rules (no in_workflow source/target,
// current_step_id cleanup, worktree cleanup on terminal targets) apply.
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

			var (
				patch       store.TaskPatch
				statusPatch *store.Status
			)
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
				statusPatch = &st
			}
			if cmd.Flags().Changed("priority") {
				patch.Priority = &priority
			}
			if patch.IsEmpty() && statusPatch == nil {
				return errors.New("nothing to update (pass --title/--description/--status/--priority)")
			}

			// Apply non-status fields first via the generic store path
			// so a partial title-only update doesn't go through tasksvc.
			var t store.Task
			if !patch.IsEmpty() {
				t, err = s.UpdateTask(cmd.Context(), args[0], patch)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("task not found: %s", args[0])
					}
					return err
				}
			}
			if statusPatch != nil {
				opts := tasksvc.Options{
					Warn: func(format string, a ...any) {
						fmt.Fprintf(os.Stderr, "warning: "+format+"\n", a...)
					},
				}
				if root, perr := projectRootFromCwd(); perr == nil {
					opts.ProjectRoot = root
				}
				t, err = tasksvc.SetStatus(cmd.Context(), s, args[0], *statusPatch, opts)
				if err != nil {
					return err
				}
			}
			commitWrite(cmd.Context(), s, "update "+args[0])
			return emit(t)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "new description")
	cmd.Flags().StringVar(&statusFlag, "status", "", "new status (new|human_feedback|done|cancelled)")
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
