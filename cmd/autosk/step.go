package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newStepCmd is the parent for agent-facing step verbs. The only
// subcommand today is `next`, which records the transition signal that
// closes a workflow run (plan §5.4).
func newStepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "step",
		Short: "Workflow step commands (agent-facing)",
		Long: "Step verbs are invoked by agents from inside a workflow run.\n" +
			"They never mutate the task directly; they record the agent's\n" +
			"chosen transition so the daemon can advance the task atomically.",
	}
	cmd.AddCommand(newStepNextCmd())
	return cmd
}

func newStepNextCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "next <task-id>",
		Short: "Record the agent's chosen transition for the active run",
		Long: "Resolve the active daemon_runs row for <task-id>, validate --to\n" +
			"against the current step's outgoing transitions, and insert a row\n" +
			"into step_signals. The daemon advances the task after the agent\n" +
			"finishes its turn.\n\n" +
			"Calling `step next` twice in the same run is rejected (PK on\n" +
			"step_signals.run_id).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return errors.New("--to NAME is required (sibling step name, or done|cancel|human)")
			}
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			emitted, err := cl.StepNext(cmd.Context(), args[0], to)
			if err != nil {
				return cleanRPCError(err)
			}
			return emitStepSignal(emitted)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "transition target: sibling step name OR done|cancel|human")
	return cmd
}

// ---- render --------------------------------------------------------------

type stepSignalJSON struct {
	RunID        string `json:"run_id"`
	TaskID       string `json:"task_id"`
	TransitionID int64  `json:"transition_id"`
	NextStep     string `json:"next_step,omitempty"`
	TaskStatus   string `json:"task_status,omitempty"`
	PromptRule   string `json:"prompt_rule"`
	CreatedAt    string `json:"created_at"`
}

func emitStepSignal(e rpcclient.StepSignal) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(stepSignalJSON{
			RunID:        e.RunID,
			TaskID:       e.TaskID,
			TransitionID: e.TransitionID,
			NextStep:     e.NextStepName,
			TaskStatus:   e.TaskStatus,
			PromptRule:   e.PromptRule,
			CreatedAt:    e.CreatedAt,
		})
	}
	fmt.Printf("recorded transition for run %s\n", e.RunID)
	if e.NextStepName != "" {
		fmt.Printf("  next step:  %s\n", e.NextStepName)
	}
	if e.TaskStatus != "" {
		fmt.Printf("  task_status: %s\n", e.TaskStatus)
	}
	fmt.Printf("  rule:       %s\n", e.PromptRule)
	return nil
}
