package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newCreateCmd() *cobra.Command {
	var (
		title       string
		description string
		blocks      []string
		blockedBy   []string
		workflowArg string
		agentArg    string
	)
	cmd := &cobra.Command{
		Use:   "create [title]",
		Short: "Create a new task (optionally enroll it into a workflow)",
		Long: `Create a new task.

The task starts in status='new' unless --workflow or --agent (mutually
exclusive) is given, in which case it is enrolled right after creation:

  --workflow NAME   enroll into the named workflow at its first step
                    (status becomes 'work').

  --agent    NAME   materialise the single-step workflow single:<NAME>
                    and enroll into it (status becomes 'work').

For tasks that already exist, use 'autosk enroll <id> --workflow NAME'
/ '--agent NAME' to attach them without recreating the task.

If --description is "-", the description is read from stdin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if workflowArg != "" && agentArg != "" {
				return errors.New("--workflow and --agent are mutually exclusive")
			}

			if description == "-" {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read description from stdin: %w", err)
				}
				description = strings.TrimRight(string(b), "\n")
			}

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.CreateTask(cmd.Context(), title, description, blockedBy)
			if err != nil {
				return err
			}
			// --blocks X means "this new task blocks X": add the edge to each X.
			for _, b := range blocks {
				if _, err := cl.Block(cmd.Context(), b, t.ID); err != nil {
					return fmt.Errorf("block %s by %s: %w", b, t.ID, err)
				}
			}
			// Optional enrollment.
			switch {
			case workflowArg != "":
				if t, err = cl.EnrollWorkflow(cmd.Context(), t.ID, workflowArg); err != nil {
					return err
				}
			case agentArg != "":
				if t, err = cl.EnrollAgent(cmd.Context(), t.ID, agentArg); err != nil {
					return err
				}
			}

			if flagJSON {
				return emitTaskWire(t)
			}
			fmt.Println(t.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "task title")
	cmd.Flags().StringVarP(&description, "description", "d", "", "task description ('-' reads stdin)")
	cmd.Flags().StringSliceVar(&blocks, "blocks", nil, "ids of tasks this task blocks")
	cmd.Flags().StringSliceVar(&blockedBy, "blocked-by", nil, "ids of tasks that block this task")
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "enroll into this named workflow at its first step")
	cmd.Flags().StringVar(&agentArg, "agent", "", "enroll into the single-step workflow single:<name>")
	return cmd
}
