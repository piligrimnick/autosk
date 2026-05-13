package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/render"
	"autosk/internal/store"
)

func newCreateCmd() *cobra.Command {
	var (
		title       string
		description string
		priority    int
		blocks      []string
		blockedBy   []string
	)
	cmd := &cobra.Command{
		Use:   "create [title]",
		Short: "Create a new task",
		Long: `Create a new task.

Title may be passed positionally or via --title (but not both with different values).

If --description is "-", the description is read from stdin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve title: positional or flag.
			switch {
			case len(args) == 1 && title != "" && args[0] != title:
				return errors.New("title given both positionally and via --title with different values")
			case len(args) == 1:
				title = args[0]
			}
			title = strings.TrimSpace(title)
			if title == "" {
				return errors.New("title is required")
			}

			// Description from stdin if "-".
			if description == "-" {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read description from stdin: %w", err)
				}
				description = strings.TrimRight(string(b), "\n")
			}

			s, closeFn, err := openStore(cmd.Context(), true /*writeOK*/)
			if err != nil {
				return err
			}
			defer closeFn()

			t, err := s.CreateTask(cmd.Context(), store.Task{
				Title:       title,
				Description: description,
				Status:      store.StatusNew,
				Priority:    priority,
			})
			if err != nil {
				return err
			}

			// Edges. --blocks: t blocks each listed id. --blocked-by: each listed id blocks t.
			if len(blocks) > 0 {
				for _, otherID := range blocks {
					if err := s.Block(cmd.Context(), otherID, t.ID); err != nil {
						return fmt.Errorf("--blocks %s: %w", otherID, err)
					}
				}
			}
			if len(blockedBy) > 0 {
				if err := s.Block(cmd.Context(), t.ID, blockedBy...); err != nil {
					return fmt.Errorf("--blocked-by: %w", err)
				}
			}

			commitWrite(cmd.Context(), s, "create "+t.ID)

			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t)
			}
			fmt.Println(t.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "task title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "task description ('-' reads stdin)")
	cmd.Flags().IntVarP(&priority, "priority", "p", store.DefaultPriority, "priority (0=highest..3=lowest)")
	cmd.Flags().StringSliceVar(&blocks, "blocks", nil, "ids of tasks this task blocks (P4)")
	cmd.Flags().StringSliceVar(&blockedBy, "blocked-by", nil, "ids of tasks that block this task (P4)")
	return cmd
}
