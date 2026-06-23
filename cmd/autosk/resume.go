package main

import (
	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newResumeCmd: `autosk resume <id> [--to STEP]` — move a task out of `human`
// back into `work`. A thin client of the daemon's task.resume {to?}. With no
// --to the task returns to the step it was waiting in; --to STEP relocates it
// to a sibling step (or a terminal/park status: done|cancel|human).
func newResumeCmd() *cobra.Command {
	var toStep string
	cmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a task from human back into the workflow",
		Long: "Move a task out of `human` and back into `work`.\n\n" +
			"By default it returns to the step it was waiting in. Use --to STEP\n" +
			"to jump to a different step in the same workflow, or --to one of\n" +
			"done|cancel|human to relocate to a terminal/park status.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			var to *rpcclient.StepTarget
			if toStep != "" {
				switch toStep {
				case "done", "cancel", "human":
					to = &rpcclient.StepTarget{Status: toStep}
				default:
					to = &rpcclient.StepTarget{Step: toStep}
				}
			}
			t, err := cl.Resume(cmd.Context(), args[0], to)
			if err != nil {
				return err
			}
			return emitTaskWire(t)
		},
	}
	cmd.Flags().StringVar(&toStep, "to", "", "step name to resume at (or done|cancel|human); default: current step")
	return cmd
}
