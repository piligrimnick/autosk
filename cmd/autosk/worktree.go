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
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// newWorktreeCmd builds the read-mostly `autosk worktree` parent. The
// engine handles allocate/cleanup automatically; these verbs are for
// diagnostics and manual recovery only. None of them mutate task rows.
func newWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Inspect / clean up per-task git worktrees (isolation=worktree workflows)",
		Long: "Diagnostic verbs for the per-task worktrees allocated by\n" +
			"isolation=worktree workflows. The engine creates worktrees on\n" +
			"enroll/create and removes them on done/cancelled \u2014 these verbs\n" +
			"are for inspection and manual recovery.",
	}
	cmd.AddCommand(
		newWorktreeListCmd(),
		newWorktreePathCmd(),
		newWorktreeRmCmd(),
	)
	return cmd
}

// worktreeRow is the per-task row surfaced by `worktree list`.
type worktreeRow struct {
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	Workflow   string `json:"workflow"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	Exists     bool   `json:"exists"`
	ProjectKey string `json:"project_root"`
}

func newWorktreeListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List per-task worktrees in this project",
		Long: "Lists every task whose workflow has isolation=worktree, alongside\n" +
			"the derived worktree path/branch and whether the directory is\n" +
			"currently on disk. Output respects --json.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := collectWorktreeRows(cmd.Context())
			if err != nil {
				return err
			}
			return emitWorktreeRows(rows)
		},
	}
	return cmd
}

func newWorktreePathCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path <task-id>",
		Short: "Print the derived worktree path for a task (does not stat)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := projectRootFromCwd()
			if err != nil {
				return err
			}
			path, err := worktree.PathFor(root, args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{
					"task_id": args[0],
					"path":    path,
					"branch":  worktree.BranchFor(args[0]),
				})
			}
			fmt.Println(path)
			return nil
		},
	}
	return cmd
}

func newWorktreeRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <task-id>",
		Short: "Force-remove the worktree directory for a non-live task (branch preserved)",
		Long: "Manually removes the on-disk worktree directory for a task. The\n" +
			"engine performs this automatically on done/cancelled \u2014 use this\n" +
			"verb for orphan cleanup, stranded-worktree recovery, or when the\n" +
			"engine couldn't reach the filesystem at terminal time. The branch\n" +
			"is never touched.\n\n" +
			"Refuses tasks that are status=in_workflow: the daemon may be\n" +
			"executing inside that worktree right now, and yanking it would\n" +
			"crash the live run. Parked (human_feedback) and new tasks are\n" +
			"fair game \u2014 the documented recovery flow for `worktree_stranded`\n" +
			"(run parks \u2192 worktree rm \u2192 cancel \u2192 reopen \u2192 enroll) relies on\n" +
			"this. The `worktree_missing` case is auto-recovered by the daemon\n" +
			"itself \u2014 no manual `rm` needed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			t, err := s.GetTask(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if t.Status == store.StatusInWorkflow {
				return fmt.Errorf("refusing to rm worktree of in_workflow task %s; the daemon may be executing inside it. Cancel or wait for the workflow to close / park it first",
					t.ID)
			}
			root, err := projectRootFromCwd()
			if err != nil {
				return err
			}
			wtMgr := worktree.NewManager()
			res, err := wtMgr.OnTerminal(cmd.Context(), root, t.ID)
			if err != nil {
				return fmt.Errorf("worktree rm %s: %w", t.ID, err)
			}
			if flagJSON {
				// `removed` is the action effect (did we actually reap a
				// directory?), distinct from `exists` on worktreeRow
				// (`worktree list`) which is a state observation (is
				// the directory on disk at the time of the read?).
				// Keeping both names because they carry distinct
				// semantics; consumers that want either form pick the
				// matching verb.
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"task_id": t.ID,
					"path":    res.Path,
					"branch":  res.Branch,
					"removed": res.Existed,
				})
			}
			if res.Existed {
				fmt.Printf("removed: %s\n", res.Path)
			} else {
				fmt.Printf("no-op: %s did not exist\n", res.Path)
			}
			return nil
		},
	}
	return cmd
}

// collectWorktreeRows enumerates tasks under isolated workflows in the
// current project. Only opens the store once.
func collectWorktreeRows(ctx context.Context) ([]worktreeRow, error) {
	s, closeFn, err := openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	dl, ok := s.(*doltlite.Store)
	if !ok {
		return nil, errors.New("worktree list: doltlite store required")
	}
	ag := agent.New(dl.DB())
	wfs := workflow.New(dl.DB(), ag)

	root, err := projectRootFromCwd()
	if err != nil {
		return nil, err
	}

	// Two-pass: first identify which workflow IDs are isolation=worktree
	// so we don't fetch the workflow row per task. Synthetic workflows
	// (single:<agent>) are pinned to isolation=none at construction
	// time, so we don't include them — saves an unbounded number of
	// rows on projects with many ad-hoc --agent runs.
	allWFs, err := wfs.List(ctx, false /*includeSynthetic*/)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	isolated := make(map[string]string, len(allWFs))
	for _, w := range allWFs {
		if w.Isolation == workflow.IsolationWorktree {
			isolated[w.ID] = w.Name
		}
	}
	if len(isolated) == 0 {
		return nil, nil
	}

	// Enumerate ALL tasks (every status) so closed tasks with surviving
	// directories still appear in the list.
	tasks, err := s.ListTasks(ctx, store.ListFilter{Statuses: []store.Status{}})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	var rows []worktreeRow
	for _, t := range tasks {
		wfName, ok := isolated[t.WorkflowID]
		if !ok {
			continue
		}
		path, perr := worktree.PathFor(root, t.ID)
		if perr != nil {
			continue
		}
		exists := false
		if _, statErr := os.Stat(path); statErr == nil {
			exists = true
		}
		rows = append(rows, worktreeRow{
			TaskID:     t.ID,
			Status:     string(t.Status),
			Workflow:   wfName,
			Path:       path,
			Branch:     worktree.BranchFor(t.ID),
			Exists:     exists,
			ProjectKey: root,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
	return rows, nil
}

func emitWorktreeRows(rows []worktreeRow) error {
	if flagJSON {
		if rows == nil {
			rows = []worktreeRow{}
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "(no isolated worktrees)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tSTATUS\tWORKFLOW\tBRANCH\tDIR\tPATH")
	for _, r := range rows {
		dir := "missing"
		if r.Exists {
			dir = "present"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.TaskID, r.Status, r.Workflow, r.Branch, dir, shortenHome(r.Path))
	}
	return w.Flush()
}

// shortenHome replaces a leading $HOME with `~` for readable output.
func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}
