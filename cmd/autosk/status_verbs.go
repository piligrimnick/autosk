package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/render"
	"autosk/internal/store"
)

// newClaimCmd implements `autosk claim <id>`: idempotent new|claimed → claimed.
func newClaimCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "claim <id>",
		Short: "Claim a task (new|claimed → claimed; idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			t, err := s.Claim(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, store.ErrNotClaimable) {
					return fmt.Errorf("task %s is not claimable (done or cancelled)", args[0])
				}
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			commitWrite(cmd.Context(), s, "claim "+args[0])
			return emit(t)
		},
	}
}

// newDoneCmd / newCancelCmd: simple status setters via UpdateTask.
func newDoneCmd() *cobra.Command {
	return statusSetterCmd("done", "Mark a task done", store.StatusDone, nil)
}

func newCancelCmd() *cobra.Command {
	return statusSetterCmd("cancel", "Mark a task cancelled", store.StatusCancelled, nil)
}

// newReopenCmd: status → new, but only from done|cancelled.
func newReopenCmd() *cobra.Command {
	allowed := map[store.Status]bool{
		store.StatusDone:      true,
		store.StatusCancelled: true,
	}
	return statusSetterCmd("reopen", "Reopen a done/cancelled task (→ new)", store.StatusNew, allowed)
}

// statusSetterCmd builds a `<verb> <id>` command that sets status.
// If allowedFrom is non-nil, the current status must be a key in the map.
func statusSetterCmd(use, short string, target store.Status, allowedFrom map[store.Status]bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			if allowedFrom != nil {
				cur, err := s.GetTask(cmd.Context(), args[0])
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("task not found: %s", args[0])
					}
					return err
				}
				if !allowedFrom[cur.Status] {
					return fmt.Errorf("cannot %s task in status %q (use `autosk update <id> --status ...` to force)", use, cur.Status)
				}
			}

			t, err := s.UpdateTask(cmd.Context(), args[0], store.TaskPatch{Status: &target})
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			commitWrite(cmd.Context(), s, use+" "+args[0])
			return emit(t)
		},
	}
}

// newUpdateCmd is the generic field-update verb.
func newUpdateCmd() *cobra.Command {
	var (
		title       string
		description string
		statusFlag  string
		priority    int
	)
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update one or more fields on a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			var patch store.TaskPatch
			if cmd.Flags().Changed("title") {
				patch.Title = &title
			}
			if cmd.Flags().Changed("description") {
				patch.Description = &description
			}
			if cmd.Flags().Changed("status") {
				st := store.Status(statusFlag)
				if !st.Valid() {
					return fmt.Errorf("invalid status %q (valid: new, claimed, done, cancelled)", statusFlag)
				}
				patch.Status = &st
			}
			if cmd.Flags().Changed("priority") {
				patch.Priority = &priority
			}
			if patch.IsEmpty() {
				return errors.New("nothing to update (pass --title/--description/--status/--priority)")
			}

			t, err := s.UpdateTask(cmd.Context(), args[0], patch)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			commitWrite(cmd.Context(), s, "update "+args[0])
			return emit(t)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "new description")
	cmd.Flags().StringVar(&statusFlag, "status", "", "new status (new|claimed|done|cancelled)")
	cmd.Flags().IntVarP(&priority, "priority", "p", 0, "new priority (0..3)")
	return cmd
}

// emit prints a single task post-mutation. Honors --json and --quiet.
func emit(t store.Task) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t)
	}
	return render.Task(os.Stdout, t)
}
