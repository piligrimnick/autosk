package rpcclient

import (
	"context"
	"encoding/json"
)

// The Phase 3 write surface (plan §5). Every write carries the project selector
// plus a `source` discriminator ("cli" | "lazy") so the daemon reproduces the
// exact dolt_commit message + source-specific behaviour of the front end that
// issued it.

// TaskCreateParams mirrors the daemon's task.create params.
type TaskCreateParams struct {
	Source      string
	Title       string
	Description string
	Priority    *int
	Blocks      []string
	BlockedBy   []string
	Workflow    string
	Agent       string
	Step        string
	BaseRef     string
	Caller      string
}

// CreateTask creates a task (optionally entering a workflow) and returns the
// enriched view.
func (c *Client) CreateTask(ctx context.Context, p TaskCreateParams) (Task, error) {
	extra := map[string]any{"source": p.Source, "title": p.Title}
	putStr(extra, "description", p.Description)
	if p.Priority != nil {
		extra["priority"] = *p.Priority
	}
	if len(p.Blocks) > 0 {
		extra["blocks"] = p.Blocks
	}
	if len(p.BlockedBy) > 0 {
		extra["blocked_by"] = p.BlockedBy
	}
	putStr(extra, "workflow", p.Workflow)
	putStr(extra, "agent", p.Agent)
	putStr(extra, "step", p.Step)
	putStr(extra, "base_ref", p.BaseRef)
	putStr(extra, "caller", p.Caller)
	var out Task
	err := c.call(ctx, "task.create", c.selector(extra), &out)
	return out, err
}

// UpdateTask applies the CLI `update` field/status patch (one commit).
func (c *Client) UpdateTask(ctx context.Context, id string, title, description *string, priority *int, status *string) (Task, error) {
	extra := map[string]any{"id": id}
	if title != nil {
		extra["title"] = *title
	}
	if description != nil {
		extra["description"] = *description
	}
	if priority != nil {
		extra["priority"] = *priority
	}
	if status != nil {
		extra["status"] = *status
	}
	var out Task
	err := c.call(ctx, "task.update", c.selector(extra), &out)
	return out, err
}

// TaskDone / TaskCancel / TaskReopen are the CLI terminal verbs.
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

// SetStatus is lazy's status flip (commit `lazy: status <id>=<s>`).
func (c *Client) SetStatus(ctx context.Context, id, status string) (Task, error) {
	var out Task
	err := c.call(ctx, "task.setStatus", c.selector(map[string]any{"id": id, "status": status}), &out)
	return out, err
}

// SetTitleDescription / SetPriority are lazy's field edits.
func (c *Client) SetTitleDescription(ctx context.Context, id, title, description string) (Task, error) {
	var out Task
	err := c.call(ctx, "task.setTitleDescription",
		c.selector(map[string]any{"id": id, "title": title, "description": description}), &out)
	return out, err
}
func (c *Client) SetPriority(ctx context.Context, id string, p int) (Task, error) {
	var out Task
	err := c.call(ctx, "task.setPriority", c.selector(map[string]any{"id": id, "priority": p}), &out)
	return out, err
}

// Enroll (re-)attaches a task to a workflow's entry step.
func (c *Client) Enroll(ctx context.Context, source, id, workflow, agent, step, baseRef string) (Task, error) {
	extra := map[string]any{"source": source, "id": id}
	putStr(extra, "workflow", workflow)
	putStr(extra, "agent", agent)
	putStr(extra, "step", step)
	putStr(extra, "base_ref", baseRef)
	var out Task
	err := c.call(ctx, "task.enroll", c.selector(extra), &out)
	return out, err
}

// Resume flips human→work, optionally relocating the current step.
func (c *Client) Resume(ctx context.Context, source, id, toStep string) (Task, error) {
	extra := map[string]any{"source": source, "id": id}
	putStr(extra, "to_step", toStep)
	var out Task
	err := c.call(ctx, "task.resume", c.selector(extra), &out)
	return out, err
}

// Block / Unblock add or remove blocker edges.
func (c *Client) Block(ctx context.Context, source, id string, blockers []string) error {
	return c.call(ctx, "task.block", c.selector(map[string]any{"source": source, "id": id, "blockers": blockers}), nil)
}
func (c *Client) Unblock(ctx context.Context, source, id string, blockers []string) error {
	return c.call(ctx, "task.unblock", c.selector(map[string]any{"source": source, "id": id, "blockers": blockers}), nil)
}

// UnblockAll removes every incoming blocker edge.
func (c *Client) UnblockAll(ctx context.Context, id string) (int, error) {
	var out struct {
		Removed int `json:"removed"`
	}
	err := c.call(ctx, "task.unblockAll", c.selector(map[string]any{"id": id}), &out)
	return out.Removed, err
}

// MetaResult is the {task, changed} envelope of the metadata verbs.
type MetaResult struct {
	Task    Task `json:"task"`
	Changed bool `json:"changed"`
}

// MetadataSet sets a dotted key (CLI) or replaces the whole blob (lazy,
// replaceAll=true).
func (c *Client) MetadataSet(ctx context.Context, source, id, key string, value any, replaceAll bool) (MetaResult, error) {
	extra := map[string]any{"source": source, "id": id, "value": value, "replace_all": replaceAll}
	putStr(extra, "key", key)
	var out MetaResult
	err := c.call(ctx, "task.metadata.set", c.selector(extra), &out)
	return out, err
}

// MetadataUnset removes a dotted key.
func (c *Client) MetadataUnset(ctx context.Context, id, key string) (MetaResult, error) {
	var out MetaResult
	err := c.call(ctx, "task.metadata.unset", c.selector(map[string]any{"id": id, "key": key}), &out)
	return out, err
}

// MetadataResetVisits clears step counters.
func (c *Client) MetadataResetVisits(ctx context.Context, id, step, stepID string) (MetaResult, error) {
	extra := map[string]any{"id": id}
	putStr(extra, "step", step)
	putStr(extra, "step_id", stepID)
	var out MetaResult
	err := c.call(ctx, "task.metadata.resetVisits", c.selector(extra), &out)
	return out, err
}

// AddComment inserts a comment authored by `author` (empty → human).
func (c *Client) AddComment(ctx context.Context, source, taskID, author, text string) (Comment, error) {
	extra := map[string]any{"source": source, "task_id": taskID, "text": text}
	putStr(extra, "author", author)
	var out Comment
	err := c.call(ctx, "comment.add", c.selector(extra), &out)
	return out, err
}

// WorkflowCreate persists a workflow from a file path or inline JSON.
func (c *Client) WorkflowCreate(ctx context.Context, source, file, json string, noInstall bool) (string, error) {
	extra := map[string]any{"source": source, "no_install": noInstall}
	putStr(extra, "file", file)
	putStr(extra, "json", json)
	var out struct {
		Name string `json:"name"`
	}
	err := c.call(ctx, "workflow.create", c.selector(extra), &out)
	return out.Name, err
}

// WorkflowDelete removes a workflow by name.
func (c *Client) WorkflowDelete(ctx context.Context, source, name string) error {
	return c.call(ctx, "workflow.delete", c.selector(map[string]any{"source": source, "name": name}), nil)
}

// WorkflowUpdateIsolation flips the isolation column. On failure it still
// returns the partial force-safety report (the daemon embeds it in the error
// details), so callers can render the rollback/leftover diagnostics that Go's
// `(out, err)` path showed.
func (c *Client) WorkflowUpdateIsolation(ctx context.Context, source, name, mode string, force, dryRun bool) (UpdateIsolationReport, error) {
	extra := map[string]any{"source": source, "name": name, "mode": mode, "force": force, "dry_run": dryRun}
	var out UpdateIsolationReport
	err := c.call(ctx, "workflow.updateIsolation", c.selector(extra), &out)
	if err != nil {
		if apiErr, ok := IsAPIError(err); ok && apiErr.Details != nil {
			if rep, has := apiErr.Details["report"]; has {
				if b, mErr := json.Marshal(rep); mErr == nil {
					_ = json.Unmarshal(b, &out)
				}
			}
		}
	}
	return out, err
}

// AgentInstall installs an agent package + ensures its DB row.
func (c *Client) AgentInstall(ctx context.Context, name, version string) (Agent, error) {
	extra := map[string]any{"name": name}
	putStr(extra, "version", version)
	var out Agent
	err := c.call(ctx, "agent.install", c.selector(extra), &out)
	return out, err
}

// AgentUninstall removes an agent package (no DB commit).
func (c *Client) AgentUninstall(ctx context.Context, name string, force bool) error {
	return c.call(ctx, "agent.uninstall", c.selector(map[string]any{"name": name, "force": force}), nil)
}

// SQLRows is a sql.query result.
type SQLRows struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// SQLQuery runs a read-only query.
func (c *Client) SQLQuery(ctx context.Context, query string) (SQLRows, error) {
	var out SQLRows
	err := c.call(ctx, "sql.query", c.selector(map[string]any{"query": query}), &out)
	return out, err
}

// SQLExec runs a write statement; returns rows affected.
func (c *Client) SQLExec(ctx context.Context, query string) (int64, error) {
	var out struct {
		RowsAffected int64 `json:"rows_affected"`
	}
	err := c.call(ctx, "sql.exec", c.selector(map[string]any{"query": query}), &out)
	return out.RowsAffected, err
}

// ProjectInitResult is the project.init outcome.
type ProjectInitResult struct {
	Root         string  `json:"root"`
	DBPath       string  `json:"db_path"`
	Bootstrapped *string `json:"bootstrapped"`
}

// ProjectInit migrates + bootstraps a project (creating the DB if needed).
func (c *Client) ProjectInit(ctx context.Context, skipBootstrap bool) (ProjectInitResult, error) {
	var out ProjectInitResult
	err := c.call(ctx, "project.init", c.selector(map[string]any{"skip_bootstrap": skipBootstrap}), &out)
	return out, err
}

// SendInput dispatches steer/follow_up/prompt to a running job.
func (c *Client) SendInput(ctx context.Context, jobID, message, behavior string) (string, error) {
	extra := map[string]any{"job_id": jobID, "message": message}
	putStr(extra, "streaming_behavior", behavior)
	var out struct {
		Dispatched string `json:"dispatched"`
	}
	err := c.call(ctx, "job.input", c.selector(extra), &out)
	return out.Dispatched, err
}

// AbortJob requests a graceful abort of a running job.
func (c *Client) AbortJob(ctx context.Context, jobID string) error {
	return c.call(ctx, "job.abort", c.selector(map[string]any{"job_id": jobID}), nil)
}

// Shutdown asks the daemon to stop (forces idle-shutdown).
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, "shutdown", nil, nil)
}

// UpdateIsolationReport mirrors autosk-proto::wire::UpdateIsolationReport.
type UpdateIsolationReport struct {
	Workflow          string             `json:"workflow"`
	From              string             `json:"from"`
	To                string             `json:"to"`
	Noop              bool               `json:"noop"`
	DryRun            bool               `json:"dry_run"`
	NonTerminalTasks  []string           `json:"non_terminal_tasks"`
	EnsuredTasks      []EnsureRecord     `json:"ensured_tasks"`
	LeftoverWorktrees []LeftoverWorktree `json:"leftover_worktrees"`
	RolledBackEnsures []EnsureRecord     `json:"rolled_back_ensures"`
	FailedTask        string             `json:"failed_task"`
}

// EnsureRecord / LeftoverWorktree mirror the wire types of the same name.
type EnsureRecord struct {
	TaskID   string `json:"task_id"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

type LeftoverWorktree struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}
