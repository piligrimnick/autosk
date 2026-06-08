package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/meta"
	"autosk/internal/render"
	"autosk/internal/store"
)

// This file adapts the daemon's enriched wire views (rpcclient.Task) onto
// the doltlite-free render package (which consumes store.Task + render
// Options). Routing every CLI verb's output through internal/render this
// way keeps the human + --json formats byte-identical to the pre-daemon
// path while the data now comes over JSON-RPC.

// taskFromWire converts a daemon TaskView into the storage-shaped
// store.Task the renderer marshals. Derived display names
// (workflow/step/agent/author names, blocked edges) are NOT stored on
// store.Task; they are layered on via wireNameOpts / wireBlockedOpt.
func taskFromWire(w rpcclient.Task) store.Task {
	return store.Task{
		ID:            w.ID,
		Title:         w.Title,
		Description:   w.Description,
		Status:        store.Status(w.Status),
		Priority:      w.Priority,
		AuthorID:      w.AuthorID,
		WorkflowID:    w.WorkflowID,
		CurrentStepID: w.CurrentStepID,
		Metadata:      w.Metadata,
		CreatedAt:     w.CreatedAt,
		UpdatedAt:     w.UpdatedAt,
	}
}

// wireRefIDs projects a slice of TaskRef onto their ids, preserving the
// daemon's order (which mirrors the old store.Deps ordering).
func wireRefIDs(refs []rpcclient.TaskRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

// wireNameOpts builds the human-friendly name render.Options from a wire
// view: derived current_step + current_agent (with the step agent's id),
// workflow name, and author name. Mirrors the old cmd/autosk
// taskRenderOpts, but every name now arrives pre-resolved on the wire.
func wireNameOpts(w rpcclient.Task) []render.Option {
	var opts []render.Option
	if w.CurrentStepID != "" && w.StepName != "" {
		opts = append(opts, render.WithStep(w.StepName, w.AgentName, w.AgentID))
	}
	if w.WorkflowID != "" && w.WorkflowName != "" {
		opts = append(opts, render.WithWorkflow(w.WorkflowName))
	}
	if w.AuthorID != "" && w.AuthorName != "" {
		opts = append(opts, render.WithAuthor(w.AuthorName))
	}
	return opts
}

// wireBlockedOpt builds the derived blocked / blocked_by / blocks
// render.Option from the wire view (the daemon computes these joins).
func wireBlockedOpt(w rpcclient.Task) render.Option {
	return render.WithBlocked(w.Blocked, wireRefIDs(w.BlockedBy), wireRefIDs(w.Blocks))
}

// emitTaskWire renders a single task to stdout (human or --json) from a
// wire view, layering the name opts and any caller-supplied extras
// (WithBlocked / WithMetadata / WithWorktree). Honours --quiet.
func emitTaskWire(w rpcclient.Task, extra ...render.Option) error {
	if flagQuiet {
		return nil
	}
	opts := append(wireNameOpts(w), extra...)
	t := taskFromWire(w)
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t, opts...)
	}
	return render.Task(os.Stdout, t, opts...)
}

// wireVisitsSummary builds the single-line step_visits summary for the
// human `autosk show` renderer from a wire workflow view (id → name +
// max_visits). Mirrors the pre-daemon renderVisitsSummary:
// `dev 3/5, review 5/5*, validator 1`.
func wireVisitsSummary(wf rpcclient.Workflow, md map[string]any) string {
	sv := meta.GetStepVisits(md)
	if len(sv) == 0 {
		return ""
	}
	byID := make(map[string]rpcclient.WorkflowStep, len(wf.Steps))
	for _, s := range wf.Steps {
		byID[s.ID] = s
	}
	ids := make([]string, 0, len(sv))
	for k := range sv {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		st, found := byID[id]
		label := st.Name
		if !found {
			label = "<unknown:" + id + ">"
		}
		var seg string
		if found && st.MaxVisits > 0 {
			marker := ""
			if sv[id] >= st.MaxVisits {
				marker = "*"
			}
			seg = fmt.Sprintf("%s %d/%d%s", label, sv[id], st.MaxVisits, marker)
		} else {
			seg = fmt.Sprintf("%s %d", label, sv[id])
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ", ")
}

// tasksFromWire converts a slice of wire views for the list/ready table
// + array renderers. The pre-daemon list/ready path rendered with a nil
// decorator (no derived blocked asterisk), so callers pass nil too.
func tasksFromWire(ws []rpcclient.Task) []store.Task {
	ts := make([]store.Task, 0, len(ws))
	for _, w := range ws {
		ts = append(ts, taskFromWire(w))
	}
	return ts
}
