package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newDoneCmd / newCancelCmd / newReopenCmd / newUpdateCmd: thin clients of the
// daemon's terminal/update verbs. autoskd owns the store mutations (status set,
// isolation release on terminal transitions); the cobra layer only wires the
// CLI surface.

func newDoneCmd() *cobra.Command {
	return terminalVerbCmd("done", "Mark a task done", (*rpcclient.Client).TaskDone)
}

func newCancelCmd() *cobra.Command {
	return terminalVerbCmd("cancel", "Cancel a task", (*rpcclient.Client).TaskCancel)
}

// terminalVerbCmd builds a `<verb> <id> [-f]` command for done/cancel. On a
// terminal transition the daemon reaps the task's worktree (preserving the
// branch); `--force` reaps it even with uncommitted changes. A dirty refusal
// (CodeEnvironmentDirty) is surfaced with a hint instead of a raw rpc error.
func terminalVerbCmd(use, short string, fn func(*rpcclient.Client, context.Context, string, bool) (rpcclient.Task, error)) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := fn(cl, cmd.Context(), args[0], force)
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeEnvironmentDirty {
					return fmt.Errorf("%s\n(re-run with --force to remove the isolation environment and discard the uncommitted changes)", apiErr.Message)
				}
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove the worktree even with uncommitted changes (branch is preserved)")
	return cmd
}

// newReopenCmd: reopen a done/cancel task. Enrolled tasks park to `human`
// (resumable); never-enrolled tasks return to the `new` backlog.
func newReopenCmd() *cobra.Command {
	return taskIDVerbCmd("reopen", "Reopen a closed task", (*rpcclient.Client).TaskReopen)
}

// taskIDVerbCmd builds a `<verb> <id>` command that invokes a one-arg task verb.
func taskIDVerbCmd(use, short string, fn func(*rpcclient.Client, context.Context, string) (rpcclient.Task, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := fn(cl, cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emitTaskWire(t)
		},
	}
}

// newUpdateCmd is the generic field-update verb (title/description only in v2;
// status changes flow through done/cancel/reopen/resume).
func newUpdateCmd() *cobra.Command {
	var (
		title       string
		description string
	)
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a task's title and/or description",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var titlePtr, descPtr *string
			if cmd.Flags().Changed("title") {
				titlePtr = &title
			}
			if cmd.Flags().Changed("description") {
				descPtr = &description
			}
			if titlePtr == nil && descPtr == nil {
				return errors.New("nothing to update (pass --title/--description)")
			}
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.UpdateTask(cmd.Context(), args[0], titlePtr, descPtr)
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "new description")
	return cmd
}
