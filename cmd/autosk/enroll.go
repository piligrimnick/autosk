package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/render"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// newEnrollCmd: `autosk enroll <id> --workflow NAME|--agent NAME` —
// attach an already-created task to a workflow or single-agent flow.
//
// Mirror of the --workflow / --agent flags on `autosk create`, but for
// tasks that were created without a workflow (status='new').
//
// Status guard:
//   - `new`              → enroll succeeds.
//   - `in_workflow`      → already owned by the engine; rejection points
//     the user at cancel+reopen+enroll for a workflow switch (the
//     daemon will otherwise advance it on its own).
//   - `human_feedback`   → parked for human input; rejection points the
//     user at `autosk resume --to STEP`.
//   - `done` / `cancelled` → terminal; rejection points the user at
//     `autosk reopen <id>`.
func newEnrollCmd() *cobra.Command {
	var (
		workflowArg string
		agentArg    string
		stepArg     string
	)
	cmd := &cobra.Command{
		Use:   "enroll <id>",
		Short: "Enroll an existing task into a workflow (or single-agent flow)",
		Long: `Attach an existing task to a workflow.

This is the post-creation equivalent of:

  autosk create ... --workflow NAME
  autosk create ... --agent    NAME

Use it when a task already exists (status='new') and you want to put it
into a workflow without recreating it. Exactly one of --workflow /
--agent is required; they are mutually exclusive.

  --workflow NAME   enroll into the named workflow at its first step
                    (status becomes 'in_workflow'). Pair with --step
                    NAME to enter at a specific step instead of
                    first_step.

  --agent    NAME   shorthand for --workflow single:<NAME>; the
                    synthetic single:<NAME> workflow is auto-created
                    on first use (matches 'create --agent').

  --step     NAME   enroll at this step inside --workflow instead of
                    the workflow's first step. Requires --workflow;
                    incompatible with --agent (single:<agent> flows
                    only have one step).

Only tasks in status='new' can be enrolled:
  - in_workflow      → the daemon will advance this task; to put it
                       into a different workflow, cancel + reopen +
                       enroll.
  - human_feedback   → use 'autosk resume --to STEP' to move the task
                       within its current workflow.
  - done / cancelled → use 'autosk reopen <id>' first.

Examples:

  autosk enroll as-bea9 --workflow feature-dev-generic
  autosk enroll as-bea9 --workflow feature-dev-generic --step review`,
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
			taskID := args[0]

			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			reg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			ag := agent.New(dl.DB()).WithResolver(reg)
			wfs := workflow.New(dl.DB(), ag)

			cur, err := s.GetTask(cmd.Context(), taskID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", taskID)
				}
				return err
			}
			if err := checkEnrollable(cmd.Context(), wfs, cur); err != nil {
				return err
			}

			w, st, err := resolveWorkflowEntry(cmd.Context(), wfs, ag, workflowArg, agentArg, stepArg)
			if err != nil {
				return err
			}

			newStatus := store.StatusInWorkflow
			t, err := s.UpdateTask(cmd.Context(), taskID, store.TaskPatch{
				Status:        &newStatus,
				WorkflowID:    &w.ID,
				CurrentStepID: &st.ID,
			})
			if err != nil {
				return err
			}

			label := "--workflow " + workflowArg
			if agentArg != "" {
				label = "--agent " + agentArg
			}
			commitWrite(cmd.Context(), s, "enroll "+taskID+" "+label)
			// Plan acceptance §5: `--json` shape must match `show --json`,
			// including derived `blocked` / `blocked_by` / `blocks`. Use the
			// blocked-aware emitter (taskRenderOpts on its own omits them).
			return emitRichWithBlocked(cmd.Context(), wfs, s, t)
		},
	}
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "enroll the task into this named workflow at its first step")
	cmd.Flags().StringVar(&agentArg, "agent", "", "shorthand for --workflow single:<name>; ensures the synthetic workflow exists")
	cmd.Flags().StringVar(&stepArg, "step", "", "enroll at this step name instead of the workflow's first step (requires --workflow)")
	return cmd
}

// checkEnrollable returns nil when `t` is in a state that admits enroll
// (currently: only status='new'). Otherwise it returns a CLI-style error
// pointing the user at the right verb to make progress. The two
// already-enrolled states get distinct hints: `human_feedback` can be
// unparked with `autosk resume --to`, while `in_workflow` is owned by
// the engine — switching workflows means cancel+reopen+enroll.
func checkEnrollable(ctx context.Context, wfs *workflow.Store, t store.Task) error {
	switch t.Status {
	case store.StatusNew:
		return nil
	case store.StatusInWorkflow:
		wfRef, stepRef := refsForLocation(ctx, wfs, t)
		return fmt.Errorf(
			"task already enrolled in workflow %s at step %s; the daemon will advance it — to switch workflows, cancel + reopen + enroll",
			wfRef, stepRef,
		)
	case store.StatusHumanFeedback:
		wfRef, stepRef := refsForLocation(ctx, wfs, t)
		return fmt.Errorf(
			"task is parked in workflow %s at step %s for human feedback; use 'autosk resume %s [--to STEP]' to put it back into the workflow",
			wfRef, stepRef, t.ID,
		)
	case store.StatusDone, store.StatusCancelled:
		return fmt.Errorf("task is terminal (%s); reopen first ('autosk reopen %s')", t.Status, t.ID)
	default:
		return fmt.Errorf("cannot enroll task in status %q", t.Status)
	}
}

// refsForLocation returns user-facing `[id]: name` references for the
// workflow + current step the task is in, falling back to a bare
// `[id]` (via render.BracketedRef) when name lookup fails. Used by the
// already-enrolled error messages so the visual style matches `show`.
func refsForLocation(ctx context.Context, wfs *workflow.Store, t store.Task) (string, string) {
	var wfName, stepName string
	if t.WorkflowID != "" {
		if w, err := wfs.GetByID(ctx, t.WorkflowID); err == nil {
			wfName = w.Name
		}
	}
	if t.CurrentStepID != "" {
		if st, err := wfs.FindStepByID(ctx, t.CurrentStepID); err == nil {
			stepName = st.Name
		}
	}
	return render.BracketedRef(t.WorkflowID, wfName), render.BracketedRef(t.CurrentStepID, stepName)
}
