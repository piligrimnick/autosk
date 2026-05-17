package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/render"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// workflowStoreFromCmd opens the project DB and returns a workflow.Store +
// the doltlite handle (for commits) + a close func.
func workflowStoreFromCmd(ctx context.Context, writeOK bool) (*workflow.Store, *doltlite.Store, func(), error) {
	s, closeFn, err := openStore(ctx, writeOK)
	if err != nil {
		return nil, nil, nil, err
	}
	dl, ok := s.(*doltlite.Store)
	if !ok {
		closeFn()
		return nil, nil, nil, errors.New("workflow CLI: doltlite store required")
	}
	ag := agent.New(dl.DB())
	return workflow.New(dl.DB(), ag), dl, closeFn, nil
}

func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Manage workflows (step graphs that agents traverse)",
		Long: "A workflow is a small directed graph of steps; each step has an agent\n" +
			"and a list of transitions annotated with prompt_rule text. Define one\n" +
			"in JSON (see docs/notes/workflow-example.json) and create it with\n" +
			"`autosk workflow create --file PATH`.",
	}
	cmd.AddCommand(
		newWorkflowCreateCmd(),
		newWorkflowListCmd(),
		newWorkflowShowCmd(),
		newWorkflowDeleteCmd(),
	)
	return cmd
}

func newWorkflowCreateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a workflow from a JSON definition file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return errors.New("--file is required")
			}
			def, err := workflow.ParseFile(file)
			if err != nil {
				return err
			}
			wf, dl, closeFn, err := workflowStoreFromCmd(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			w, err := wf.Create(cmd.Context(), def, false /*isSynthetic*/)
			if err != nil {
				if errors.Is(err, workflow.ErrAlreadyExist) {
					return fmt.Errorf("workflow already exists: %s", def.Name)
				}
				return err
			}
			_ = dl.DoltCommit(cmd.Context(), "workflow create "+w.Name)
			return emitWorkflow(w, false /*withSteps*/)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to workflow JSON definition")
	return cmd
}

func newWorkflowListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workflows (synthetic single:<agent> rows hidden unless --all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wf, _, closeFn, err := workflowStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			ws, err := wf.List(cmd.Context(), all)
			if err != nil {
				return err
			}
			return emitWorkflows(ws)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include auto-generated single:<agent> workflows")
	return cmd
}

func newWorkflowShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one workflow with its steps and transitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wf, _, closeFn, err := workflowStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			w, err := wf.GetByName(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, workflow.ErrNotFound) {
					return fmt.Errorf("workflow not found: %s", args[0])
				}
				return err
			}
			return emitWorkflow(w, true /*withSteps*/)
		},
	}
	return cmd
}

func newWorkflowDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a workflow (refuses if any task references it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wf, dl, closeFn, err := workflowStoreFromCmd(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := wf.Delete(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, workflow.ErrNotFound) {
					return fmt.Errorf("workflow not found: %s", args[0])
				}
				return err
			}
			_ = dl.DoltCommit(cmd.Context(), "workflow delete "+args[0])
			if !flagQuiet {
				fmt.Printf("deleted: %s\n", args[0])
			}
			return nil
		},
	}
	return cmd
}

// ---- render --------------------------------------------------------------

// workflowJSON is the JSON projection used by `workflow show/create --json`.
type workflowJSON struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	FirstStepID string        `json:"first_step_id"`
	IsSynthetic bool          `json:"is_synthetic"`
	CreatedAt   string        `json:"created_at"`
	Steps       []stepJSON    `json:"steps,omitempty"`
}

type stepJSON struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	AgentID     string             `json:"agent_id"`
	AgentName   string             `json:"agent_name"`
	Transitions []transitionJSON   `json:"transitions"`
}

type transitionJSON struct {
	ID           int64  `json:"id"`
	NextStepID   string `json:"next_step_id,omitempty"`
	NextStepName string `json:"next_step_name,omitempty"`
	TaskStatus   string `json:"task_status,omitempty"`
	PromptRule   string `json:"prompt_rule"`
}

func toWorkflowJSON(w workflow.Workflow, withSteps bool) workflowJSON {
	wj := workflowJSON{
		ID:          w.ID,
		Name:        w.Name,
		Description: w.Description,
		FirstStepID: w.FirstStepID,
		IsSynthetic: w.IsSynthetic,
		CreatedAt:   w.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if withSteps {
		for _, st := range w.Steps {
			sj := stepJSON{
				ID: st.ID, Name: st.Name, AgentID: st.AgentID, AgentName: st.AgentName,
			}
			for _, tr := range st.Transitions {
				sj.Transitions = append(sj.Transitions, transitionJSON{
					ID:           tr.ID,
					NextStepID:   tr.NextStepID,
					NextStepName: tr.NextStepName,
					TaskStatus:   tr.TaskStatus,
					PromptRule:   tr.PromptRule,
				})
			}
			wj.Steps = append(wj.Steps, sj)
		}
	}
	return wj
}

func emitWorkflow(w workflow.Workflow, withSteps bool) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toWorkflowJSON(w, withSteps))
	}
	fmt.Printf("%s\n", render.BracketedRef(w.ID, w.Name))
	if w.Description != "" {
		fmt.Printf("description:  %s\n", w.Description)
	}
	// first_step: render with the step name when we can find it among
	// the loaded steps; fall back to raw id otherwise.
	var firstStepName string
	for _, st := range w.Steps {
		if st.ID == w.FirstStepID {
			firstStepName = st.Name
			break
		}
	}
	fmt.Printf("first_step:   %s\n", render.BracketedRef(w.FirstStepID, firstStepName))
	fmt.Printf("synthetic:    %t\n", w.IsSynthetic)
	fmt.Printf("created_at:   %s\n", w.CreatedAt.Format("2006-01-02T15:04:05Z"))
	if withSteps && len(w.Steps) > 0 {
		fmt.Println("steps:")
		for _, st := range w.Steps {
			fmt.Printf("  %s\n", render.BracketedRef(st.ID, st.Name))
			fmt.Printf("    agent: %s\n", render.BracketedRef(st.AgentID, st.AgentName))
			for _, tr := range st.Transitions {
				if tr.IsTaskStatus() {
					fmt.Printf("    → task_status=%s: %s\n", tr.TaskStatus, tr.PromptRule)
				} else {
					fmt.Printf("    → step %s: %s\n", tr.NextStepName, tr.PromptRule)
				}
			}
		}
	}
	return nil
}

func emitWorkflows(ws []workflow.Workflow) error {
	if flagJSON {
		out := make([]workflowJSON, len(ws))
		for i, w := range ws {
			out[i] = toWorkflowJSON(w, false)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(ws) == 0 {
		fmt.Fprintln(os.Stderr, "(no workflows)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSYNTHETIC\tDESCRIPTION")
	for _, wf := range ws {
		synth := "no"
		if wf.IsSynthetic {
			synth = "yes"
		}
		desc := wf.Description
		if len(desc) > 60 {
			desc = desc[:57] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", wf.ID, wf.Name, synth, desc)
	}
	return w.Flush()
}
