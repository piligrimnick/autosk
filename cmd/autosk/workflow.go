package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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
	var (
		file      string
		noInstall bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a workflow from a JSON definition file",
		Long: "Create a workflow from a JSON definition file.\n\n" +
			"Scoped npm-style agent names (e.g. @scope/name) that aren't yet\n" +
			"installed are auto-installed from the registry before the workflow\n" +
			"is created (same as `autosk agent install <name>`). Use\n" +
			"--no-install to disable this and fail on any missing agent instead.",
		Args: cobra.NoArgs,
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

			if !noInstall {
				if err := autoInstallMissingAgents(cmd.Context(), def, wf.Agents(), dl); err != nil {
					return err
				}
			}

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
	cmd.Flags().BoolVar(&noInstall, "no-install", false, "do not auto-install missing scoped-npm agents; fail validation instead")
	return cmd
}

// autoInstallMissingAgents scans def.Steps for agents not present in the
// DB and installs the ones whose names look like scoped npm packages
// (`@scope/name`). Bare names are skipped: we can't safely guess that
// `developer` on the public npm registry is the same agent the workflow
// author had in mind, so those still produce the standard validation
// error.
//
// On a successful install the matching `agents` row is created so the
// downstream workflow validation sees a real agent.
//
// The function is a no-op (returns nil) when every agent is either
// `human` or already in the DB.
func autoInstallMissingAgents(ctx context.Context, def workflow.Definition, ag *agent.Store, dl *doltlite.Store) error {
	// Collect unique referenced agent names.
	seen := make(map[string]struct{}, len(def.Steps))
	for _, s := range def.Steps {
		if s.Agent == "" {
			continue
		}
		seen[s.Agent] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}

	// Determine which need install (not in DB, not 'human', scoped name).
	var todo []string
	for name := range seen {
		if name == agent.HumanAgentName {
			continue
		}
		if _, err := ag.GetByName(ctx, name); err == nil {
			continue
		}
		if !looksLikeScopedNpmName(name) {
			continue
		}
		todo = append(todo, name)
	}
	if len(todo) == 0 {
		return nil
	}
	sort.Strings(todo)

	reg, err := openPackagesRegistry()
	if err != nil {
		return fmt.Errorf("open packages registry: %w", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		return fmt.Errorf("ensure packages prefix: %w", err)
	}

	// Attach the registry to the agent store so EnsureByName accepts the
	// newly-installed names. Re-uses the same *sql.DB handle.
	agWithResolver := agent.New(dl.DB()).WithResolver(reg)

	if !flagQuiet {
		fmt.Fprintf(os.Stderr, "workflow references %d uninstalled agent(s); installing: %s\n",
			len(todo), strings.Join(todo, ", "))
	}

	for _, name := range todo {
		if !flagQuiet {
			fmt.Fprintf(os.Stderr, "\u2192 agent install %s\n", name)
		}
		entry, ierr := reg.Install(ctx, name, "")
		if ierr != nil {
			return fmt.Errorf("auto-install %s failed: %w (pass --no-install to skip, or install manually with `autosk agent install %s`)",
				name, ierr, name)
		}
		if _, eerr := agWithResolver.EnsureByName(ctx, entry.Name); eerr != nil {
			return fmt.Errorf("register %s in agents table: %w", entry.Name, eerr)
		}
		if !flagQuiet {
			fmt.Fprintf(os.Stderr, "  installed %s@%s\n", entry.Name, entry.Version)
		}
	}
	return nil
}

// looksLikeScopedNpmName reports whether s is an npm name with a
// leading `@scope/` prefix. We only auto-install scoped names because
// bare names (`developer`, `code-reviewer`) collide too easily with
// arbitrary public npm packages.
func looksLikeScopedNpmName(s string) bool {
	if len(s) < 3 || s[0] != '@' {
		return false
	}
	slash := strings.IndexByte(s, '/')
	return slash > 1 && slash < len(s)-1
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
