package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// newBlockCmd: `autosk block <id> <blocker>...` — add blocker edges. v2
// task.block takes a single blocker, so multiple are applied in a loop.
func newBlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "block <id> <blocker-id>...",
		Short: "Add blocker edge(s): each <blocker> blocks <id>",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			blockers := args[1:]
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			for _, b := range blockers {
				if _, err := cl.Block(cmd.Context(), id, b); err != nil {
					return err
				}
			}
			if !flagQuiet {
				fmt.Printf("blocked %s by %v\n", id, blockers)
			}
			return nil
		},
	}
}

// newUnblockCmd: `autosk unblock <id> <blocker>... | --all`.
func newUnblockCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "unblock <id> <blocker-id>... | --all",
		Short: "Remove blocker edge(s) for <id>",
		Args: func(cmd *cobra.Command, args []string) error {
			a, _ := cmd.Flags().GetBool("all")
			switch {
			case a && len(args) < 1:
				return errors.New("unblock --all needs the task id")
			case a && len(args) > 1:
				return errors.New("unblock --all takes only the task id (no blocker list)")
			case !a && len(args) < 2:
				return errors.New("unblock needs <id> and at least one blocker id (or use --all)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			if all {
				t, err := cl.GetTask(cmd.Context(), id)
				if err != nil {
					return err
				}
				n := 0
				for _, ref := range t.BlockedBy {
					if _, err := cl.Unblock(cmd.Context(), id, ref.ID); err != nil {
						return err
					}
					n++
				}
				if !flagQuiet {
					fmt.Printf("unblocked %s (%d edge(s) removed)\n", id, n)
				}
				return nil
			}
			blockers := args[1:]
			for _, b := range blockers {
				if _, err := cl.Unblock(cmd.Context(), id, b); err != nil {
					return err
				}
			}
			if !flagQuiet {
				fmt.Printf("unblocked %s from %v\n", id, blockers)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every incoming blocker edge")
	return cmd
}

// newDepCmd: `autosk dep list <id>` — read-only viewer over the derived
// blocked_by / blocks edges.
func newDepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dep",
		Short: "Inspect task dependencies",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list <id>",
		Short: "Show incoming (blocked_by) and outgoing (blocks) edges",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.GetTask(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("blocked_by: %v\n", wireRefIDs(t.BlockedBy))
			fmt.Printf("blocks:     %v\n", wireRefIDs(t.Blocks))
			return nil
		},
	})
	return cmd
}
