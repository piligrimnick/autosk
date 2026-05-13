package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/store"
)

// newBlockCmd: `autosk block <id> <blocker>...` — variadic, transactional.
func newBlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "block <id> <blocker-id>...",
		Short: "Add blocker edge(s): each <blocker> blocks <id>",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			id := args[0]
			blockers := args[1:]
			if err := s.Block(cmd.Context(), id, blockers...); err != nil {
				return translateBlockErr(err, id, blockers)
			}
			commitWrite(cmd.Context(), s, fmt.Sprintf("block %s by %s", id, strings.Join(blockers, ",")))
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
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			id := args[0]
			if all {
				n, err := s.UnblockAll(cmd.Context(), id)
				if err != nil {
					return err
				}
				commitWrite(cmd.Context(), s, fmt.Sprintf("unblock %s --all", id))
				if !flagQuiet {
					fmt.Printf("unblocked %s (%d edge(s) removed)\n", id, n)
				}
				return nil
			}
			blockers := args[1:]
			if err := s.Unblock(cmd.Context(), id, blockers...); err != nil {
				return err
			}
			commitWrite(cmd.Context(), s, fmt.Sprintf("unblock %s from %s", id, strings.Join(blockers, ",")))
			if !flagQuiet {
				fmt.Printf("unblocked %s from %v\n", id, blockers)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every incoming blocker edge")
	return cmd
}

// newDepCmd: `autosk dep list <id>` — read-only viewer.
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
			s, closeFn, err := openStore(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()

			incoming, outgoing, err := s.Deps(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("blocked_by: %v\n", incoming)
			fmt.Printf("blocks:     %v\n", outgoing)
			return nil
		},
	})
	return cmd
}

func translateBlockErr(err error, id string, blockers []string) error {
	switch {
	case errors.Is(err, store.ErrSelfBlock):
		return fmt.Errorf("a task cannot block itself: %s", id)
	case errors.Is(err, store.ErrCycle):
		return fmt.Errorf("adding %v as blocker(s) of %s would create a cycle", blockers, id)
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("task not found: %s", id)
	case errors.Is(err, store.ErrBlockerNotFound):
		return fmt.Errorf("one of the blocker tasks does not exist: %v", blockers)
	default:
		return err
	}
}
