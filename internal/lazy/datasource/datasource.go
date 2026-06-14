// Package datasource is the only seam between the lazy TUI and the
// rest of autosk. Contexts, controllers, and helpers never reach into
// internal/store, internal/daemon/rpcclient, or internal/workflow
// directly — they go through a Datasource.
//
// There is a single implementation, RPC (rpc.go): every read, write, and the
// live transcript tail route to autoskd (the daemon that solely owns the
// project's .autosk store) over the JSON-RPC client (internal/daemon/rpcclient),
// which auto-spawns the daemon on first use. The Go binary opens no store of
// its own — it is a pure RPC client.
//
// All verbs are context-cancellable and MUST return within a few
// hundred milliseconds. The live transcript stream (session.subscribe) is
// expressed as a dedicated StreamSession() method that returns a handle
// the caller can close.
package datasource

import (
	"context"
	"errors"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
)

// ErrDaemonRequired is the sentinel for verbs that cannot be answered without a
// reachable daemon. With the single RPC datasource every verb requires the
// daemon, so this is no longer returned in production; it survives as the error
// the TUI's fake datasources use as a stand-in in tests.
var ErrDaemonRequired = errors.New("daemon required")

// TaskRef is a lightweight reference to a related task carrying just
// enough metadata for the detail pane to render the id with the right
// status hue without re-querying the store. Used in Task.BlockedBy
// and Task.Blocks.
type TaskRef struct {
	ID     string
	Status store.Status
}

// Task is the lazy TUI's projection of a task row. Derived fields
// (WorkflowName, StepName, Blocked, CommentCount) are resolved by
// the datasource so the TUI never joins by hand. v2 drops Priority,
// AuthorID, AuthorName, WorkflowID, CurrentStepID, AgentName, Metadata.
type Task struct {
	ID           string
	Title        string
	Description  string
	Status       store.Status
	WorkflowName string // workflow name (from api Workflow)
	StepName     string // current step name (from api Step)
	Blocked      bool
	BlockedBy    []TaskRef // every blocker (open and closed), in store-order
	Blocks       []TaskRef // every task this task blocks (open and closed)
	CommentCount int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session is the lazy TUI's projection of a session (replaces Job).
// Built from api.SessionMeta with derived fields for rendering.
type Session struct {
	ID        string
	TaskID    string
	Workflow  string
	Step      string
	Agent     string
	Status    runstore.RunStatus
	Error     string
	StartedAt *time.Time
	EndedAt   *time.Time
}

// Workflow is the read-only projection of a workflow from code (api.WorkflowInfo).
// v2 drops ID, IsSynthetic, TaskCount, NonTerminalTaskCount, NonTerminalTasks.
type Workflow struct {
	Name        string
	Description string
	FirstStep   string
	Steps       []WorkflowStep
	Isolation   string
}

// NOTE: NonTerminalTaskSampleSize, NonTerminalTaskRef removed - v2 workflows are read-only

// WorkflowStep is one step of a workflow rendered from code.
type WorkflowStep struct {
	Name string
	// Status is the terminal/park status for a statusStep ("done"/"cancel"/
	// "human"), or "" for an agent step (whose Name is the agent name).
	Status  string
	Targets []string // step names or status values from Targets
}

// Agent is the read-only projection of an agent. v2 agents are inline step
// values (the step key is the agent name), so this is derived from the
// registered workflows' agent steps rather than a dedicated registry view.
type Agent struct {
	Name string
}

// Comment mirrors api.Comment for rendering convenience.
type Comment struct {
	ID        string // v2 uses string IDs (cm-...)
	Author    string // single rendered string
	Text      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NOTE: Signal type removed - v2 has no signals

// Health describes the datasource's view of the daemon.
// v2 Health dropped DBPath/ProjectRoot scoped fields.
type Health struct {
	Daemon    string // "ok" | "down" | "stale"
	Workers   int
	Queued    int
	Running   int
	UpdatedAt time.Time
}

// IsOK reports whether daemon is reachable and healthy.
func (h Health) IsOK() bool { return h.Daemon == "ok" }

// LiveEvent is one session-event frame from StreamSession.
type LiveEvent struct {
	// Kind is "message" | "status" | "done" | "error".
	Kind    string
	Line    api.TranscriptLine // populated when Kind == "message"
	Session api.SessionMeta    // populated when Kind == "status" or "done"
	Err     error              // populated when Kind == "error"
	LineNum int                // line number from transcript
}

// LiveHandle is the active session-event subscription returned by StreamSession.
type LiveHandle struct {
	Events <-chan LiveEvent
	close  func() error
}

// Close terminates the SSE stream. Idempotent.
func (h *LiveHandle) Close() error {
	if h == nil || h.close == nil {
		return nil
	}
	return h.close()
}

// NewLiveHandle constructs a LiveHandle; used by StreamSession (rpc.go) and the
// TUI's live-pump tests.
func NewLiveHandle(ch <-chan LiveEvent, closeFn func() error) *LiveHandle {
	return &LiveHandle{Events: ch, close: closeFn}
}

// ChangeEvent is one daemon push delivered by a Watcher: a signal that the
// project's task state (Kind=="task"), its workflow/agent registry
// (Kind=="project"), or one of its sessions (Kind=="session") changed and the
// dashboard should re-fetch. It carries no diff — the consumer re-reads the
// affected panels. Backed by the daemon's task-changed / project-changed /
// session-changed notifications (plan §5).
type ChangeEvent struct {
	Kind string // "task" | "project" | "session"
}

// WatchHandle is the active task-changed/project-changed subscription returned
// by Watcher.Watch.
type WatchHandle struct {
	Events <-chan ChangeEvent
	close  func() error
}

// Close terminates the subscription. Idempotent.
func (h *WatchHandle) Close() error {
	if h == nil || h.close == nil {
		return nil
	}
	return h.close()
}

// NewWatchHandle constructs a WatchHandle; used by RPC.Watch (rpc.go) and the
// TUI's watch-loop tests.
func NewWatchHandle(ch <-chan ChangeEvent, closeFn func() error) *WatchHandle {
	return &WatchHandle{Events: ch, close: closeFn}
}

// Watcher is the OPTIONAL push-notification capability a Datasource may
// implement. The RPC datasource (rpc.go) implements it over the daemon's
// task.subscribe stream so the TUI refreshes on a server push instead of a
// fixed poll (plan §5: "replace lazy's client-side 2s poll with a server
// push"). A datasource that does NOT implement Watcher (e.g. the test fakes)
// makes the TUI fall back to its periodic safety re-sync. Kept separate from
// Datasource so adding push refresh does not force every fake to grow a method.
type Watcher interface {
	// Watch opens a task-changed/project-changed subscription. The handle
	// delivers a ChangeEvent on each daemon push until the stream ends (daemon
	// disconnect / subscribe error) or the caller Closes it. Returns promptly.
	Watch(ctx context.Context) (*WatchHandle, error)
}

// NOTE: UpdateIsolationReport, EnsureRecord, LeftoverWorktree removed - v2 workflows are read-only

// Datasource is the read+write contract the TUI talks through.
type Datasource interface {
	// ---- reads (return promptly; safe to call from g.OnWorker) ----

	Tasks(ctx context.Context, f TaskFilter) ([]Task, error)
	GetTask(ctx context.Context, id string) (Task, error)
	Sessions(ctx context.Context, taskID string) ([]Session, error) // replaces Jobs
	GetSession(ctx context.Context, id string) (Session, error)     // replaces GetJob
	Workflows(ctx context.Context) ([]Workflow, error)              // drops includeSynthetic
	Agents(ctx context.Context) ([]Agent, error)                    // derived from workflow agent steps
	Comments(ctx context.Context, taskID string) ([]Comment, error)
	Healthz(ctx context.Context) (Health, error)

	// ---- writes (task lifecycle) ----

	CreateTask(ctx context.Context, title, description string) (string, error) // drops priority
	// Status actions map to explicit verbs: done→TaskDone, cancel→TaskCancel, reopen→TaskReopen
	TaskDone(ctx context.Context, id string) error
	TaskCancel(ctx context.Context, id string) error
	TaskReopen(ctx context.Context, id string) error
	// UpdateTask replaces UpdateTitleDescription - uses pointers for optional updates
	UpdateTask(ctx context.Context, id string, title, description *string) error
	// EnrollWorkflow replaces Enroll - drops stepName/base-ref; v2 enroll has no step
	EnrollWorkflow(ctx context.Context, id, workflow string) error
	// Resume maps to Resume(id, nil) for "resume from human" or Resume(id, &StepTarget{Step:toStep})
	Resume(ctx context.Context, id, toStep string) error
	Block(ctx context.Context, id, blocker string) error
	Unblock(ctx context.Context, id, blocker string) error
	AddComment(ctx context.Context, taskID, text string) error

	// ---- workflow/agent writes removed - v2 workflows are read-only ----

	// ---- sessions (live only) ----

	AbortSession(ctx context.Context, id string) error                // replaces CancelJob/AbortJob
	SessionInput(ctx context.Context, id, message, kind string) error // replaces SendInput

	// Reconnect is a no-op for the RPC datasource: the daemon owns the
	// store, and the Go binary holds no connection of its own to drop.
	// It survives on the interface as a recovery lever the TUI's
	// Ctrl-R hard-refresh can call; daemon-only datasources treat it
	// as a no-op. Safe to call concurrently; idempotent.
	Reconnect(ctx context.Context) error

	// SessionTranscript fetches a session's full archived transcript as a slice
	// of LiveEvents (oldest first). Used for the Detail pane snapshot.
	SessionTranscript(ctx context.Context, sessionID string) ([]LiveEvent, error)

	// StreamSession opens a session.subscribe transcript tail. Caller must call
	// LiveHandle.Close. Backed by the daemon's persistent connection.
	StreamSession(ctx context.Context, sessionID string) (*LiveHandle, error)
}

// TaskFilter narrows Tasks results. v2 drops Priority, AgentName, AuthorName, StepAgentName.
// Keep status filter + search only. WorkflowID becomes WorkflowName.
type TaskFilter struct {
	Statuses     []store.Status
	WorkflowName string // workflow name filter
	StepName     string // step name filter
	Blocked      *bool  // blocked state filter
	Search       string // substring match on id / title (case-insensitive)
}

// DefaultTaskFilter returns the filter that mirrors `autosk list`
// default behaviour: open work (new/work/human).
func DefaultTaskFilter() TaskFilter {
	return TaskFilter{Statuses: store.OpenStatuses()}
}

// NOTE: JobFilter removed - Sessions() takes just taskID parameter
