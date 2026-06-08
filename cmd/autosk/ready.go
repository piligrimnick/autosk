package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/render"
)

func newReadyCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List tasks with no open blockers (status='new'), prio-sorted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wtasks, err := cl.Ready(cmd.Context(), limit)
			if err != nil {
				return err
			}
			tasks := tasksFromWire(wtasks)
			if flagJSON {
				return render.TasksJSONTo(os.Stdout, tasks, nil)
			}
			if len(tasks) == 0 {
				if !flagQuiet {
					fmt.Fprintln(os.Stderr, "(nothing ready)")
				}
				return nil
			}
			return render.Tasks(os.Stdout, tasks, nil)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}

func newNextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "next",
		Short: "Show the single top ready task (alias: ready --limit 1)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wtasks, err := cl.Ready(cmd.Context(), 1)
			if err != nil {
				return err
			}
			if len(wtasks) == 0 {
				if flagJSON {
					_, _ = os.Stdout.Write([]byte("null\n"))
				} else if !flagQuiet {
					fmt.Fprintln(os.Stderr, "(nothing ready)")
				}
				return errSilentExit1
			}
			tasks := tasksFromWire(wtasks)
			if flagJSON {
				return render.TaskJSONTo(os.Stdout, tasks[0])
			}
			return render.Task(os.Stdout, tasks[0])
		},
	}
}
