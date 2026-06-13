package main

import (
	"errors"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newEnrollCmd: `autosk enroll <id> --workflow NAME|--agent NAME` —
// (re-)attach an existing task to a workflow or single-agent flow. A thin
// client of the daemon's task.enroll (workflow XOR agent), which owns the
// status guard, isolation acquire, and first-step transition.
func newEnrollCmd() *cobra.Command {
	var (
		workflowArg string
		agentArg    string
	)
	cmd := &cobra.Command{
		Use:   "enroll <id>",
		Short: "Enroll an existing task into a workflow (or single-agent flow)",
		Long: `(Re-)attach an existing task to a workflow at its first step.

Exactly one of --workflow / --agent is required; they are mutually
exclusive.

  --workflow NAME   enroll into the named workflow at its first step
                    (status becomes 'work').

  --agent    NAME   materialise the single-step workflow single:<NAME>
                    and enroll into it.

Examples:

  autosk enroll ask-bea935 --workflow feature-dev
  autosk enroll ask-bea935 --agent @autosk/pi-agent/dev`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if workflowArg != "" && agentArg != "" {
				return errors.New("--workflow and --agent are mutually exclusive")
			}
			if workflowArg == "" && agentArg == "" {
				return errors.New("--workflow NAME or --agent NAME is required")
			}
			taskID := args[0]

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			// Exactly one of workflowArg / agentArg is non-empty here
			// (the guards above enforce XOR), so dispatch to the matching
			// enroll RPC — never call EnrollWorkflow with an empty
			// workflow, which the daemon rejects with INVALID_PARAMS.
			var t rpcclient.Task
			if workflowArg != "" {
				t, err = cl.EnrollWorkflow(cmd.Context(), taskID, workflowArg)
			} else {
				t, err = cl.EnrollAgent(cmd.Context(), taskID, agentArg)
			}
			if err != nil {
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "enroll the task into this named workflow at its first step")
	cmd.Flags().StringVar(&agentArg, "agent", "", "enroll into the single-step workflow single:<name>")
	return cmd
}
