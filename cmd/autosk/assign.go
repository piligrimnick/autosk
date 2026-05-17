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
)

// newAssignCmd: `autosk assign <id> --agent NAME` — moves a `new` task
// into the synthetic single:<NAME> workflow. Plan §6.1.
//
// Only valid on status='new'. In-flight / terminal tasks are rejected.
func newAssignCmd() *cobra.Command {
	var agentName string
	cmd := &cobra.Command{
		Use:   "assign <id>",
		Short: "Assign a task to an agent (puts it into single:<agent> workflow)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if agentName == "" {
				return errors.New("--agent NAME is required")
			}
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
			if cur.Status != store.StatusNew {
				return fmt.Errorf("cannot assign task in status %q: only `new` is acceptable (cancel/reopen first)", cur.Status)
			}
			if _, err := ag.EnsureByName(cmd.Context(), agentName); err != nil {
				return fmt.Errorf("ensure agent %s: %w", agentName, err)
			}
			w, err := wfs.EnsureSingle(cmd.Context(), agentName)
			if err != nil {
				return fmt.Errorf("ensure single:%s: %w", agentName, err)
			}
			st, err := stepByID(w, w.FirstStepID)
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
			commitWrite(cmd.Context(), s, "assign "+taskID+" --agent "+agentName)
			return emitRich(cmd.Context(), wfs, t)
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "agent to assign the task to (e.g. human, developer)")
	return cmd
}

// emitRich prints a task with derived workflow / step / author info
// resolved against the provided workflow store + an agent store derived
// from it. Best-effort: lookup failures degrade to bare ids.
func emitRich(ctx context.Context, wfs *workflow.Store, t store.Task) error {
	if flagQuiet {
		return nil
	}
	opts := taskRenderOpts(ctx, wfs, t)
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t, opts...)
	}
	return render.Task(os.Stdout, t, opts...)
}

// taskRenderOpts builds the standard set of render.Options for a task:
// derived current_step + current_agent (from the step) and the
// human-friendly names for workflow + author.
func taskRenderOpts(ctx context.Context, wfs *workflow.Store, t store.Task) []render.Option {
	var opts []render.Option
	if t.CurrentStepID != "" {
		if step, err := wfs.FindStepByID(ctx, t.CurrentStepID); err == nil {
			opts = append(opts, render.WithStep(step.Name, step.AgentName, step.AgentID))
		}
	}
	if t.WorkflowID != "" {
		if w, err := wfs.GetByID(ctx, t.WorkflowID); err == nil {
			opts = append(opts, render.WithWorkflow(w.Name))
		}
	}
	if t.AuthorID != "" {
		if ag := wfs.Agents(); ag != nil {
			if a, err := ag.GetByID(ctx, t.AuthorID); err == nil {
				opts = append(opts, render.WithAuthor(a.Name))
			}
		}
	}
	return opts
}
