package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
	"autosk/internal/store"
)

// newDoneCmd / newCancelCmd / newReopenCmd / newUpdateCmd: thin clients
// of the daemon's terminal/update verbs. autoskd owns the actual store
// mutations (status patch + current_step_id cleanup + best-effort
// worktree cleanup + the human breadcrumb comment); the cobra layer here
// only wires the CLI surface (args, output rendering).
//
// The lazy TUI calls the same daemon verbs through
// internal/lazy/datasource so `autosk done <id>` and `d` in lazy go
// through identical code paths.
func newDoneCmd() *cobra.Command {
	return statusSetterCmd("done", "Mark a task done", store.StatusDone)
}

func newCancelCmd() *cobra.Command {
	return statusSetterCmd("cancel", "Cancel a task", store.StatusCancel)
}

// newReopenCmd: status → new, but only from done|cancel. The daemon
// enforces the precondition AND clears current_step_id so the CHECK
// invariant (status='work' ⇔ current_step_id IS NOT NULL) stays
// satisfied; workflow_id is preserved for audit (plan §7).
func newReopenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reopen <id>",
		Short: "Reopen a closed task (→ new)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.TaskReopen(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(taskFromWire(t))
		},
	}
}

// statusSetterCmd builds a `<verb> <id>` command that sets status via the
// daemon. Terminal verbs (done/cancel) also trigger the daemon's
// worktree cleanup when the task ran under an isolated workflow.
func statusSetterCmd(use, short string, target store.Status) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			var t rpcclient.Task
			switch target {
			case store.StatusDone:
				t, err = cl.TaskDone(cmd.Context(), args[0])
			case store.StatusCancel:
				t, err = cl.TaskCancel(cmd.Context(), args[0])
			default:
				return fmt.Errorf("statusSetterCmd: unsupported target %q", target)
			}
			if err != nil {
				return err
			}
			return emit(taskFromWire(t))
		},
	}
}

// newUpdateCmd is the generic field-update verb. Status edits flow
// through the daemon's task.update (which applies the same rules: no
// work source/target, current_step_id cleanup, worktree cleanup on
// terminal targets) in a single commit.
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
			var (
				titlePtr  *string
				descPtr   *string
				prioPtr   *int
				statusPtr *string
			)
			if cmd.Flags().Changed("title") {
				titlePtr = &title
			}
			if cmd.Flags().Changed("description") {
				descPtr = &description
			}
			if cmd.Flags().Changed("status") {
				st := store.Status(statusFlag)
				if !st.Valid() {
					return fmt.Errorf("invalid status %q (valid: new, work, human, done, cancel)", statusFlag)
				}
				s := statusFlag
				statusPtr = &s
			}
			if cmd.Flags().Changed("priority") {
				prioPtr = &priority
			}
			if titlePtr == nil && descPtr == nil && prioPtr == nil && statusPtr == nil {
				return errors.New("nothing to update (pass --title/--description/--status/--priority)")
			}

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.UpdateTask(cmd.Context(), args[0], titlePtr, descPtr, prioPtr, statusPtr)
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			return emit(taskFromWire(t))
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "new description")
	cmd.Flags().StringVar(&statusFlag, "status", "", "new status (new|human|done|cancel)")
	cmd.Flags().IntVarP(&priority, "priority", "p", 0, "new priority (0..3)")
	return cmd
}

// emit prints a single task post-mutation. Honors --json and --quiet.
// Matches the pre-daemon path: a bare task render (no name joins).
func emit(t store.Task) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t)
	}
	return render.Task(os.Stdout, t)
}
