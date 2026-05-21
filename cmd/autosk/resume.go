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
//
// Visit-counter semantics (docs/plans/20260520-Step-Visit-Limits.md):
//
//   - `resume <id>` with NO --to does NOT count as a transition. The
//     task is flipped back to in_workflow at the same step; no visit
//     bump and no cap check.
//   - `resume <id> --to STEP` IS treated as a deliberate transition
//     into STEP, even when STEP == current_step. It goes through
//     workflow.EnterStep so the visit counter bumps and step.max_visits
//     is enforced.
func newResumeCmd() *cobra.Command {
	var toStep string
	cmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a task from human_feedback back into the workflow",
		Long: "Move a task out of `human_feedback` and back into `in_workflow`.\n\n" +
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

			if toStep == "" {
				// No transition: just flip the status. Do NOT touch
				// step_visits or current_step_id (the task resumes on the
				// step it was parked on).
				if cur.CurrentStepID == "" {
					return errors.New("task has no current_step_id; pass --to STEP")
				}
				newStatus := store.StatusInWorkflow
				t, err := s.UpdateTask(cmd.Context(), taskID, store.TaskPatch{Status: &newStatus})
				if err != nil {
					return err
				}
				commitWrite(cmd.Context(), s, "resume "+taskID)
				return emitRich(cmd.Context(), wfs, t)
			}

			// --to STEP: a deliberate transition. Resolve the step,
			// then let EnterStep bump step_visits + enforce the cap.
			st, err := wfs.FindStepByName(cmd.Context(), cur.WorkflowID, toStep)
			if err != nil {
				if errors.Is(err, workflow.ErrNotFound) {
					return fmt.Errorf("step %q not found in this task's workflow", toStep)
				}
				return err
			}
			if err := workflow.EnterStep(cmd.Context(), s, wfs, workflow.EnterStepInput{
				TaskID: taskID,
				StepID: st.ID,
			}); err != nil {
				return mapEnterStepError(err, taskID)
			}
			t, err := s.GetTask(cmd.Context(), taskID)
			if err != nil {
				return err
			}
			commitWrite(cmd.Context(), s, "resume "+taskID+" --to "+toStep)
			return emitRich(cmd.Context(), wfs, t)
		},
	}
	cmd.Flags().StringVar(&toStep, "to", "", "step name within the task's workflow to resume at (default: current step). Counts as a transition (visit bump + cap check).")
	return cmd
}
