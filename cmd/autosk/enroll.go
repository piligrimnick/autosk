package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// newEnrollCmd: `autosk enroll <id> --workflow NAME|--agent NAME` —
// (re-)attach an existing task to a workflow or single-agent flow.
//
// Mirror of the --workflow / --agent flags on `autosk create`, but for
// tasks that already exist in the project DB. The daemon's task.enroll
// owns the status guard, workflow resolution, worktree allocation (for
// isolation=worktree workflows), EnterStep (step_visits bump + cap), and
// the failure rollback — all reproduced server-side with the CLI commit
// dialect (`enroll <id> --workflow NAME` / `--agent NAME`).
//
// Status guard (enforced by the daemon):
//   - `new`, `human`, `done`, `cancel` → enroll succeeds.
//   - `work` → refused (the engine owns the task; cancel then enroll).
func newEnrollCmd() *cobra.Command {
	var (
		workflowArg string
		agentArg    string
		stepArg     string
		baseRefArg  string
	)
	cmd := &cobra.Command{
		Use:   "enroll <id>",
		Short: "Enroll an existing task into a workflow (or single-agent flow)",
		Long: `(Re-)attach an existing task to a workflow.

This is the post-creation equivalent of:

  autosk create ... --workflow NAME
  autosk create ... --agent    NAME

Use it when the task already exists and you want to put it into a
workflow without recreating it. Exactly one of --workflow / --agent
is required; they are mutually exclusive.

  --workflow NAME   enroll into the named workflow at its first step
                    (status becomes 'work'). Pair with --step NAME
                    to enter at a specific step instead of first_step.

  --agent    NAME   shorthand for --workflow single:<NAME>; the
                    synthetic single:<NAME> workflow is auto-created
                    on first use (matches 'create --agent').

  --step     NAME   enroll at this step inside --workflow instead of
                    the workflow's first step. Requires --workflow;
                    incompatible with --agent (single:<agent> flows
                    only have one step).

Accepted source statuses: new, human, done, cancel. The only refusal
is status='work' — the task is currently owned by the engine. To
put it into a different workflow, cancel then enroll (or reopen
first if you want to inspect the task in 'new' state).

Note: enroll never clears metadata.step_visits, so re-enrolling into
the same workflow keeps the max_visits cap honest. If you want a
clean slate, run 'autosk metadata reset-visits <id>' first. Stale
keys from a prior workflow are left in place; clear them the same way.

Examples:

  autosk enroll ask-bea935 --workflow feature-dev-generic
  autosk enroll ask-bea935 --workflow feature-dev-generic --step review`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if workflowArg != "" && agentArg != "" {
				return errors.New("--workflow and --agent are mutually exclusive")
			}
			if workflowArg == "" && agentArg == "" {
				return errors.New("--workflow NAME or --agent NAME is required")
			}
			if stepArg != "" && agentArg != "" {
				return errors.New("--step only applies with --workflow (single:<agent> workflows have a single step)")
			}
			if baseRefArg != "" && agentArg != "" {
				return errors.New("--base-ref requires --workflow targeting an isolation=worktree workflow")
			}
			taskID := args[0]

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.Enroll(cmd.Context(), cliSource, taskID, workflowArg, agentArg, stepArg, baseRefArg)
			if err != nil {
				return err
			}
			// Plan acceptance §5: `--json` shape must match `show --json`,
			// including derived `blocked` / `blocked_by` / `blocks`.
			return emitTaskWire(t, wireBlockedOpt(t))
		},
	}
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "enroll the task into this named workflow at its first step")
	cmd.Flags().StringVar(&agentArg, "agent", "", "shorthand for --workflow single:<name>; ensures the synthetic workflow exists")
	cmd.Flags().StringVar(&stepArg, "step", "", "enroll at this step name instead of the workflow's first step (requires --workflow)")
	cmd.Flags().StringVar(&baseRefArg, "base-ref", "", "git ref to branch the worktree from (isolation=worktree workflows; default HEAD; ignored when the task's branch already exists)")
	return cmd
}
