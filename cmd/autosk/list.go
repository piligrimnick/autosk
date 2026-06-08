package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
	"autosk/internal/store"
)

func newListCmd() *cobra.Command {
	var (
		statuses []string
		priority int
		limit    int
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks (default: open work — new, work, human)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var f rpcclient.TaskListFilter

			// --status not given → daemon default (open work).
			// --status all       → no filter.
			// --status a,b,c     → those.
			if len(statuses) > 0 {
				if len(statuses) == 1 && strings.ToLower(statuses[0]) == "all" {
					f.Statuses = []string{} // explicit empty = no filter
				} else {
					for _, s := range statuses {
						st := store.Status(strings.TrimSpace(s))
						if !st.Valid() {
							return fmt.Errorf("invalid status %q (valid: new, work, human, done, cancel, all)", s)
						}
						f.Statuses = append(f.Statuses, string(st))
					}
				}
			}
			if cmd.Flags().Changed("priority") {
				if priority < store.MinPriority || priority > store.MaxPriority {
					return errors.New("priority must be 0..3")
				}
				f.Priority = &priority
			}

			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wtasks, err := cl.Tasks(cmd.Context(), f)
			if err != nil {
				return err
			}
			// --limit is applied client-side: the daemon's task.list returns
			// the full ordered set (priority ASC, created_at ASC) and the
			// CLI truncates, matching the old backend limit semantics.
			if limit > 0 && len(wtasks) > limit {
				wtasks = wtasks[:limit]
			}
			tasks := tasksFromWire(wtasks)
			if flagJSON {
				return render.TasksJSONTo(os.Stdout, tasks, nil)
			}
			if len(tasks) == 0 {
				if !flagQuiet {
					fmt.Fprintln(os.Stderr, "(no tasks)")
				}
				return nil
			}
			return render.Tasks(os.Stdout, tasks, nil)
		},
	}
	cmd.Flags().StringSliceVar(&statuses, "status", nil, "filter by status (comma-separated; 'all' = no filter; default: new,work,human)")
	cmd.Flags().IntVarP(&priority, "priority", "p", 0, "filter by exact priority (0..3)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}
