package main

import (
	"os"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
	"autosk/internal/store"
)

// This file adapts the daemon's enriched wire view (rpcclient.Task =
// api.TaskView) onto the render package. The v2 TaskView already carries the
// workflow/step names + derived blocked/blocked_by/blocks/comment_count, so the
// adapter is a thin projection.

// taskFromWire converts a daemon TaskView into the render-shaped store.Task.
func taskFromWire(w rpcclient.Task) store.Task {
	return store.Task{
		ID:          w.ID,
		Title:       w.Title,
		Description: w.Description,
		Status:      store.Status(w.Status),
		Workflow:    w.Workflow,
		Step:        w.Step,
		Metadata:    w.Metadata,
		CreatedAt:   w.CreatedAt,
		UpdatedAt:   w.UpdatedAt,
	}
}

// wireRefIDs projects a slice of TaskRef onto their ids, preserving order.
func wireRefIDs(refs []rpcclient.TaskRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

// wireBlockedOpt builds the derived blocked / blocked_by / blocks render.Option
// from the wire view (the daemon computes these joins).
func wireBlockedOpt(w rpcclient.Task) render.Option {
	return render.WithBlocked(w.Blocked, wireRefIDs(w.BlockedBy), wireRefIDs(w.Blocks))
}

// emitTaskWire renders a single task to stdout (human or --json) from a wire
// view, always layering the derived blocked edges + comment count and any
// caller-supplied extras. Honours --quiet.
func emitTaskWire(w rpcclient.Task, extra ...render.Option) error {
	if flagQuiet {
		return nil
	}
	opts := append([]render.Option{wireBlockedOpt(w), render.WithCommentCount(w.CommentCount)}, extra...)
	t := taskFromWire(w)
	if flagJSON {
		return render.TaskJSONTo(os.Stdout, t, opts...)
	}
	return render.Task(os.Stdout, t, opts...)
}

// tasksFromWire converts a slice of wire views for the list/ready renderers.
func tasksFromWire(ws []rpcclient.Task) []store.Task {
	ts := make([]store.Task, 0, len(ws))
	for _, w := range ws {
		ts = append(ts, taskFromWire(w))
	}
	return ts
}

// wireListDecorator surfaces the derived blocked asterisk in the list/ready
// table + JSON array (the daemon already computed the flag).
func wireListDecorator(ws []rpcclient.Task) render.Decorator {
	blocked := make(map[string]rpcclient.Task, len(ws))
	for _, w := range ws {
		blocked[w.ID] = w
	}
	return func(tj *render.TaskJSON) {
		if w, ok := blocked[tj.ID]; ok {
			wireBlockedOpt(w)(tj)
			render.WithCommentCount(w.CommentCount)(tj)
		}
	}
}
