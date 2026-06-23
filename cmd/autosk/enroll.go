package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// newEnrollCmd: `autosk enroll <id> --workflow NAME [--step STEP]` — (re-)attach
// an existing task to a workflow. A thin client of the daemon's task.enroll,
// which owns the status guard, isolation acquire, and entry transition. Enroll
// is allowed from new / cancel / human (work and done are rejected).
func newEnrollCmd() *cobra.Command {
	var workflowArg string
	var stepArg string
	cmd := &cobra.Command{
		Use:   "enroll <id>",
		Short: "Enroll an existing task into a workflow",
		Long: `(Re-)attach an existing task to a workflow.

  --workflow NAME   enroll into the named workflow (status becomes 'work').
  --step STEP       start at this step (default: the workflow's first step).

Enroll is allowed from new / cancel / human; work and done are rejected.

Example:

  autosk enroll ask-bea935 --workflow feature-dev
  autosk enroll ask-bea935 --workflow feature-dev --step review`,
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
			t, err := cl.EnrollWorkflow(cmd.Context(), taskID, workflowArg, stepArg)
			if err != nil {
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "enroll the task into this named workflow")
	cmd.Flags().StringVar(&stepArg, "step", "", "start at this step (default: the workflow's first step)")
	return cmd
}
