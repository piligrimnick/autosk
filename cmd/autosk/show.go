package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/render"
	"autosk/internal/store"
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
			opt := render.WithBlocked(blocked, incoming, outgoing)

			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t, opt)
			}
			return render.Task(os.Stdout, t, opt)
		},
	}
	return cmd
}
