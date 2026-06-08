package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
	"autosk/internal/timeformat"
	"autosk/internal/workflow"
)

// workflowFromWire converts a daemon workflow view (rpcclient.Workflow) into
// the storage-shaped workflow.Workflow the CLI renderers consume, so the
// human + --json output stays byte-identical to the pre-daemon path while the
// data now arrives over JSON-RPC. The wire view is a superset: it carries both
// the lazy datasource fields and the CLI show fields (first_step_id, created_at,
// per-step agent_id/agent_params/transitions).
func workflowFromWire(w rpcclient.Workflow) workflow.Workflow {
	out := workflow.Workflow{
		ID:          w.ID,
		Name:        w.Name,
		Description: w.Description,
		FirstStepID: w.FirstStepID,
		IsSynthetic: w.IsSynthetic,
		Isolation:   workflow.IsolationMode(w.Isolation),
		CreatedAt:   w.CreatedAt,
	}
	for _, s := range w.Steps {
		st := workflow.Step{
			ID:        s.ID,
			Name:      s.Name,
			AgentID:   s.AgentID,
			AgentName: s.AgentName,
			MaxVisits: s.MaxVisits,
		}
		if len(s.AgentParams) > 0 && string(s.AgentParams) != "null" {
			var ap workflow.AgentParams
			if err := json.Unmarshal(s.AgentParams, &ap); err == nil {
				st.AgentParams = &ap
			}
		}
		for _, tr := range s.Transitions {
			st.Transitions = append(st.Transitions, workflow.Transition{
				ID:           tr.ID,
				StepID:       s.ID,
				NextStepID:   tr.NextStepID,
				NextStepName: tr.NextStepName,
				TaskStatus:   tr.TaskStatus,
				PromptRule:   tr.PromptRule,
			})
		}
		out.Steps = append(out.Steps, st)
	}
	return out
}

func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Manage workflows (step graphs that agents traverse)",
		Long: "A workflow is a small directed graph of steps; each step has an agent\n" +
			"and a list of transitions annotated with prompt_rule text. Define one\n" +
			"in JSON (see docs/examples/workflows/workflow-example.json) and create it with\n" +
			"`autosk workflow create --file PATH`.",
	}
	cmd.AddCommand(
		newWorkflowCreateCmd(),
		newWorkflowListCmd(),
		newWorkflowShowCmd(),
		newWorkflowDeleteCmd(),
		newWorkflowUpdateCmd(),
	)
	return cmd
}

// newWorkflowUpdateCmd implements `autosk workflow update <name>
// --isolation <none|worktree>`. See docs/plans/20260521-Workflow-Update-Isolation.md
// for the safety semantics; in short:
//
//   - synthetic workflows are always rejected (single:<agent> is
//     pinned to none by construction);
//   - non-terminal tasks referencing the workflow refuse the update
//     unless --force is passed;
//   - none→worktree --force allocates a fresh worktree per task,
//     atomically rolling back every prior Ensure on the first
//     failure;
//   - worktree→none --force does NOT remove leftover directories
//     (that would discard uncommitted state); the leftover paths
//     are printed for human cleanup via `autosk worktree rm`.
//
// The daemon owns the DB + worktree allocation; the CLI is a thin RPC client of
// workflow.updateIsolation, which returns the (possibly partial) safety report
// even on the error path.
func newWorkflowUpdateCmd() *cobra.Command {
	var (
		isolationFlag string
		force         bool
		dryRun        bool
	)
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update mutable fields on a workflow (v0.4: only --isolation)",
		Long: "Flip a workflow's isolation mode.\n\n" +
			"The only mutable field today is --isolation. Other workflow fields\n" +
			"(description, first_step, step graph) are immutable; delete+recreate\n" +
			"the workflow instead.\n\n" +
			"By default the verb refuses to run if any task currently references\n" +
			"the workflow in a non-terminal state (new+workflow_id, work,\n" +
			"human). --force overrides the refusal:\n\n" +
			"  none → worktree   each non-terminal task gets a fresh worktree\n" +
			"                    allocated from HEAD. The update is atomic: if any\n" +
			"                    one Ensure fails, every prior Ensure for this run\n" +
			"                    is rolled back.\n\n" +
			"  worktree → none   leftover worktree directories are NOT removed\n" +
			"                    (that would discard uncommitted state). Their\n" +
			"                    paths are printed; clean them with\n" +
			"                    'autosk worktree rm <task-id>' once you have\n" +
			"                    salvaged anything you need.\n\n" +
			"Use --dry-run to preview the side-effects without committing.\n\n" +
			"Synthetic workflows (single:<agent>) are always rejected: their\n" +
			"isolation is pinned to 'none' by construction.\n\n" +
			"Daemon race: this verb does NOT lock against an in-flight daemon.\n" +
			"A run that's already picked up will finish in its current cwd;\n" +
			"the new mode takes effect on the next step run. Stop the daemon\n" +
			"first if you need to be strict about the cutover.\n\n" +
			"Examples:\n" +
			"  autosk workflow update feature-dev --isolation worktree\n" +
			"  autosk workflow update feature-dev --isolation worktree --force --dry-run\n" +
			"  autosk workflow update feature-dev --isolation none --force\n",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			target, err := parseIsolationFlag(isolationFlag)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			// dry-run never mutates, so it must not auto-init a fresh project.
			var cl *rpcclient.Client
			if dryRun {
				cl, err = readClient(ctx)
			} else {
				cl, err = writeClient(ctx)
			}
			if err != nil {
				return err
			}

			wireRep, err := cl.WorkflowUpdateIsolation(ctx, cliSource, name, string(target), force, dryRun)
			rep := updateReportFromWire(wireRep)
			if err != nil {
				// FR10 / report contract: --json must always emit the
				// UpdateIsolationReport to stdout, even on the error
				// paths, so tooling that parses the JSON output sees
				// the structured outcome (offending task ids,
				// rolled-back ensures, etc.) instead of an empty
				// stdout + a string error on stderr. The exit code
				// carries the failure signal; the JSON body carries the
				// diagnosis.
				if flagJSON {
					_ = json.NewEncoder(os.Stdout).Encode(toWorkflowUpdateJSON(rep))
					return err
				}
				return renderWorkflowUpdateError(err, rep)
			}
			return emitWorkflowUpdateReport(rep)
		},
	}
	cmd.Flags().StringVar(&isolationFlag, "isolation", "", "target isolation mode (none|worktree)")
	_ = cmd.MarkFlagRequired("isolation")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the non-terminal-tasks guard with mode-specific side-effects")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen without committing")
	return cmd
}

// updateReportFromWire maps the wire UpdateIsolationReport onto the
// workflow-package shape the renderers consume.
func updateReportFromWire(r rpcclient.UpdateIsolationReport) workflow.UpdateIsolationReport {
	out := workflow.UpdateIsolationReport{
		Workflow:         r.Workflow,
		From:             workflow.IsolationMode(r.From),
		To:               workflow.IsolationMode(r.To),
		Noop:             r.Noop,
		DryRun:           r.DryRun,
		NonTerminalTasks: r.NonTerminalTasks,
		FailedTask:       r.FailedTask,
	}
	for _, e := range r.EnsuredTasks {
		out.EnsuredTasks = append(out.EnsuredTasks, workflow.EnsureRecord{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	for _, l := range r.LeftoverWorktrees {
		out.LeftoverWorktrees = append(out.LeftoverWorktrees, workflow.LeftoverWorktree{
			TaskID: l.TaskID, Path: l.Path,
		})
	}
	for _, e := range r.RolledBackEnsures {
		out.RolledBackEnsures = append(out.RolledBackEnsures, workflow.EnsureRecord{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	return out
}

// parseIsolationFlag validates the --isolation CLI value. We do this
// in the verb rather than at flag-parse time so the error message
// matches the contract spelled out in plan FR2.
func parseIsolationFlag(raw string) (workflow.IsolationMode, error) {
	switch raw {
	case string(workflow.IsolationNone), string(workflow.IsolationWorktree):
		return workflow.IsolationMode(raw), nil
	default:
		return "", fmt.Errorf("invalid --isolation: %q (want none|worktree)", raw)
	}
}

// workflowUpdateJSON is the JSON projection of UpdateIsolationReport.
// We re-emit it through a CLI-owned struct so the wire shape stays
// stable across internal-package refactors (lazygit's HTTP API
// pattern).
type workflowUpdateJSON struct {
	Workflow          string                 `json:"workflow"`
	From              string                 `json:"from"`
	To                string                 `json:"to"`
	Noop              bool                   `json:"noop"`
	DryRun            bool                   `json:"dry_run"`
	NonTerminalTasks  []string               `json:"non_terminal_tasks,omitempty"`
	EnsuredTasks      []workflowEnsureJSON   `json:"ensured_tasks,omitempty"`
	LeftoverWorktrees []workflowLeftoverJSON `json:"leftover_worktrees,omitempty"`
	RolledBackEnsures []workflowEnsureJSON   `json:"rolled_back_ensures,omitempty"`
	FailedTask        string                 `json:"failed_task,omitempty"`
}

type workflowEnsureJSON struct {
	TaskID   string `json:"task_id"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

type workflowLeftoverJSON struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}

func toWorkflowUpdateJSON(rep workflow.UpdateIsolationReport) workflowUpdateJSON {
	out := workflowUpdateJSON{
		Workflow:         rep.Workflow,
		From:             string(rep.From),
		To:               string(rep.To),
		Noop:             rep.Noop,
		DryRun:           rep.DryRun,
		NonTerminalTasks: rep.NonTerminalTasks,
		FailedTask:       rep.FailedTask,
	}
	for _, e := range rep.EnsuredTasks {
		out.EnsuredTasks = append(out.EnsuredTasks, workflowEnsureJSON{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	for _, l := range rep.LeftoverWorktrees {
		out.LeftoverWorktrees = append(out.LeftoverWorktrees, workflowLeftoverJSON{
			TaskID: l.TaskID, Path: l.Path,
		})
	}
	for _, e := range rep.RolledBackEnsures {
		out.RolledBackEnsures = append(out.RolledBackEnsures, workflowEnsureJSON{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	return out
}

// emitWorkflowUpdateReport prints the report. Text and JSON branches
// surface the same fields with the same semantics.
func emitWorkflowUpdateReport(rep workflow.UpdateIsolationReport) error {
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toWorkflowUpdateJSON(rep))
	}
	if flagQuiet {
		return nil
	}
	if rep.Noop {
		fmt.Printf("workflow %s: isolation already %s (no-op)\n", rep.Workflow, rep.To)
		return nil
	}
	prefix := ""
	if rep.DryRun {
		prefix = "DRY-RUN: "
	}
	fmt.Printf("%sworkflow %s: isolation %s → %s\n", prefix, rep.Workflow, rep.From, rep.To)
	if len(rep.EnsuredTasks) > 0 {
		fmt.Println("ensured worktrees:")
		for _, e := range rep.EnsuredTasks {
			state := "new"
			if e.Existing {
				state = "existing"
			}
			fmt.Printf("  %s  %s  (branch=%s, %s)\n", e.TaskID, e.Path, e.Branch, state)
		}
	}
	if len(rep.LeftoverWorktrees) > 0 {
		fmt.Println("leftover worktree (not removed):")
		for _, l := range rep.LeftoverWorktrees {
			fmt.Printf("  %s  %s\n", l.TaskID, l.Path)
		}
		fmt.Println("  (run `autosk worktree rm <task-id>` once you have salvaged uncommitted state)")
	}
	return nil
}

// renderWorkflowUpdateError emits structured details on the error
// paths so the operator sees the offending task ids (refusal) or the
// rolled-back set (mid-run Ensure failure) without having to re-run
// with --json.
//
// The diagnostics are reconstructed from the daemon's returned report
// rather than from Go sentinels (the error now arrives as a JSON-RPC
// error). We surface the daemon's message verbatim (it already carries
// the `pass --force to update` hint) and append the report-derived
// detail lines, returning a plain error so the central RPC-error
// unwrapper in main() does not strip the appended diagnostics.
func renderWorkflowUpdateError(err error, rep workflow.UpdateIsolationReport) error {
	var b strings.Builder
	for _, id := range rep.NonTerminalTasks {
		fmt.Fprintf(&b, "\nnon-terminal task in workflow: %s", id)
	}
	if rep.FailedTask != "" {
		fmt.Fprintf(&b, "\nfailed task: %s", rep.FailedTask)
	}
	if len(rep.RolledBackEnsures) > 0 {
		b.WriteString("\nrolled back:")
		for _, e := range rep.RolledBackEnsures {
			fmt.Fprintf(&b, "\n  %s  %s", e.TaskID, e.Path)
		}
		b.WriteString("\n(workflows.isolation left unchanged)")
	}
	return errors.New(cleanRPCError(err).Error() + b.String())
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
			// Resolve to an absolute path: the daemon re-parses the file
			// (relative FirstMessageFile inlining is resolved against its
			// directory) and may run in a different cwd than the CLI.
			absFile, err := filepath.Abs(file)
			if err != nil {
				return err
			}
			// Parse client-side too: surfaces parse errors with the Go
			// messages and lets us scan referenced agents for auto-install.
			def, err := workflow.ParseFile(absFile)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cl, err := writeClient(ctx)
			if err != nil {
				return err
			}

			if !noInstall {
				if err := autoInstallMissingAgents(ctx, cl, def); err != nil {
					return fmt.Errorf("%w (pass --no-install to skip)", err)
				}
			}

			// The client already handled auto-install (so the "installing"
			// notice renders here, not silently in the daemon); ask the daemon
			// to skip its own auto-install and just create + commit.
			name, err := cl.WorkflowCreate(ctx, cliSource, absFile, "", true)
			if err != nil {
				return err
			}
			w, err := cl.GetWorkflow(ctx, name)
			if err != nil {
				return err
			}
			return emitWorkflow(workflowFromWire(w), false /*withSteps*/)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to workflow JSON definition")
	cmd.Flags().BoolVar(&noInstall, "no-install", false, "do not auto-install missing scoped-npm agents; fail validation instead")
	return cmd
}

// autoInstallMissingAgents scans def.Steps for scoped-npm agent names not yet
// installed in the project DB and installs them through the daemon's
// agent.install RPC (which performs the npm install + agents-row commit). Bare
// names are skipped: we can't safely guess that `developer` on the public npm
// registry is the same agent the workflow author had in mind, so those still
// produce the standard validation error from the daemon's create.
//
// The function is a no-op (returns nil) when every referenced agent is either
// `human` or already present in the DB.
func autoInstallMissingAgents(ctx context.Context, cl *rpcclient.Client, def workflow.Definition) error {
	seen := make(map[string]struct{}, len(def.Steps))
	for _, s := range def.Steps {
		if s.AgentName == "" {
			continue
		}
		seen[s.AgentName] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}

	// Which referenced agents already exist in the DB? Ask the daemon.
	agents, err := cl.Agents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	installed := make(map[string]bool, len(agents))
	for _, a := range agents {
		installed[a.Name] = true
	}

	var todo []string
	for name := range seen {
		if name == agent.HumanAgentName {
			continue
		}
		if installed[name] {
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

	if !flagQuiet {
		fmt.Fprintf(os.Stderr, "workflow references %d uninstalled agent(s); installing: %s\n",
			len(todo), strings.Join(todo, ", "))
	}
	for _, name := range todo {
		if !flagQuiet {
			fmt.Fprintf(os.Stderr, "\u2192 agent install %s\n", name)
		}
		a, ierr := cl.AgentInstall(ctx, name, "", "")
		if ierr != nil {
			// The helper deliberately does NOT mention --no-install
			// because the caller wraps this error with its own contextual
			// flag hint.
			return fmt.Errorf("auto-install %s failed: %w (install manually with `autosk agent install %s`)",
				name, cleanRPCError(ierr), name)
		}
		if !flagQuiet {
			ver := a.Version
			if ver == "" {
				ver = installedVersion(name)
			}
			if ver != "" {
				fmt.Fprintf(os.Stderr, "  installed %s@%s\n", name, ver)
			} else {
				fmt.Fprintf(os.Stderr, "  installed %s\n", name)
			}
		}
	}
	return nil
}

// installedVersion best-effort reads the resolved version of a just-installed
// package from the local packages registry (the daemon installs into the same
// prefix in local mode). Empty on any failure.
func installedVersion(name string) string {
	reg, err := openPackagesRegistry()
	if err != nil {
		return ""
	}
	if e, err := reg.Get(name); err == nil {
		return e.Version
	}
	return ""
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
			ctx := cmd.Context()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			ws, err := cl.Workflows(ctx, all)
			if err != nil {
				return err
			}
			out := make([]workflow.Workflow, len(ws))
			for i, w := range ws {
				out[i] = workflowFromWire(w)
			}
			return emitWorkflows(out)
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
			ctx := cmd.Context()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			w, err := cl.GetWorkflow(ctx, args[0])
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("workflow not found: %s", args[0])
				}
				return err
			}
			return emitWorkflow(workflowFromWire(w), true /*withSteps*/)
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
			ctx := cmd.Context()
			cl, err := writeClient(ctx)
			if err != nil {
				return err
			}
			if err := cl.WorkflowDelete(ctx, cliSource, args[0]); err != nil {
				return err
			}
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
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	FirstStepID string     `json:"first_step_id"`
	IsSynthetic bool       `json:"is_synthetic"`
	Isolation   string     `json:"isolation"`
	CreatedAt   string     `json:"created_at"`
	Steps       []stepJSON `json:"steps,omitempty"`
}

type stepJSON struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	AgentID     string                `json:"agent_id"`
	AgentName   string                `json:"agent_name"`
	AgentParams *workflow.AgentParams `json:"agent_params,omitempty"`
	Transitions []transitionJSON      `json:"transitions"`
}

type transitionJSON struct {
	ID           int64  `json:"id"`
	NextStepID   string `json:"next_step_id,omitempty"`
	NextStepName string `json:"next_step_name,omitempty"`
	TaskStatus   string `json:"task_status,omitempty"`
	PromptRule   string `json:"prompt_rule"`
}

func toWorkflowJSON(w workflow.Workflow, withSteps bool) workflowJSON {
	iso := string(w.Isolation)
	if iso == "" {
		iso = string(workflow.IsolationNone)
	}
	wj := workflowJSON{
		ID:          w.ID,
		Name:        w.Name,
		Description: w.Description,
		FirstStepID: w.FirstStepID,
		IsSynthetic: w.IsSynthetic,
		Isolation:   iso,
		CreatedAt:   w.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if withSteps {
		for _, st := range w.Steps {
			sj := stepJSON{
				ID: st.ID, Name: st.Name, AgentID: st.AgentID, AgentName: st.AgentName,
				AgentParams: st.AgentParams,
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
	if w.Isolation != "" && w.Isolation != workflow.IsolationNone {
		fmt.Printf("isolation:    %s\n", w.Isolation)
	}
	// Text output uses the operator's local TZ; the JSON form
	// (toWorkflowJSON above) keeps the RFC3339 UTC string.
	fmt.Printf("created_at:   %s\n", timeformat.FormatDateTime(w.CreatedAt))
	if withSteps && len(w.Steps) > 0 {
		fmt.Println("steps:")
		for _, st := range w.Steps {
			fmt.Printf("  %s\n", render.BracketedRef(st.ID, st.Name))
			fmt.Printf("    agent: %s\n", render.BracketedRef(st.AgentID, st.AgentName))
			renderAgentParams(st.AgentParams)
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

// renderAgentParams pretty-prints the per-step agent.params overrides
// under `agent:` in `workflow show`. Skips the long FirstMessage body
// (shows just the byte count) so the output stays scannable.
func renderAgentParams(p *workflow.AgentParams) {
	if p.IsZero() {
		return
	}
	fmt.Println("    agent_params:")
	if p.Model != nil {
		fmt.Printf("      model: %q\n", *p.Model)
	}
	if p.Thinking != nil {
		fmt.Printf("      thinking: %q\n", *p.Thinking)
	}
	if p.FirstMessage != nil {
		fmt.Printf("      first_message: <%d bytes>\n", len(*p.FirstMessage))
	}
	if p.ExtraArgs != nil {
		fmt.Printf("      extra_args: %v\n", p.ExtraArgs)
	}
	if p.PiExtensions != nil {
		fmt.Printf("      pi_extensions: %v\n", p.PiExtensions)
	}
	if p.PiSkills != nil {
		fmt.Printf("      pi_skills: %v\n", p.PiSkills)
	}
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
