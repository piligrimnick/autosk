package main

import (
	"github.com/spf13/cobra"
)

// newResumeCmd: `autosk resume <id> [--to STEP]` — out of human.
// Plan §5.6. A thin client of the daemon's task.resume verb, which owns
// the status guard + the visit-counter semantics:
//
//   - `resume <id>` with NO --to does NOT count as a transition. The
//     task is flipped back to work at the same step; no visit bump and
//     no cap check. Commit: `resume <id>`.
//   - `resume <id> --to STEP` IS treated as a deliberate transition
//     into STEP, even when STEP == current_step. It goes through
//     EnterStep so the visit counter bumps and step.max_visits is
//     enforced. Commit: `resume <id> --to STEP`.
func newResumeCmd() *cobra.Command {
	var toStep string
	cmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a task from human back into the workflow",
		Long: "Move a task out of `human` and back into `work`.\n\n" +
			"By default it returns to the step it was waiting in (so the same\n" +
			"agent sees the new comments and retries). Use --to STEP to jump\n" +
			"to a different step in the same workflow.\n\n" +
			"Visit counter semantics:\n" +
			"  - resume <id>          — does NOT count as a step visit (no\n" +
			"                            transition; the task stays on the same\n" +
			"                            step).\n" +
			"  - resume <id> --to STEP — DOES count: this is treated as a\n" +
			"                            deliberate transition into STEP and\n" +
			"                            therefore bumps step_visits[STEP].\n" +
			"                            If STEP is at its max_visits cap, the\n" +
			"                            command fails; clear the counter with\n" +
			"                            `autosk metadata reset-visits <id> --step STEP`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.Resume(cmd.Context(), cliSource, args[0], toStep)
			if err != nil {
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().StringVar(&toStep, "to", "", "step name within the task's workflow to resume at (default: current step). Counts as a transition (visit bump + cap check).")
	return cmd
}
