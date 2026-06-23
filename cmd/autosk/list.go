package main

import (
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

			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wtasks, err := cl.Tasks(cmd.Context(), f)
			if err != nil {
				return err
			}
			// --limit is applied client-side.
			if limit > 0 && len(wtasks) > limit {
				wtasks = wtasks[:limit]
			}
			tasks := tasksFromWire(wtasks)
			deco := wireListDecorator(wtasks)
			if flagJSON {
				return render.TasksJSONTo(os.Stdout, tasks, deco)
			}
			if len(tasks) == 0 {
				if !flagQuiet {
					fmt.Fprintln(os.Stderr, "(no tasks)")
				}
				return nil
			}
			return render.Tasks(os.Stdout, tasks, deco)
		},
	}
	cmd.Flags().StringSliceVar(&statuses, "status", nil, "filter by status (comma-separated; 'all' = no filter; default: new,work,human)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}
