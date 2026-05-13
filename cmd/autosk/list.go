package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
		Short:   "List tasks (default: open work — new + claimed)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()

			f := store.ListFilter{Limit: limit}

			// --status not given → backend default (open work).
			// --status all       → no filter.
			// --status a,b,c     → those.
			if len(statuses) > 0 {
				if len(statuses) == 1 && strings.ToLower(statuses[0]) == "all" {
					f.Statuses = []store.Status{} // explicit empty = no filter
				} else {
					for _, s := range statuses {
						st := store.Status(strings.TrimSpace(s))
						if !st.Valid() {
							return fmt.Errorf("invalid status %q (valid: new, claimed, done, cancelled, all)", s)
						}
						f.Statuses = append(f.Statuses, st)
					}
				}
			}
			if cmd.Flags().Changed("priority") {
				if priority < store.MinPriority || priority > store.MaxPriority {
					return errors.New("priority must be 0..3")
				}
				f.Priority = &priority
			}

			tasks, err := s.ListTasks(cmd.Context(), f)
			if err != nil {
				return err
			}
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
	cmd.Flags().StringSliceVar(&statuses, "status", nil, "filter by status (comma-separated; 'all' = no filter; default: new,claimed)")
	cmd.Flags().IntVarP(&priority, "priority", "p", 0, "filter by exact priority (0..3)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}
