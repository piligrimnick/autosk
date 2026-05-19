package tui

import (
	"context"
	"sync"
	"time"

	"autosk/internal/lazy/datasource"
)

// panelID is the identifier of one of the four dashboard list
// contexts. Used by the focus stack + the scope helper.
type panelID int

const (
	panelTasks panelID = iota
	panelJobs
	panelWorkflows
	panelAgents
	panelDetail // the right detail pane (cursor land for j/k scroll)
)

func (p panelID) String() string {
	switch p {
	case panelTasks:
		return "Tasks"
	case panelJobs:
		return "Jobs"
	case panelWorkflows:
		return "Workflows"
	case panelAgents:
		return "Agents"
	case panelDetail:
		return "Detail"
	}
	return "?"
}

func (p panelID) window() string {
	switch p {
	case panelTasks:
		return winTasks
	case panelJobs:
		return winJobs
	case panelWorkflows:
		return winWorkflows
	case panelAgents:
		return winAgents
	case panelDetail:
		return winDetail
	}
	return ""
}

// inspectorTab is the inspector's current focused tab.
type inspectorTab int

const (
	tabLive inspectorTab = iota
	tabArchive
	tabMeta
	tabSignals
)

func (t inspectorTab) String() string {
	switch t {
	case tabLive:
		return "Live"
	case tabArchive:
		return "Archive"
	case tabMeta:
		return "Meta"
	case tabSignals:
		return "Signals"
	}
	return "?"
}

// scope describes the cross-link scope chips active on the dashboard.
type scope struct {
	TaskID       string // narrows Jobs
	WorkflowID   string // narrows Tasks + Jobs
	WorkflowName string
	Agent        string // narrows Tasks (opt-in via Enter)
}

// IsEmpty reports whether no scope chip is active.
func (s scope) IsEmpty() bool {
	return s.TaskID == "" && s.WorkflowID == "" && s.Agent == ""
}

// filterState holds the per-panel filter strings (`/`).
type filterState struct {
	Tasks     string
	Jobs      string
	Workflows string
	Agents    string
}

// popupKind enumerates the active modal popup. None when zero-value.
type popupKind int

const (
	popupNone popupKind = iota
	popupMenu
	popupConfirm
	popupPrompt
	popupSearch
	popupHelp
)

// popupState is the runtime state of the current popup.
type popupState struct {
	Kind     popupKind
	Title    string
	Lines    []string         // for menu / help / search-results
	Cursor   int              // for menu
	Input    string           // for prompt / search
	OnAccept func(value string) error
	OnSelect func(index int) error
	OnCancel func() error
}

// inspectorState carries everything the inspector view needs.
type inspectorState struct {
	JobID         string
	Tab           inspectorTab
	Job           datasource.Job
	Streaming     bool
	TerminalAtOpen bool
	// Live tab plumbing.
	live          *datasource.LiveHandle
	liveCancel    context.CancelFunc
	liveBuf       []string  // pre-rendered transcript lines (oldest→newest)
	liveInput     string    // textarea contents (single-line in v1 — risk #3)
	// Archive tab plumbing.
	archive       []datasource.MessageEvent
	archiveLoaded bool
	// Meta + Signals.
	signals       []datasource.Signal
	comments      []datasource.Comment
	scrollY       int
}

// flashState is the ephemeral toast line. CreatedAt makes the layout
// loop drop the toast after a short timeout.
type flashState struct {
	Text      string
	Level     string // "info" | "warn" | "err"
	CreatedAt time.Time
}

// state is the entire mutable model of the TUI. Guarded by mu; the
// gocui main thread is the only writer (everything that mutates state
// goes through g.Update so we never need a goroutine-side mutex), but
// the read side is concurrent (e.g. status-bar render off the layout
// goroutine), so we keep the lock for safety.
type state struct {
	mu sync.RWMutex

	// Top-level mode.
	view ViewState

	// Focus.
	focused      panelID
	focusedStack []panelID // for Esc-pop semantics (popups remember the side panel)

	// Data caches. The View() is the source of truth for rendered
	// content — these are the most recent slice from the datasource so
	// the cursor positions are stable across re-renders.
	tasks         []datasource.Task
	jobs          []datasource.Job
	workflows     []datasource.Workflow
	agents        []datasource.Agent
	taskCursor    int
	jobCursor     int
	workflowCursor int
	agentCursor   int

	scope   scope
	filter  filterState
	popup   popupState
	insp    inspectorState
	flash   flashState
	logBuf  []string
	logHide bool

	health datasource.Health
}

// newState seeds an empty model with sensible defaults.
func newState() *state {
	return &state{
		focused: panelTasks,
		view:    StateDashboard,
		logBuf:  []string{"lazy started"},
		health:  datasource.Health{Daemon: "down"},
	}
}

// withLock runs f under the model lock. Helper for read sites that
// only need a snapshot of a few fields.
func (s *state) withLock(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *state) withRLock(f func()) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f()
}

// pushFocus moves focus to p, remembering the previous panel.
func (s *state) pushFocus(p panelID) {
	s.focusedStack = append(s.focusedStack, s.focused)
	s.focused = p
}

// popFocus moves back to the previous panel (or to Tasks).
func (s *state) popFocus() {
	if n := len(s.focusedStack); n > 0 {
		s.focused = s.focusedStack[n-1]
		s.focusedStack = s.focusedStack[:n-1]
		return
	}
	s.focused = panelTasks
}

// selectedTask returns the currently-highlighted task, or zero.
func (s *state) selectedTask() (datasource.Task, bool) {
	if len(s.tasks) == 0 || s.taskCursor < 0 || s.taskCursor >= len(s.tasks) {
		return datasource.Task{}, false
	}
	return s.tasks[s.taskCursor], true
}

func (s *state) selectedJob() (datasource.Job, bool) {
	if len(s.jobs) == 0 || s.jobCursor < 0 || s.jobCursor >= len(s.jobs) {
		return datasource.Job{}, false
	}
	return s.jobs[s.jobCursor], true
}

func (s *state) selectedWorkflow() (datasource.Workflow, bool) {
	if len(s.workflows) == 0 || s.workflowCursor < 0 || s.workflowCursor >= len(s.workflows) {
		return datasource.Workflow{}, false
	}
	return s.workflows[s.workflowCursor], true
}

func (s *state) selectedAgent() (datasource.Agent, bool) {
	if len(s.agents) == 0 || s.agentCursor < 0 || s.agentCursor >= len(s.agents) {
		return datasource.Agent{}, false
	}
	return s.agents[s.agentCursor], true
}

// appendLog adds a one-line entry to the command log with a relative
// timestamp.
func (s *state) appendLog(line string) {
	stamp := time.Now().Format("15:04")
	s.logBuf = append(s.logBuf, stamp+" "+line)
	if len(s.logBuf) > 200 {
		s.logBuf = s.logBuf[len(s.logBuf)-200:]
	}
}

// setFlash records an ephemeral toast.
func (s *state) setFlash(text, level string) {
	s.flash = flashState{Text: text, Level: level, CreatedAt: time.Now()}
}
