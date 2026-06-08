package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
	"autosk/internal/worktree"
)

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			w, err := cl.GetTask(cmd.Context(), args[0])
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return errors.New("task not found: " + args[0])
				}
				return err
			}

			// blocked / blocked_by / blocks + derived names arrive on the
			// wire (the daemon computes those joins).
			opts := []render.Option{wireBlockedOpt(w)}
			opts = append(opts, wireNameOpts(w)...)

			// Worktree block + step_visits summary need the workflow's
			// isolation + step graph; fetch it once when the task is in a
			// workflow. Best-effort: a lookup failure degrades to bare ids,
			// matching the old store-side behaviour.
			var visitsSummary string
			if w.WorkflowID != "" && w.WorkflowName != "" {
				if wf, werr := cl.GetWorkflow(cmd.Context(), w.WorkflowName); werr == nil {
					// Surface the deterministic worktree block when the
					// workflow opts into isolation. Path / branch are pure
					// functions of (root, taskID); presence is stat'd here.
					if wf.Isolation == "worktree" {
						if root, perr := projectRootFromCwd(); perr == nil {
							if path, pErr := worktree.PathFor(root, w.ID); pErr == nil {
								wj := render.WorktreeJSON{
									Path:   path,
									Branch: worktree.BranchFor(w.ID),
								}
								if _, statErr := os.Stat(path); statErr == nil {
									wj.Exists = true
								}
								opts = append(opts, render.WithWorktree(&wj))
							}
						}
					}
					// Inline step_visits so humans see why a parked task
					// is stuck on a step. JSON output carries the raw map
					// under `metadata` instead.
					if len(w.Metadata) > 0 {
						visitsSummary = wireVisitsSummary(wf, w.Metadata)
					}
				}
			}
			opts = append(opts, render.WithMetadata(w.Metadata, visitsSummary))

			t := taskFromWire(w)
			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t, opts...)
			}
			return render.Task(os.Stdout, t, opts...)
		},
	}
	return cmd
}
