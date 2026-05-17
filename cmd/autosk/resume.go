package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// newResumeCmd: `autosk resume <id> [--to STEP]` — out of human_feedback.
// Plan §5.6.
func newResumeCmd() *cobra.Command {
	var toStep string
	cmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a task from human_feedback back into the workflow",
		Long: "Move a task out of `human_feedback` and back into `in_workflow`. By\n" +
			"default it returns to the step it was waiting in (so the same agent\n" +
			"sees the new comments and retries). Use --to STEP to jump to a\n" +
			"different step in the same workflow.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			ag := agent.New(dl.DB())
			wfs := workflow.New(dl.DB(), ag)

			cur, err := s.GetTask(cmd.Context(), taskID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", taskID)
				}
				return err
			}
			if cur.Status != store.StatusHumanFeedback {
				return fmt.Errorf("cannot resume task in status %q (only `human_feedback`)", cur.Status)
			}
			if cur.WorkflowID == "" {
				return fmt.Errorf("task %s has no workflow_id; cannot resume", taskID)
			}

			// Resolve target step.
			var stepID string
			if toStep != "" {
				st, err := wfs.FindStepByName(cmd.Context(), cur.WorkflowID, toStep)
				if err != nil {
					if errors.Is(err, workflow.ErrNotFound) {
						return fmt.Errorf("step %q not found in this task's workflow", toStep)
					}
					return err
				}
				stepID = st.ID
			} else if cur.CurrentStepID != "" {
				stepID = cur.CurrentStepID
			} else {
				return errors.New("task has no current_step_id; pass --to STEP")
			}

			newStatus := store.StatusInWorkflow
			t, err := s.UpdateTask(cmd.Context(), taskID, store.TaskPatch{
				Status:        &newStatus,
				CurrentStepID: &stepID,
			})
			if err != nil {
				return err
			}
			commitWrite(cmd.Context(), s, "resume "+taskID)
			return emitRich(cmd.Context(), wfs, t)
		},
	}
	cmd.Flags().StringVar(&toStep, "to", "", "step name within the task's workflow to resume at (default: current step)")
	return cmd
}
