package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/store"
)

func newCreateCmd() *cobra.Command {
	var (
		title       string
		description string
		priority    int
		blocks      []string
		blockedBy   []string
		workflowArg string
		agentArg    string
		stepArg     string
	)
	cmd := &cobra.Command{
		Use:   "create [title]",
		Short: "Create a new task (optionally enter a workflow)",
		Long: `Create a new task.

The task starts in status='new' unless --workflow or --agent (mutually
exclusive) is given:

  --workflow NAME   join an existing workflow at its first_step (status
                    becomes 'work'). Pair with --step NAME to
                    enter at a specific step instead of first_step.

  --agent    NAME   shorthand for joining the auto-generated workflow
                    single:<NAME> (status becomes 'work').

  --step     NAME   start the workflow at this step (requires --workflow;
                    incompatible with --agent).

For tasks that already exist (status='new'), use 'autosk enroll <id>
--workflow NAME' / '--agent NAME' to attach them to a workflow without
recreating the task.

If --description is "-", the description is read from stdin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Title resolution.
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
			// Flag-combination checks stay client-side so the messages are
			// identical without a daemon round-trip (the workflow resolution,
			// worktree allocation + EnterStep + rollback all happen
			// server-side in task.create).
			if stepArg != "" {
				switch {
				case agentArg != "":
					return errors.New("--step only applies with --workflow (single:<agent> workflows have a single step)")
				case workflowArg == "":
					return errors.New("--step requires --workflow")
				}
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
			t, err := cl.CreateTask(cmd.Context(), rpcclient.TaskCreateParams{
				Source:      cliSource,
				Title:       title,
				Description: description,
				Priority:    &priority,
				Blocks:      blocks,
				BlockedBy:   blockedBy,
				Workflow:    workflowArg,
				Agent:       agentArg,
				Step:        stepArg,
				Caller:      callerAgentName(),
			})
			if err != nil {
				return err
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
	cmd.Flags().IntVarP(&priority, "priority", "p", store.DefaultPriority, "priority (0=highest..3=lowest)")
	cmd.Flags().StringSliceVar(&blocks, "blocks", nil, "ids of tasks this task blocks")
	cmd.Flags().StringSliceVar(&blockedBy, "blocked-by", nil, "ids of tasks that block this task")
	cmd.Flags().StringVar(&workflowArg, "workflow", "", "join this named workflow at its first step")
	cmd.Flags().StringVar(&agentArg, "agent", "", "shorthand for --workflow single:<name>; ensures the synthetic workflow exists")
	cmd.Flags().StringVar(&stepArg, "step", "", "start at this step name instead of the workflow's first step (requires --workflow)")
	return cmd
}
