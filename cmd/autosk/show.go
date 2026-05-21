package main

import (
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

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), false /*read-only*/)
			if err != nil {
				return err
			}
			defer closeFn()

			t, err := s.GetTask(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return errors.New("task not found: " + args[0])
				}
				return err
			}

			incoming, outgoing, err := s.Deps(cmd.Context(), t.ID)
			if err != nil {
				return fmt.Errorf("deps: %w", err)
			}
			blocked, err := s.IsBlocked(cmd.Context(), t.ID)
			if err != nil {
				return fmt.Errorf("is_blocked: %w", err)
			}
			opts := []render.Option{render.WithBlocked(blocked, incoming, outgoing)}

			// Best-effort: surface derived current_step / current_agent +
			// author + workflow names + step_visits summary when the
			// underlying store is doltlite. Hoisted into one block so we
			// don't construct the agent / workflow handles twice.
			var visitsSummary string
			if dl, ok := s.(*doltlite.Store); ok {
				ag := agent.New(dl.DB())
				wfs := workflow.New(dl.DB(), ag)
				if t.CurrentStepID != "" {
					if step, err := wfs.FindStepByID(cmd.Context(), t.CurrentStepID); err == nil {
						opts = append(opts, render.WithStep(step.Name, step.AgentName, step.AgentID))
					}
				}
				if t.AuthorID != "" {
					if a, err := ag.GetByID(cmd.Context(), t.AuthorID); err == nil {
						opts = append(opts, render.WithAuthor(a.Name))
					}
				}
				if t.WorkflowID != "" {
					if wf, err := wfs.GetByID(cmd.Context(), t.WorkflowID); err == nil {
						opts = append(opts, render.WithWorkflow(wf.Name))
						// Surface the deterministic worktree block when
						// the workflow opts into isolation. Path / branch
						// are pure functions of (root, taskID); presence is
						// stat'd at render time.
						if wf.Isolation == workflow.IsolationWorktree {
							if root, perr := projectRootFromCwd(); perr == nil {
								if path, werr := worktree.PathFor(root, t.ID); werr == nil {
									wj := render.WorktreeJSON{
										Path:   path,
										Branch: worktree.BranchFor(t.ID),
									}
									if _, statErr := os.Stat(path); statErr == nil {
										wj.Exists = true
									}
									opts = append(opts, render.WithWorktree(&wj))
								}
							}
						}
					}
				}
				// Surface step_visits inline so humans see why a parked
				// task is stuck on a step. The summary is a single line
				// under the standard task block; JSON output carries
				// the raw map under `metadata` instead.
				if len(t.Metadata) > 0 {
					visitsSummary = renderVisitsSummary(cmd.Context(), wfs, t)
				}
			}
			opts = append(opts, render.WithMetadata(t.Metadata, visitsSummary))

			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t, opts...)
			}
			return render.Task(os.Stdout, t, opts...)
		},
	}
	return cmd
}
