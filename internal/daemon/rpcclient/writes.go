package rpcclient

import (
	"context"

	"autosk/internal/daemon/api"
)

// The proto-v2 write surface (plan §4). Every write carries the {cwd} selector
// only — v2 dropped db_path + the cli|lazy source discriminator.

// ProjectInit lays down the .autosk/{tasks,sessions,extensions} skeleton and
// registers the project in ~/.autosk/projects.json. Idempotent. No DB, no
// bootstrap seeding — workflows/agents ship as bundled extensions.
func (c *Client) ProjectInit(ctx context.Context) (ProjectInfo, error) {
	var out ProjectInfo
	err := c.call(ctx, "project.init", c.selector(nil), &out)
	return out, err
}

// ProjectAdd registers the project rooted at cwd (without laying down the
// skeleton). name is optional (default: basename of the root).
func (c *Client) ProjectAdd(ctx context.Context, name string) (ProjectInfo, error) {
	extra := map[string]any{}
	putStr(extra, "name", name)
	var out ProjectInfo
	err := c.call(ctx, "project.add", c.selector(extra), &out)
	return out, err
}

// CreateTask creates a task in status='new' and returns the enriched view.
func (c *Client) CreateTask(ctx context.Context, title, description string, blockedBy []string) (Task, error) {
	extra := map[string]any{"title": title}
	putStr(extra, "description", description)
	if len(blockedBy) > 0 {
		extra["blocked_by"] = blockedBy
	}
	var out Task
	err := c.call(ctx, "task.create", c.selector(extra), &out)
	return out, err
}

// UpdateTask applies a title/description patch. Nil fields are left unchanged.
func (c *Client) UpdateTask(ctx context.Context, id string, title, description *string) (Task, error) {
	extra := map[string]any{"id": id}
	if title != nil {
		extra["title"] = *title
	}
	if description != nil {
		extra["description"] = *description
	}
	var out Task
	err := c.call(ctx, "task.update", c.selector(extra), &out)
	return out, err
}

// EnrollWorkflow enrolls a task into a named workflow at its first step.
func (c *Client) EnrollWorkflow(ctx context.Context, id, workflow string) (Task, error) {
	var out Task
	err := c.call(ctx, "task.enroll", c.selector(map[string]any{"id": id, "workflow": workflow}), &out)
	return out, err
}

// Resume flips human→work, optionally relocating to a sibling step or status.
// A nil `to` resumes at the current step.
func (c *Client) Resume(ctx context.Context, id string, to *StepTarget) (Task, error) {
	extra := map[string]any{"id": id}
	if to != nil {
		extra["to"] = stepTargetParam(*to)
	}
	var out Task
	err := c.call(ctx, "task.resume", c.selector(extra), &out)
	return out, err
}

// TaskDone / TaskCancel / TaskReopen are the terminal/reopen verbs.
func (c *Client) TaskDone(ctx context.Context, id string) (Task, error) {
	return c.taskIDVerb(ctx, "task.done", id)
}
func (c *Client) TaskCancel(ctx context.Context, id string) (Task, error) {
	return c.taskIDVerb(ctx, "task.cancel", id)
}
func (c *Client) TaskReopen(ctx context.Context, id string) (Task, error) {
	return c.taskIDVerb(ctx, "task.reopen", id)
}

func (c *Client) taskIDVerb(ctx context.Context, method, id string) (Task, error) {
	var out Task
	err := c.call(ctx, method, c.selector(map[string]any{"id": id}), &out)
	return out, err
}

// Block adds a single blocker edge.
func (c *Client) Block(ctx context.Context, id, blocker string) (Task, error) {
	var out Task
	err := c.call(ctx, "task.block", c.selector(map[string]any{"id": id, "blocked_by": blocker}), &out)
	return out, err
}

// Unblock removes a single blocker edge.
func (c *Client) Unblock(ctx context.Context, id, blocker string) (Task, error) {
	var out Task
	err := c.call(ctx, "task.unblock", c.selector(map[string]any{"id": id, "blocked_by": blocker}), &out)
	return out, err
}

// AddComment inserts a comment authored by `author` (empty → "human").
func (c *Client) AddComment(ctx context.Context, taskID, author, text string) (Comment, error) {
	extra := map[string]any{"task_id": taskID, "text": text}
	putStr(extra, "author", author)
	var out Comment
	err := c.call(ctx, "task.comment.add", c.selector(extra), &out)
	return out, err
}

// EditComment rewrites a comment's text.
func (c *Client) EditComment(ctx context.Context, taskID, commentID, text string) (Comment, error) {
	var out Comment
	err := c.call(ctx, "task.comment.edit",
		c.selector(map[string]any{"task_id": taskID, "comment_id": commentID, "text": text}), &out)
	return out, err
}

// DeleteComment removes a comment.
func (c *Client) DeleteComment(ctx context.Context, taskID, commentID string) error {
	return c.call(ctx, "task.comment.delete",
		c.selector(map[string]any{"task_id": taskID, "comment_id": commentID}), nil)
}

// SessionInput dispatches a steer/followup message to a live session. kind is
// "steer" | "followup".
func (c *Client) SessionInput(ctx context.Context, id, message, kind string) error {
	return c.call(ctx, "session.input",
		c.selector(map[string]any{"id": id, "message": message, "kind": kind}), nil)
}

// SessionAbort requests a graceful abort of a live session.
func (c *Client) SessionAbort(ctx context.Context, id string) error {
	return c.call(ctx, "session.abort", c.selector(map[string]any{"id": id}), nil)
}

// Shutdown asks the daemon to stop (forces idle-shutdown).
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, "meta.shutdown", nil, nil)
}

// InstallExtension installs an extension (`npm:<spec>` or a local path) into the
// global scope, or the project (`-l/--local`). A global install does not require
// an open project; the {cwd} selector is still sent so a relative local path
// resolves against the caller's directory.
func (c *Client) InstallExtension(ctx context.Context, source string, local bool) (ExtensionInstallResult, error) {
	extra := map[string]any{"source": source}
	if local {
		extra["local"] = true
	}
	var out ExtensionInstallResult
	err := c.call(ctx, "extension.install", c.selector(extra), &out)
	return out, err
}

// RemoveExtension drops an extension entry from the right scope's settings.json
// (match by name for npm, by path for local). node_modules is left untouched.
func (c *Client) RemoveExtension(ctx context.Context, source string, local bool) (ExtensionRemoveResult, error) {
	extra := map[string]any{"source": source}
	if local {
		extra["local"] = true
	}
	var out ExtensionRemoveResult
	err := c.call(ctx, "extension.remove", c.selector(extra), &out)
	return out, err
}

// ListExtensions lists the global + project settings entries (classified, with a
// resolved flag). Tolerant of a cwd outside any project (global scope only).
func (c *Client) ListExtensions(ctx context.Context) (ExtensionListResult, error) {
	var out ExtensionListResult
	err := c.call(ctx, "extension.list", c.selector(nil), &out)
	return out, err
}

// stepTargetParam renders a StepTarget into its wire object ({step} XOR
// {status}).
func stepTargetParam(t api.StepTarget) map[string]any {
	if t.Step != "" {
		return map[string]any{"step": t.Step}
	}
	return map[string]any{"status": t.Status}
}
