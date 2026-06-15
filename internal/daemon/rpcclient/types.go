package rpcclient

import (
	"context"

	"autosk/internal/daemon/api"
)

// The proto-v2 view types are canonically defined in internal/daemon/api
// (mirroring daemon/sdk/src/{types,transcript,proto}.ts). These aliases let
// callers keep referring to rpcclient.<Type> while the wire shapes live in one
// place.
type (
	Task               = api.TaskView
	TaskRef            = api.TaskRef
	Comment            = api.Comment
	Session            = api.SessionMeta
	WorkflowInfo       = api.WorkflowInfo
	WorkflowStepInfo   = api.WorkflowStepInfo
	StepTarget         = api.StepTarget
	Health             = api.Health
	HealthProject      = api.HealthProject
	Version            = api.VersionInfo
	ProjectInfo        = api.ProjectInfo
	ProjectDiagnostics = api.ProjectDiagnostics
	TranscriptLine     = api.TranscriptLine
	TranscriptResult   = api.SessionTranscriptResult

	ExtensionInstallResult = api.ExtensionInstallResult
	ExtensionRemoveResult  = api.ExtensionRemoveResult
	ExtensionEntryInfo     = api.ExtensionEntryInfo
	ExtensionListResult    = api.ExtensionListResult
)

// TaskListFilter narrows Tasks. A nil OR empty Statuses sends no status filter
// (the daemon returns tasks of every status); a non-empty slice filters to those
// statuses. Callers use an empty slice to mean "all" (the lazy dashboard, the
// CLI `--status all`), so an empty list must NOT be forwarded as `status: []`
// (which the daemon would read as "match none"). v2 has no
// priority/author/search/limit filters.
type TaskListFilter struct {
	Statuses []string
	Workflow string
	Step     string
	Blocked  *bool
}

// ---- typed read methods ---------------------------------------------------

// Version returns the daemon's version/commit.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.call(ctx, "meta.version", nil, &out)
	return out, err
}

// Healthz returns the daemon's cross-project aggregate health.
func (c *Client) Healthz(ctx context.Context) (Health, error) {
	var out Health
	err := c.call(ctx, "meta.healthz", nil, &out)
	return out, err
}

// Tasks lists tasks matching the filter.
func (c *Client) Tasks(ctx context.Context, f TaskListFilter) ([]Task, error) {
	filter := map[string]any{}
	// Only send a positive status constraint. An empty (or nil) slice means "all
	// statuses", which on the wire is the ABSENCE of the `status` key — sending
	// `status: []` would make the daemon's membership filter match nothing.
	if len(f.Statuses) > 0 {
		filter["status"] = f.Statuses
	}
	putStr(filter, "workflow", f.Workflow)
	putStr(filter, "step", f.Step)
	if f.Blocked != nil {
		filter["blocked"] = *f.Blocked
	}
	extra := map[string]any{}
	if len(filter) > 0 {
		extra["filter"] = filter
	}
	var out []Task
	err := c.call(ctx, "task.list", c.selector(extra), &out)
	return out, err
}

// GetTask returns one enriched task.
func (c *Client) GetTask(ctx context.Context, id string) (Task, error) {
	var out Task
	err := c.call(ctx, "task.get", c.selector(map[string]any{"id": id}), &out)
	return out, err
}

// Comments returns a task's thread, oldest first.
func (c *Client) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	var out []Comment
	err := c.call(ctx, "task.comment.list", c.selector(map[string]any{"task_id": taskID}), &out)
	return out, err
}

// Workflows lists the project's registered workflows (read-only registry view).
func (c *Client) Workflows(ctx context.Context) ([]WorkflowInfo, error) {
	var out []WorkflowInfo
	err := c.call(ctx, "registry.workflow.list", c.selector(nil), &out)
	return out, err
}

// GetWorkflow returns one workflow by name.
func (c *Client) GetWorkflow(ctx context.Context, name string) (WorkflowInfo, error) {
	var out WorkflowInfo
	err := c.call(ctx, "registry.workflow.get", c.selector(map[string]any{"name": name}), &out)
	return out, err
}

// Sessions lists a task's sessions (taskID empty → every session in the
// project), newest-id last.
func (c *Client) Sessions(ctx context.Context, taskID string) ([]Session, error) {
	extra := map[string]any{}
	putStr(extra, "task_id", taskID)
	var out []Session
	err := c.call(ctx, "session.list", c.selector(extra), &out)
	return out, err
}

// GetSession returns one session's metadata.
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	var out Session
	err := c.call(ctx, "session.get", c.selector(map[string]any{"id": id}), &out)
	return out, err
}

// Transcript returns a slice of a session's pi-format transcript starting at
// fromLine (1-based; 0 → from the start), up to limit lines (0 → no limit).
func (c *Client) Transcript(ctx context.Context, id string, fromLine, limit int) (TranscriptResult, error) {
	extra := map[string]any{"id": id}
	if fromLine > 0 {
		extra["from_line"] = fromLine
	}
	if limit > 0 {
		extra["limit"] = limit
	}
	var out TranscriptResult
	err := c.call(ctx, "session.transcript", c.selector(extra), &out)
	return out, err
}

// Projects lists every project in the daemon's registry (~/.autosk/projects.json).
func (c *Client) Projects(ctx context.Context) ([]ProjectInfo, error) {
	var out []ProjectInfo
	err := c.call(ctx, "project.list", nil, &out)
	return out, err
}

// Diagnostics returns the project's extension load errors.
func (c *Client) Diagnostics(ctx context.Context) (ProjectDiagnostics, error) {
	var out ProjectDiagnostics
	err := c.call(ctx, "project.diagnostics", c.selector(nil), &out)
	return out, err
}

func putStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}
