package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/render"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
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
	)
	cmd := &cobra.Command{
		Use:   "create [title]",
		Short: "Create a new task (optionally enter a workflow)",
		Long: `Create a new task.

The task starts in status='new' unless --workflow or --agent (mutually
exclusive) is given:

  --workflow NAME   join an existing workflow at its first_step (status
                    becomes 'in_workflow').

  --agent    NAME   shorthand for joining the auto-generated workflow
                    single:<NAME> (status becomes 'in_workflow').

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

			if description == "-" {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read description from stdin: %w", err)
				}
				description = strings.TrimRight(string(b), "\n")
			}

			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			reg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			ag := agent.New(dl.DB()).WithResolver(reg)
			wfStore := workflow.New(dl.DB(), ag)

			// Resolve caller → author_id.
			author, err := resolveCallerAgent(cmd.Context(), ag)
			if err != nil {
				return fmt.Errorf("resolve caller agent: %w", err)
			}

			task := store.Task{
				Title:       title,
				Description: description,
				Status:      store.StatusNew,
				Priority:    priority,
				AuthorID:    author.ID,
			}

			// Workflow assignment, if any.
			if workflowArg != "" || agentArg != "" {
				wf, firstStep, err := resolveWorkflowEntry(cmd.Context(), wfStore, ag, workflowArg, agentArg)
				if err != nil {
					return err
				}
				task.Status = store.StatusInWorkflow
				task.WorkflowID = wf.ID
				task.CurrentStepID = firstStep.ID
			}

			t, err := s.CreateTask(cmd.Context(), task)
			if err != nil {
				return err
			}

			// Edges.
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
				opts := renderOptsForTask(cmd.Context(), wfStore, t)
				return render.TaskJSONTo(os.Stdout, t, opts...)
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
	return cmd
}

// resolveWorkflowEntry returns the workflow + entry step the task should
// land on, given either --workflow NAME or --agent NAME. Exactly one of
// the two must be non-empty (caller verifies that).
func resolveWorkflowEntry(ctx context.Context, wfs *workflow.Store, ag *agent.Store, wfName, agentName string) (workflow.Workflow, workflow.Step, error) {
	if wfName != "" {
		w, err := wfs.GetByName(ctx, wfName)
		if err != nil {
			if errors.Is(err, workflow.ErrNotFound) {
				return workflow.Workflow{}, workflow.Step{}, fmt.Errorf("workflow not found: %s", wfName)
			}
			return workflow.Workflow{}, workflow.Step{}, err
		}
		st, err := stepByID(w, w.FirstStepID)
		if err != nil {
			return workflow.Workflow{}, workflow.Step{}, fmt.Errorf("first step missing in %s: %w", wfName, err)
		}
		return w, st, nil
	}
	// --agent NAME: ensure the agent exists, then ensure single:<name>.
	if _, err := ag.EnsureByName(ctx, agentName); err != nil {
		return workflow.Workflow{}, workflow.Step{}, fmt.Errorf("ensure agent %s: %w", agentName, err)
	}
	w, err := wfs.EnsureSingle(ctx, agentName)
	if err != nil {
		return workflow.Workflow{}, workflow.Step{}, fmt.Errorf("ensure single:%s: %w", agentName, err)
	}
	st, err := stepByID(w, w.FirstStepID)
	if err != nil {
		return workflow.Workflow{}, workflow.Step{}, fmt.Errorf("single:%s missing first step: %w", agentName, err)
	}
	return w, st, nil
}

// stepByID returns the step with the given id from a loaded Workflow.
func stepByID(w workflow.Workflow, stepID string) (workflow.Step, error) {
	for _, s := range w.Steps {
		if s.ID == stepID {
			return s, nil
		}
	}
	return workflow.Step{}, fmt.Errorf("step %s not found in workflow %s", stepID, w.Name)
}

// renderOptsForTask is a thin alias for taskRenderOpts so the older
// callsite name keeps compiling.
func renderOptsForTask(ctx context.Context, wfs *workflow.Store, t store.Task) []render.Option {
	return taskRenderOpts(ctx, wfs, t)
}
