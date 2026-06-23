package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
)

// readyTasks lists tasks with status='new' and no open blocker (the daemon's
// derived `blocked` flag is false). v2 has no dedicated task.ready method; the
// ready set is just task.list filtered by status=new, blocked=false.
func readyTasks(ctx context.Context, cl *rpcclient.Client) ([]rpcclient.Task, error) {
	notBlocked := false
	return cl.Tasks(ctx, rpcclient.TaskListFilter{
		Statuses: []string{"new"},
		Blocked:  &notBlocked,
	})
}

func newReadyCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List tasks with no open blockers (status='new')",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wtasks, err := readyTasks(cmd.Context(), cl)
			if err != nil {
				return err
			}
			if limit > 0 && len(wtasks) > limit {
				wtasks = wtasks[:limit]
			}
			tasks := tasksFromWire(wtasks)
			if flagJSON {
				return render.TasksJSONTo(os.Stdout, tasks, wireListDecorator(wtasks))
			}
			if len(tasks) == 0 {
				if !flagQuiet {
					fmt.Fprintln(os.Stderr, "(nothing ready)")
				}
				return nil
			}
			return render.Tasks(os.Stdout, tasks, wireListDecorator(wtasks))
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}

// newNextCmd is the ready-task helper (`autosk next` == `ready --limit 1`),
// NOT the dropped v1 `step next` workflow-transition alias. P7 removed the
// `step` group and its `next` alias; this `next` is a read-only convenience
// that surfaces the single top task with no open blockers.
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
			wtasks, err := readyTasks(cmd.Context(), cl)
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
			t := taskFromWire(wtasks[0])
			opts := []render.Option{wireBlockedOpt(wtasks[0]), render.WithCommentCount(wtasks[0].CommentCount)}
			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t, opts...)
			}
			return render.Task(os.Stdout, t, opts...)
		},
	}
}
