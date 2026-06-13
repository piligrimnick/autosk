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
	return taskIDVerbCmd("done", "Mark a task done", (*rpcclient.Client).TaskDone)
}

func newCancelCmd() *cobra.Command {
	return taskIDVerbCmd("cancel", "Cancel a task", (*rpcclient.Client).TaskCancel)
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
