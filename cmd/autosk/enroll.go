package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/render"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// newEnrollCmd: `autosk enroll <id> --workflow NAME|--agent NAME` —
// (re-)attach an existing task to a workflow or single-agent flow.
//
// Mirror of the --workflow / --agent flags on `autosk create`, but for
// tasks that already exist in the project DB.
//
// Status guard:
//   - `new`, `human`, `done`, `cancel` → enroll succeeds. The task is
//     stamped with the chosen workflow + entry step and flipped to
//     status='work' atomically by workflow.EnterStep.
//   - `work` → already owned by the engine; the daemon will advance
//     the task on its own. To switch workflows, cancel then enroll
//     (or reopen first if you want to inspect the task in 'new'
//     state before re-enrolling).
//
// `metadata.step_visits` is preserved across enroll regardless of the
// source status; re-enrolling into the same workflow keeps the
// max_visits cap honest. Operators who want a clean slate run
// `autosk metadata reset-visits <id>` themselves.
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

			// Worktree allocation for isolated workflows. Runs BEFORE
			// EnterStep so the daemon's first run for the task sees a
			// ready worktree at the deterministic path. On failure the
			// task stays in status='new'; the user retries after fixing
			// git state.
			var (
				wtAllocated bool
				wtRoot      string
				wtMgr       worktree.Manager
			)
			if w.Isolation == workflow.IsolationWorktree {
				root, perr := projectRootFromCwd()
				if perr != nil {
					return fmt.Errorf("enroll %s: resolve project root: %w", taskID, perr)
				}
				wtMgr = worktree.NewManager()
				res, werr := wtMgr.Ensure(cmd.Context(), root, taskID, baseRefArg)
				if werr != nil {
					return fmt.Errorf("enroll %s: allocate worktree: %w", taskID, werr)
				}
				if res.BaseRefIgnored && !flagQuiet {
					fmt.Fprintf(os.Stderr, "warning: --base-ref ignored — branch %s already exists; reusing\n", res.Branch)
				}
				// Only roll back the worktree on EnterStep failure if we
				// actually allocated it. enroll from `human` is the typical
				// case where Ensure reports Existing=true: the prior step
				// (e.g. validator) parked the task in human and the
				// worktree dir is still on disk; reaping it via OnTerminal
				// would silently destroy the operator's working tree.
				wtAllocated = !res.Existing
				wtRoot = root
			} else if baseRefArg != "" {
				return errors.New("--base-ref requires the target workflow to use isolation=worktree")
			}

			// EnterStep stamps workflow_id + current_step_id + status +
			// bumps step_visits[<entry step>] in a single tx, enforcing
			// step.max_visits along the way. Hitting the cap on first
			// enroll is exotic but legitimate (e.g. someone bumped the
			// counter via `metadata set`); the helper rejects it loudly.
			if err := workflow.EnterStep(cmd.Context(), s, wfs, workflow.EnterStepInput{
				TaskID:     taskID,
				StepID:     st.ID,
				WorkflowID: w.ID,
			}); err != nil {
				// Roll back the worktree we just allocated so a failed
				// enroll preserves the pre-isolation invariant of "no
				// on-disk side effects". cleanupWorktreeOnTerminal
				// won't reach this case via `autosk cancel`: EnterStep
				// failure means workflow_id never got stamped, so the
				// cancel-time guard `t.WorkflowID == ""` short-circuits
				// before the cleanup runs. Unlike create.go's rollback
				// (which unconditionally treats the freshly-allocated
				// worktree as ours — the task id is brand-new so
				// collisions are impossible by construction), enroll
				// gates on res.Existing above so the typical
				// human-source case (pre-existing tracked worktree from
				// a prior step) doesn't get its dir reaped. See
				// TestEnroll_FromHuman_Isolated_FailedRollbackPreservesWorktree.
				if wtAllocated {
					// Reuse the outer wtMgr (already constructed for
					// Ensure) so we don't accidentally split the
					// per-(canonRoot, taskID) mutex across two managers
					// in the same CLI invocation.
					if _, terr := wtMgr.OnTerminal(cmd.Context(), wtRoot, taskID); terr != nil {
						fmt.Fprintf(os.Stderr, "warning: worktree rollback for %s failed: %v\n", taskID, terr)
					}
				}
				return mapEnterStepError(err, taskID)
			}
			t, err := s.GetTask(cmd.Context(), taskID)
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
	cmd.Flags().StringVar(&baseRefArg, "base-ref", "", "git ref to branch the worktree from (isolation=worktree workflows; default HEAD; ignored when the task's branch already exists)")
	return cmd
}

// checkEnrollable returns nil when `t` is in a state that admits enroll.
//
// The only refusal is status='work': the task is currently owned by
// the engine, so re-stamping workflow_id / current_step_id underneath
// it would race the daemon. To switch a work task into a different
// workflow, cancel then enroll (or reopen first if you want to
// inspect the task in 'new' state). tasksvc.Cancel removes any
// per-task worktree dir via cleanupWorktreeOnTerminal; the next
// enroll re-allocates a fresh dir on the surviving autosk/<id>
// branch.
//
// `new`, `human`, `done`, and `cancel` all fall through cleanly:
// workflow.EnterStep stamps workflow_id + current_step_id +
// status='work' atomically and the SQL CHECK invariant
// (status='work' ⇔ current_step_id IS NOT NULL) is upheld across every
// source status.
func checkEnrollable(ctx context.Context, wfs *workflow.Store, t store.Task) error {
	switch t.Status {
	case store.StatusNew, store.StatusHuman, store.StatusDone, store.StatusCancel:
		return nil
	case store.StatusWork:
		wfRef, stepRef := refsForLocation(ctx, wfs, t)
		return fmt.Errorf(
			"task already enrolled in workflow %s at step %s; the daemon will advance it — to switch workflows, cancel then enroll (or reopen first if you want to inspect the task in 'new' state)",
			wfRef, stepRef,
		)
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
