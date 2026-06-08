package rpcclient

import (
	"context"
	"time"

	"autosk/internal/daemon/api"
)

// The structs below mirror autosk-proto::wire (the single source of truth for
// the JSON-RPC result shapes). Field tags match the Rust serde output; RFC3339
// timestamps decode straight into time.Time.

// Task is the enriched task view (autosk-proto::wire::TaskView).
type Task struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Description   string         `json:"description"`
	Status        string         `json:"status"`
	Priority      int            `json:"priority"`
	AuthorID      string         `json:"author_id"`
	AuthorName    string         `json:"author_name"`
	WorkflowID    string         `json:"workflow_id"`
	WorkflowName  string         `json:"workflow_name"`
	CurrentStepID string         `json:"current_step_id"`
	StepName      string         `json:"step_name"`
	AgentID       string         `json:"agent_id"`
	AgentName     string         `json:"agent_name"`
	Blocked       bool           `json:"blocked"`
	BlockedBy     []TaskRef      `json:"blocked_by"`
	Blocks        []TaskRef      `json:"blocks"`
	CommentCount  int            `json:"comment_count"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// TaskRef is a lightweight related-task reference.
type TaskRef struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Job mirrors api.JobResponse (same JSON tags) plus the derived label fields.
type Job struct {
	api.JobResponse
	WorkflowName string `json:"workflow_name"`
	StepName     string `json:"step_name"`
	AgentName    string `json:"agent_name"`
}

// Workflow mirrors autosk-proto::wire::Workflow.
type Workflow struct {
	ID                   string               `json:"id"`
	Name                 string               `json:"name"`
	Description          string               `json:"description"`
	IsSynthetic          bool                 `json:"is_synthetic"`
	FirstStep            string               `json:"first_step"`
	Steps                []WorkflowStep       `json:"steps"`
	TaskCount            int                  `json:"task_count"`
	Isolation            string               `json:"isolation"`
	NonTerminalTaskCount int                  `json:"non_terminal_task_count"`
	NonTerminalTasks     []NonTerminalTaskRef `json:"non_terminal_tasks"`
}

// WorkflowStep mirrors autosk-proto::wire::WorkflowStep.
type WorkflowStep struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	AgentName  string   `json:"agent_name"`
	NextSteps  []string `json:"next_steps"`
	NextStatus []string `json:"next_status"`
	TaskCount  int      `json:"task_count"`
	MaxVisits  int      `json:"max_visits"`
}

// NonTerminalTaskRef mirrors autosk-proto::wire::NonTerminalTaskRef.
type NonTerminalTaskRef struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	StepName string `json:"step_name"`
}

// Agent mirrors autosk-proto::wire::Agent.
type Agent struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	IsHuman    bool     `json:"is_human"`
	Source     string   `json:"source"`
	Version    string   `json:"version"`
	Model      string   `json:"model"`
	Thinking   string   `json:"thinking"`
	ExtraArgs  []string `json:"extra_args"`
	PiSkills   []string `json:"pi_skills"`
	PiExt      []string `json:"pi_ext"`
	TasksOwned int      `json:"tasks_owned"`
}

// Comment mirrors autosk-proto::wire::Comment.
type Comment struct {
	ID         int64     `json:"id"`
	TaskID     string    `json:"task_id"`
	AuthorID   string    `json:"author_id"`
	AuthorName string    `json:"author_name"`
	Text       string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
}

// Signal mirrors autosk-proto::wire::Signal.
type Signal struct {
	TransitionID int64     `json:"transition_id"`
	TaskID       string    `json:"task_id"`
	JobID        string    `json:"job_id"`
	StepID       string    `json:"step_id"`
	StepName     string    `json:"step_name"`
	WorkflowID   string    `json:"workflow_id"`
	WorkflowName string    `json:"workflow_name"`
	Target       string    `json:"target"`
	AgentID      string    `json:"agent_id"`
	AgentName    string    `json:"agent_name"`
	CreatedAt    time.Time `json:"created_at"`
}

// Health mirrors autosk-proto::wire::Health (= api.HealthResponse shape).
type Health struct {
	OK          bool            `json:"ok"`
	Workers     int             `json:"workers"`
	Queued      int             `json:"queued"`
	Running     int             `json:"running"`
	DBPath      string          `json:"db_path"`
	ProjectRoot string          `json:"project_root"`
	Projects    []HealthProject `json:"projects"`
}

// HealthProject mirrors autosk-proto::wire::HealthProject.
type HealthProject struct {
	Root     string    `json:"root"`
	DBPath   string    `json:"db_path"`
	Queued   int       `json:"queued"`
	Running  int       `json:"running"`
	OpenedAt time.Time `json:"opened_at"`
}

// Version mirrors autosk-proto::wire::VersionInfo.
type Version struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// TaskListFilter narrows Tasks. A nil Statuses sends no status filter (server
// default = open statuses); a non-nil empty slice means "all statuses".
type TaskListFilter struct {
	Statuses      []string
	Priority      *int
	WorkflowID    string
	AgentName     string
	AuthorName    string
	StepAgentName string
	Search        string
}

// JobListFilter narrows Jobs.
type JobListFilter struct {
	TaskID     string
	WorkflowID string
	Statuses   []string
	Limit      int
}

// ---- typed read methods (plan §5) -----------------------------------------

// Version returns the daemon's version/commit.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.call(ctx, "version", nil, &out)
	return out, err
}

// Healthz returns the daemon's health for this client's project selector.
func (c *Client) Healthz(ctx context.Context) (Health, error) {
	var out Health
	err := c.call(ctx, "healthz", c.selector(nil), &out)
	return out, err
}

// Tasks lists tasks matching the filter.
func (c *Client) Tasks(ctx context.Context, f TaskListFilter) ([]Task, error) {
	extra := map[string]any{}
	if f.Statuses != nil {
		extra["statuses"] = f.Statuses
	}
	if f.Priority != nil {
		extra["priority"] = *f.Priority
	}
	putStr(extra, "workflow_id", f.WorkflowID)
	putStr(extra, "agent_name", f.AgentName)
	putStr(extra, "author_name", f.AuthorName)
	putStr(extra, "step_agent_name", f.StepAgentName)
	putStr(extra, "search", f.Search)
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

// Ready returns the ready set (status='new', no open blocker).
func (c *Client) Ready(ctx context.Context, limit int) ([]Task, error) {
	var out []Task
	err := c.call(ctx, "task.ready", c.selector(map[string]any{"limit": limit}), &out)
	return out, err
}

// Comments returns a task's thread, oldest first.
func (c *Client) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	var out []Comment
	err := c.call(ctx, "comment.list", c.selector(map[string]any{"task_id": taskID}), &out)
	return out, err
}

// Workflows lists workflows (+ steps); includeSynthetic surfaces single:<agent>.
func (c *Client) Workflows(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	var out []Workflow
	err := c.call(ctx, "workflow.list", c.selector(map[string]any{"include_synthetic": includeSynthetic}), &out)
	return out, err
}

// GetWorkflow returns one workflow by name.
func (c *Client) GetWorkflow(ctx context.Context, name string) (Workflow, error) {
	var out Workflow
	err := c.call(ctx, "workflow.get", c.selector(map[string]any{"name": name}), &out)
	return out, err
}

// Agents lists agents + tasks-owned counts.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	err := c.call(ctx, "agent.list", c.selector(nil), &out)
	return out, err
}

// Jobs lists daemon_runs decorated with workflow/step/agent labels.
func (c *Client) Jobs(ctx context.Context, f JobListFilter) ([]Job, error) {
	extra := map[string]any{}
	putStr(extra, "task_id", f.TaskID)
	putStr(extra, "workflow_id", f.WorkflowID)
	if len(f.Statuses) > 0 {
		extra["statuses"] = f.Statuses
	}
	if f.Limit > 0 {
		extra["limit"] = f.Limit
	}
	var out []Job
	err := c.call(ctx, "job.list", c.selector(extra), &out)
	return out, err
}

// GetJob returns one decorated job.
func (c *Client) GetJob(ctx context.Context, id string) (Job, error) {
	var out Job
	err := c.call(ctx, "job.get", c.selector(map[string]any{"id": id}), &out)
	return out, err
}

// CancelJob cancels a running or queued job (SIGTERM→grace→SIGKILL for a live
// run; mark cancelled when only queued) and returns the decorated job.
func (c *Client) CancelJob(ctx context.Context, jobID string) (Job, error) {
	var out Job
	err := c.call(ctx, "job.cancel", c.selector(map[string]any{"job_id": jobID}), &out)
	return out, err
}

// HealthzAll returns the cross-project aggregate health (every loaded project),
// independent of this client's project selector.
func (c *Client) HealthzAll(ctx context.Context) (Health, error) {
	var out Health
	err := c.call(ctx, "healthz", map[string]any{"all": true}, &out)
	return out, err
}

// Messages returns a job's archived transcript events.
func (c *Client) Messages(ctx context.Context, jobID string, full bool, limit int) ([]api.MessageEvent, error) {
	extra := map[string]any{"job_id": jobID, "full": full}
	if limit > 0 {
		extra["limit"] = limit
	}
	var out []api.MessageEvent
	err := c.call(ctx, "job.messages", c.selector(extra), &out)
	return out, err
}

// SignalsForTask returns every step_signals row for a task, newest first.
func (c *Client) SignalsForTask(ctx context.Context, taskID string) ([]Signal, error) {
	var out []Signal
	err := c.call(ctx, "signal.forTask", c.selector(map[string]any{"task_id": taskID}), &out)
	return out, err
}

// SignalsForJob returns step_signals rows for one run, newest first.
func (c *Client) SignalsForJob(ctx context.Context, jobID string) ([]Signal, error) {
	var out []Signal
	err := c.call(ctx, "signal.forJob", c.selector(map[string]any{"job_id": jobID}), &out)
	return out, err
}

func putStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}
