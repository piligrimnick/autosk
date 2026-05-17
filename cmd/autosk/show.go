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
			// Surface derived current_step / current_agent when the task is
			// in a workflow. Best-effort: failures here are ignored.
			if t.CurrentStepID != "" {
				if dl, ok := s.(*doltlite.Store); ok {
					wfs := workflow.New(dl.DB(), agent.New(dl.DB()))
					if step, err := wfs.FindStepByID(cmd.Context(), t.CurrentStepID); err == nil {
						opts = append(opts, render.WithStep(step.Name, step.AgentName))
					}
				}
			}

			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t, opts...)
			}
			return render.Task(os.Stdout, t, opts...)
		},
	}
	return cmd
}
