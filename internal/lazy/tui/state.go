package tui

import (
	"context"
	"sync"
	"time"

	"autosk/internal/lazy/datasource"
	"autosk/internal/timeformat"
)

// jobTranscriptCacheMax bounds state.jobTranscript so a long lazy
// session that visits many jobs doesn't grow it unbounded. ~3MB worst
// case at 100KB/entry.
const jobTranscriptCacheMax = 32

// jobTranscriptTerminalTTL is the per-entry TTL for terminal jobs:
// after this window the next selection refetches the archive (so
// late-flushed events appear). Running jobs are kept fresh by SSE
// alone, no TTL refetch.
const jobTranscriptTerminalTTL = 30 * time.Second

// jobLiveDebounce is the keystroke-debounce window before
// scheduleJobLive actually opens an SSE subscription. j/k-spam across
// running jobs within this window collapses into one StreamLive call
// against the final-resting cursor row.
const jobLiveDebounce = 2 * time.Second

// jobLiveBufCap is the soft cap on the per-job live transcript event
// slice. Past this we drop the oldest 25% in one allocation and set
// te.truncated=true. ~2000 events is roughly an hour of pi traffic.
const jobLiveBufCap = 2000

// jobTranscriptEntry is one entry in state.jobTranscript: the
// archive + live event slice for a single job, plus per-event
// pre-rendered drawLabeledBox strings, plus the width they were
// rendered at (so a pane resize triggers a rebuild).
//
// touchedAt is the last time this entry was touched via
// ensureTranscriptEntryLocked (read or write). The LRU eviction
// scans the cache for the minimum touchedAt to pick a victim.
type jobTranscriptEntry struct {
	events        []datasource.MessageEvent // archive + live appends, oldest first
	renderedBoxes []string                  // pre-rendered drawLabeledBox per event
	renderedWidth int                       // contentW used when boxes were built; invalidates on resize
	loadedAt      time.Time                 // for TTL on terminal jobs
	touchedAt     time.Time                 // last access; drives LRU eviction
	truncated     bool                      // hit live cap, dropped oldest 25%
	err           error                     // last archive load error (renders as plashka)
}

// panelID is the identifier of one of the four dashboard list
// contexts. Used by the focus stack + the scope helper.
//
// panelJobInput is a synthetic focus identity that represents the
// job-input textarea (winJobInput). It is never a target of Tab
// cycling or the numeric panel keys (1..4, 0); the only path that
// lands on it is jobsEnter's running-job branch. We need it as a
// distinct focus value so layout's `g.SetCurrentView(focused.window())`
// keeps the current view pinned on winJobInput across redraws —
// without it, every layout pass would yank focus back to winJobs
// (the previous focused panel) within ~100ms.
type panelID int

const (
	panelTasks panelID = iota
	panelJobs
	panelWorkflows
	panelAgents
	panelDetail   // the right detail pane (cursor land for j/k scroll)
	panelJobInput // synthetic: caret lives in winJobInput
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
	case panelJobInput:
		return "JobInput"
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
	case panelJobInput:
		return winJobInput
	}
	return ""
}

// normalizeForDetail collapses panelJobInput onto panelJobs for
// rendering / row-highlight purposes: the Detail pane above the
// input still shows Job Detail, and the Jobs panel's row
// highlight stays on while the operator is typing. Identity on
// every other value.
func (p panelID) normalizeForDetail() panelID {
	if p == panelJobInput {
		return panelJobs
	}
	return p
}

// detailDrivingPanel returns the panelID whose entity is currently
// rendered into the Detail pane, mirroring renderDetail's focus /
// detailFocus switch (with the same normalizeForDetail collapses).
// Callers must already hold state.mu (R)Lock — the function reads
// state.focused / state.detailFocus directly.
func detailDrivingPanel(s *state) panelID {
	active := s.focused.normalizeForDetail()
	if active == panelDetail {
		active = s.detailFocus.normalizeForDetail()
	}
	return active
}

// detailEntityKey returns a stable "kind:id" identifier for the
// entity currently rendered into the Detail pane, or the empty
// string when there's nothing to show. renderViews uses it to
// detect when the Detail pane should reset its viewport origin
// (entity changed → start from the natural anchor) versus preserve
// it (same entity re-rendered → keep the operator's scroll
// position). Callers must already hold state.mu (R)Lock.
func detailEntityKey(s *state) string {
	switch detailDrivingPanel(s) {
	case panelTasks:
		if t, ok := s.selectedTask(); ok {
			return "task:" + t.ID
		}
	case panelJobs:
		if j, ok := s.selectedJob(); ok {
			return "job:" + j.JobID
		}
	case panelWorkflows:
		if w, ok := s.selectedWorkflow(); ok {
			return "workflow:" + w.ID
		}
	case panelAgents:
		if a, ok := s.selectedAgent(); ok {
			return "agent:" + a.Name
		}
	}
	return ""
}

// detailShowsJob reports whether the Detail pane is currently
// rendering a job (Job Detail). Used to gate (a) the
// winJobInput overlay and (b) the writeViewSticky vs writeView
// branch in renderViews. Callers must already hold state.mu (R)Lock.
func detailShowsJob(s *state) bool {
	return detailDrivingPanel(s) == panelJobs
}

// agentRel selects which agent relation an agent-scope chip refers
// to. Design plan §3.4 forces the Agents-panel Enter popup so the
// operator picks one explicitly (the relation is ambiguous —
// author_id and current_step.agent_id are different concepts).
type agentRel int

const (
	agentRelNone   agentRel = iota
	agentRelAuthor          // narrow on tasks.author_id
	agentRelStep            // narrow on current_step.agent_id
)

func (r agentRel) String() string {
	switch r {
	case agentRelAuthor:
		return "author"
	case agentRelStep:
		return "step"
	}
	return ""
}

// scope describes the cross-link scope chips active on the dashboard.
type scope struct {
	TaskID       string // narrows Jobs
	WorkflowID   string // narrows Tasks + Jobs
	WorkflowName string
	Agent        string   // narrows Tasks (opt-in via Enter)
	AgentRel     agentRel // which agent relation Agent refers to
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
	// popupTaskCompose is the lazygit-style two-pane editor used to
	// create a task: a single-line summary on top, a multi-line
	// description below. Mirrors lazygit's commit-message panel
	// (pkg/gui/controllers/helpers/confirmation_helper.go
	// ResizeCommitMessagePanels). Owns two views
	// (winTaskComposeSummary / winTaskComposeDescription); the
	// popupState's ComposeFocus picks which one currentView lands on.
	popupTaskCompose
)

// composePane identifies one of the two panes in the task-compose
// popup. The state machine flips between them on Tab.
type composePane int

const (
	composeSummary composePane = iota
	composeDescription
)

// popupState is the runtime state of the current popup.
type popupState struct {
	Kind     popupKind
	Title    string
	Lines    []string // for menu / help / search-results
	Cursor   int      // for menu
	Input    string   // for prompt / search
	OnAccept func(value string) error
	OnSelect func(index int) error
	OnCancel func() error

	// Compose-specific fields (popupTaskCompose).
	//
	// Summary / Description are the INITIAL values seeded into the
	// view's TextArea on first layout; once the views exist their
	// TextArea is the source of truth (just like the single-pane
	// prompt's Buffer()). ComposeFocus is the only field the toggle
	// handler mutates after open — layout reads it to pick which view
	// gets SetCurrentView each frame.
	Summary         string
	Description     string
	ComposeFocus    composePane
	OnComposeAccept func(summary, description string) error
}

// flashState is the ephemeral toast line. CreatedAt makes the layout
// loop drop the toast after a short timeout.
type flashState struct {
	Text      string
	Level     string // "info" | "warn" | "err"
	CreatedAt time.Time
}

// state is the entire mutable model of the TUI. Guarded by mu.
//
// Most writes are funnelled through g.Update closures (which run on
// the gocui main thread), but a handful of OnWorker-spawned
// goroutines also mutate state directly — see jobdetail.go's SSE
// pump, which writes into state.jobTranscript from a worker
// goroutine. The RWMutex is what makes that safe; do NOT drop it
// without untangling the worker writes first.
type state struct {
	mu sync.RWMutex

	// Top-level mode.
	view ViewState

	// Focus.
	focused      panelID
	focusedStack []panelID // for Esc-pop semantics (popups remember the side panel)

	// detailFocus is the side panel whose entity renderDetail should
	// render when focused == panelDetail. Without this, focusing the
	// Detail pane (via '0' or jobsEnter on a terminal job) would
	// land on the empty `panelDetail` switch arm in renderDetail and
	// wipe the pane to "(nothing selected)". Captured eagerly on
	// transitions INTO panelDetail; ignored otherwise.
	detailFocus panelID

	// Data caches. The View() is the source of truth for rendered
	// content — these are the most recent slice from the datasource so
	// the cursor positions are stable across re-renders.
	tasks []datasource.Task
	jobs  []datasource.Job
	// taskJobIdx is the per-task job-presence index used by the
	// Tasks-panel marker column. Always computed from the
	// TaskID-UNFILTERED jobs read (workflow scope still applies)
	// so the ">" marker survives when scope.TaskID filters the
	// Jobs panel down to a single task — otherwise every other row
	// would lose its marker the moment Space was pressed.
	taskJobIdx     taskJobIndex
	workflows      []datasource.Workflow
	agents         []datasource.Agent
	taskCursor     int
	jobCursor      int
	workflowCursor int
	agentCursor    int

	scope   scope
	filter  filterState
	popup   popupState
	flash   flashState
	logBuf  []string
	logHide bool

	// jobTranscript is a per-jobID cache of the transcript shown in
	// the Detail pane. Bounded at jobTranscriptCacheMax entries via
	// LRU eviction: every ensureTranscriptEntryLocked call stamps the
	// entry's touchedAt, and evictTranscriptIfNeeded picks the
	// minimum-stamp victim when the cap is hit.
	jobTranscript map[string]*jobTranscriptEntry

	// jobLive* hold the single active SSE subscription. Exactly one
	// job at a time may be streaming into the Detail pane;
	// switching selection to a different running job cancels the
	// current handle after the jobLiveDebounce timer expires.
	jobLiveJobID  string
	jobLiveHandle *datasource.LiveHandle
	jobLiveCancel context.CancelFunc
	jobLiveTimer  *time.Timer // debounce timer; reset on every selection change

	// jobInput is the cached contents of winJobInput's textarea.
	// The view's Buffer() is authoritative once the view exists;
	// this is the model-side snapshot the renderer seeds the view
	// from on first creation and reads back on dispatch.
	//
	// jobInputOwner is the jobID whose draft is currently held in
	// jobInput. Cursor moves between running jobs detect a mismatch
	// (via afterCursorMove / applyRefreshLocked) and clear both
	// fields so a draft typed for job-A doesn't leak into job-B's
	// textarea — nor get silently dispatched to the wrong job on
	// Ctrl-D.
	jobInput      string
	jobInputOwner string

	health datasource.Health

	// fallbacksLast / fallbacksNow track the live datasource's
	// cumulative daemon-fallback counter so renderStatusBar can show
	// a 'flaky' chip when the counter advances since the last refresh
	// tick. Zero in pure offline mode (no Compose, no Live).
	fallbacksLast uint64
	fallbacksNow  uint64

	// comments is a per-task last-N comment cache, hydrated by
	// refreshAll on cursor change (RefreshHelper pattern). The
	// rendered Tasks-detail pane reads it; the comment count is
	// authoritative on the Task struct. Bounded at commentsCacheMax
	// entries (refresh.go evicts on overflow).
	comments map[string][]datasource.Comment

	// signals is a per-task signals cache: "tail of last open
	// kickback chain" per design plan §4 for the Tasks-detail widget.
	// Hydrated by refreshAll on cursor change. Same bounding rule as
	// the comments cache.
	signals map[string][]datasource.Signal
}

// commentsCacheMax bounds state.comments so a long lazy session
// that visits many tasks doesn't grow it unbounded.
const commentsCacheMax = 64

// newState seeds an empty model with sensible defaults.
func newState() *state {
	return &state{
		focused: panelTasks,
		// detailFocus mirrors focused initially — if the user presses
		// '0' before navigating anywhere, renderDetail still has a
		// sensible side panel to consult.
		detailFocus:   panelTasks,
		view:          StateDashboard,
		logBuf:        []string{"lazy started"},
		health:        datasource.Health{Daemon: "down"},
		comments:      map[string][]datasource.Comment{},
		signals:       map[string][]datasource.Signal{},
		jobTranscript: map[string]*jobTranscriptEntry{},
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

// ---- locked accessor variants -------------------------------------------
//
// These wrap the bare selected* calls under the model's RLock; they
// are the safe entry points for handlers that don't already hold the
// lock. The bare selected* methods exist for callers that already
// hold the lock (e.g. inside a withRLock closure).

func (s *state) selectedTaskLocked() (datasource.Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.selectedTask()
}
func (s *state) selectedJobLocked() (datasource.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.selectedJob()
}
func (s *state) selectedWorkflowLocked() (datasource.Workflow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.selectedWorkflow()
}
func (s *state) selectedAgentLocked() (datasource.Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.selectedAgent()
}

// appendLog adds a one-line entry to the command log. The stamp is
// HH:MM:SS in the operator's local timezone — see internal/timeformat.
func (s *state) appendLog(line string) {
	stamp := timeformat.FormatTime(time.Now())
	s.logBuf = append(s.logBuf, stamp+" "+line)
	if len(s.logBuf) > 200 {
		s.logBuf = s.logBuf[len(s.logBuf)-200:]
	}
}

// setFlash records an ephemeral toast.
func (s *state) setFlash(text, level string) {
	s.flash = flashState{Text: text, Level: level, CreatedAt: time.Now()}
}
