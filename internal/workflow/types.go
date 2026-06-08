package workflow

import "time"

// This file holds the materialised workflow view types. They describe
// the shape of workflow / step / transition data as it crosses the
// JSON-RPC boundary from autoskd and is rendered by the Go CLI + lazy
// TUI. The Go SQL store that used to populate them lives in the Rust
// daemon (autosk-core) now; the front ends only ever consume these
// types, never write them.

// Workflow is the materialised view of one `workflows` row.
type Workflow struct {
	ID          string
	Name        string
	Description string
	FirstStepID string
	IsSynthetic bool
	// Isolation is the workflow-level execution-isolation mode. See
	// docs/plans/20260521-Worktree-Isolation.md. Empty collapses to
	// IsolationNone so callers can compare against IsolationNone /
	// IsolationWorktree directly.
	Isolation IsolationMode
	CreatedAt time.Time
	Steps     []Step // populated by detail reads, not by list
}

// Step mirrors one `steps` row plus its outgoing transitions.
type Step struct {
	ID          string
	WorkflowID  string
	Name        string
	AgentID     string
	AgentName   string       // joined-in for convenience
	AgentParams *AgentParams // nil = use the package's defaults verbatim
	MaxVisits   int          // 0 = unlimited; see docs/workflows.md
	Transitions []Transition // outgoing, in source order
}

// Transition mirrors one `step_transitions` row.
type Transition struct {
	ID         int64
	StepID     string
	NextStepID string // empty when TaskStatus is set
	TaskStatus string // empty when NextStepID is set
	PromptRule string
	// NextStepName populated as a convenience when reading.
	NextStepName string
}

// IsTaskStatus reports whether this transition terminates / parks the
// workflow rather than advancing to a sibling step.
func (t Transition) IsTaskStatus() bool { return t.TaskStatus != "" }

// EnsureRecord describes one worktree allocation produced by a
// `none → worktree --force` flip. Existing reports whether the
// directory was already on disk (no-op Ensure) at the time of the run.
type EnsureRecord struct {
	TaskID   string `json:"task_id"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

// LeftoverWorktree describes one (taskID, path) pair surfaced by a
// `worktree → none --force` flip.
type LeftoverWorktree struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}

// UpdateIsolationReport carries the structured outcome of a
// `workflow update --isolation` flip as reported by autoskd.
type UpdateIsolationReport struct {
	Workflow          string             `json:"workflow"`
	From              IsolationMode      `json:"from"`
	To                IsolationMode      `json:"to"`
	Noop              bool               `json:"noop"`
	DryRun            bool               `json:"dry_run"`
	NonTerminalTasks  []string           `json:"non_terminal_tasks,omitempty"`
	EnsuredTasks      []EnsureRecord     `json:"ensured_tasks,omitempty"`
	LeftoverWorktrees []LeftoverWorktree `json:"leftover_worktrees,omitempty"`
	RolledBackEnsures []EnsureRecord     `json:"rolled_back_ensures,omitempty"`
	FailedTask        string             `json:"failed_task,omitempty"`
}
