package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// newEnrollCmd: `autosk enroll <id> --workflow NAME` — (re-)attach an existing
// task to a workflow at its first step. A thin client of the daemon's
// task.enroll, which owns the status guard, isolation acquire, and first-step
// transition.
func newEnrollCmd() *cobra.Command {
	var workflowArg string
	cmd := &cobra.Command{
		Use:   "enroll <id>",
		Short: "Enroll an existing task into a workflow",
		Long: `(Re-)attach an existing task to a workflow at its first step.

  --workflow NAME   enroll into the named workflow at its first step
                    (status becomes 'work').

Example:

  autosk enroll ask-bea935 --workflow feature-dev`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if workflowArg == "" {
				return errors.New("--workflow NAME is required")
			}
			taskID := args[0]

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.EnrollWorkflow(cmd.Context(), taskID, workflowArg)
			if err != nil {
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "enroll the task into this named workflow at its first step")
	return cmd
}
