package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/render"
	"autosk/internal/store"
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
			return emit(t)
		},
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
