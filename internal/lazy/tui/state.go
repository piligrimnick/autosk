package tui

import (
	"context"
	"sync"
	"time"

	"autosk/internal/lazy/datasource"
	"autosk/internal/timeformat"
)

// sessionTranscriptCacheMax bounds state.sessionTranscript so a long lazy
// session that visits many sessions doesn't grow it unbounded. ~3MB worst
// case at 100KB/entry.
const sessionTranscriptCacheMax = 32 // renamed from jobTranscriptCacheMax

// sessionTranscriptTerminalTTL is the per-entry TTL for terminal sessions:
// after this window the next selection refetches the archive (so
// late-flushed events appear). Running sessions are kept fresh by SSE
// alone, no TTL refetch.
const sessionTranscriptTerminalTTL = 30 * time.Second // renamed from jobTranscriptTerminalTTL

// sessionLiveDebounce is the keystroke-debounce window before
// scheduleSessionLive actually opens an SSE subscription. j/k-spam across
// running sessions within this window collapses into one StreamSession call
// against the final-resting cursor row.
const sessionLiveDebounce = 2 * time.Second // renamed from jobLiveDebounce

// sessionLiveBufCap is the soft cap on the per-session live transcript event
// slice. Past this we drop the oldest 25% in one allocation and set
// te.truncated=true. ~2000 events is roughly an hour of pi traffic.
const sessionLiveBufCap = 2000 // renamed from jobLiveBufCap

// sessionTranscriptEntry is one entry in state.sessionTranscript: the
// archive + live event slice for a single session, plus per-event
// pre-rendered drawLabeledBox strings, plus the width they were
// rendered at (so a pane resize triggers a rebuild).
//
// touchedAt is the last time this entry was touched via
// ensureTranscriptEntryLocked (read or write). The LRU eviction
// scans the cache for the minimum touchedAt to pick a victim.
type sessionTranscriptEntry struct { // renamed from jobTranscriptEntry
	events        []datasource.LiveEvent // archive + live appends, oldest first (was MessageEvent)
	renderedBoxes []string               // pre-rendered drawLabeledBox per event
	// joinedBody is the per-frame body string renderSessionDetail
	// concatenates from renderedBoxes — pre-joined as
	//
	//	"\n" + boxes[0] + "\n" + "\n" + boxes[1] + "\n" + ...
	//
	// so the renderer can emit it with a single b.WriteString
	// instead of looping over the box slice on every frame. The
	// loop is O(N) in the number of events and on a long live
	// session (5000+ events) it was the dominant CPU cost of a
	// spinner-tick redraw — see ask-beab99 for the benchmark.
	// joinedDirty is set whenever the boxes slice mutates
	// (append, truncate, full rebuild); renderSessionDetail consults
	// it AND the renderedWidth / count mismatch as the rebuild
	// triggers.
	joinedBody    string
	joinedDirty   bool
	renderedWidth int       // contentW used when boxes were built; invalidates on resize
	loadedAt      time.Time // for TTL on terminal sessions
	touchedAt     time.Time // last access; drives LRU eviction
	truncated     bool      // hit live cap, dropped oldest 25%
	err           error     // last archive load error (renders as plashka)
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
	panelTasks    panelID = iota
	panelSessions         // renamed from panelJobs
	panelWorkflows
	panelAgents
	panelDetail       // the right detail pane (cursor land for j/k scroll)
	panelSessionInput // renamed from panelJobInput - synthetic: caret lives in winSessionInput
)

func (p panelID) String() string {
	switch p {
	case panelTasks:
		return "Tasks"
	case panelSessions:
		return "Sessions" // renamed from "Jobs"
	case panelWorkflows:
		return "Workflows"
	case panelAgents:
		return "Agents"
	case panelDetail:
		return "Detail"
	case panelSessionInput:
		return "SessionInput" // renamed from "JobInput"
	}
	return "?"
}

func (p panelID) window() string {
	switch p {
	case panelTasks:
		return winTasks
	case panelSessions:
		return winSessions // renamed from winJobs
	case panelWorkflows:
		return winWorkflows
	case panelAgents:
		return winAgents
	case panelDetail:
		return winDetail
	case panelSessionInput:
		return winSessionInput // renamed from winJobInput
	}
	return ""
}

// normalizeForDetail collapses panelSessionInput onto panelSessions for
// rendering / row-highlight purposes: the Detail pane above the
// input still shows Session Detail, and the Sessions panel's row
// highlight stays on while the operator is typing. Identity on
// every other value.
func (p panelID) normalizeForDetail() panelID {
	if p == panelSessionInput {
		return panelSessions
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
	case panelSessions:
		if sess, ok := s.selectedSession(); ok {
			return "session:" + sess.ID
		}
	case panelWorkflows:
		if w, ok := s.selectedWorkflow(); ok {
			return "workflow:" + w.Name
		}
	case panelAgents:
		if a, ok := s.selectedAgent(); ok {
			return "agent:" + a.Name
		}
	}
	return ""
}

// detailShowsSession reports whether the Detail pane is currently
// rendering a session (Session Detail). Used to gate (a) the
// winSessionInput overlay and (b) the writeViewSticky vs writeView
// branch in renderViews. Callers must already hold state.mu (R)Lock.
func detailShowsSession(s *state) bool {
	return detailDrivingPanel(s) == panelSessions
}

// NOTE: agentRel removed in v2 - no agent scoping

// scope describes the cross-link scope chips active on the dashboard.
type scope struct {
	TaskID       string // narrows Sessions
	WorkflowName string // narrows Tasks + Sessions (renamed from WorkflowID)
	// NOTE: Agent and AgentRel removed in v2 - no agent scoping
}

// IsEmpty reports whether no scope chip is active.
func (s scope) IsEmpty() bool {
	return s.TaskID == "" && s.WorkflowName == ""
}

// filterState holds the per-panel filter strings (`/`).
type filterState struct {
	Tasks     string
	Sessions  string // renamed from Jobs
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
	// popupSingleCompose is the one-pane multi-line editor used by
	// the comment and metadata flows: a single editable view with
	// SimpleEditor (Enter inserts "\n"), Ctrl+S submits, Esc
	// cancels. Reuses popupState.Input as the initial value and
	// OnAccept as the submit callback — the contract matches
	// popupPrompt, only the layout and the submit chord differ.
	popupSingleCompose
	// popupIsolation is the workflow-isolation flip popup chain. The
	// `i` key on the Workflows panel opens a two-option popupMenu;
	// confirming a non-current value chains into a popupConfirm whose
	// body enumerates the affected non-terminal tasks. A dedicated
	// popupKind value (rather than reusing popupMenu) makes the
	// keymap-pin test (popup_test.go) trivially scoped and keeps the
	// `i`-handler's accept callback isolated from any other future
	// menu-based flow.
	popupIsolation
	// popupEnroll is the two-pane workflow + step picker used by both
	// `e` (enroll) and `r` (resume) on the Tasks panel. Left pane
	// lists workflows, right pane mirrors the highlighted workflow's step list.
	// `r` reuses the same popup with WorkflowLocked=true so the
	// workflow pane only shows the task's current workflow as a
	// single, non-navigable row. See popupEnrollState for the
	// per-popup fields.
	popupEnroll
	// popupCheatsheet is the lazygit-style sectioned, filterable,
	// executable keybinding cheatsheet opened by `?`. It owns one
	// editable view (winPopupCheatsheet) on top of which printable
	// runes go through the view's Editor and accumulate into
	// state.popup.CheatsheetFilter; non-char keys (Enter / Esc /
	// Backspace / arrows / wheel) are routed via view-scoped
	// keybindings. See keys.go's bindingSpec doc for the metadata
	// model that feeds the cheatsheet items.
	popupCheatsheet
	// popupChangelog is the scrollable modal that fires on first
	// lazy start of a new release (auto-popup) or on `ctrl+w`
	// (manual re-opener). The body is operator-facing markdown
	// rendered through internal/lazy/markdown.Render. Dismissing
	// fires popupState.OnDismissChangelog (when non-nil) before
	// the popup is cleared so the auto-popup path can write
	// `last_seen_changelog` to ~/.autosk/state.json. The re-opener
	// path leaves OnDismissChangelog nil so dismissal doesn't
	// mutate state.json.
	popupChangelog
)

// pickerPane identifies which side of the enroll/resume two-pane
// picker currently owns input. j/k and Enter route differently per
// pane (workflow vs step).
type pickerPane int

const (
	pickerPaneWorkflow pickerPane = iota
	pickerPaneStep
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

	// Single-compose subtitle hint (popupSingleCompose). Short label
	// drawn on the top frame next to the always-on "<ctrl+s> submit
	// · <esc> cancel" string — e.g. "markdown ok" for the comment
	// popup or "JSON object" for the metadata popup. Empty when the
	// caller doesn't want a context label.
	Hint string

	// Enroll-picker fields (popupEnroll). Workflows lists the
	// pickable workflows (synthetic entries are pre-filtered by the
	// open helper). WorkflowCursor / StepCursor index into Workflows
	// and Workflows[WorkflowCursor].Steps respectively. ActivePane
	// picks which side currentView lands on each layout pass.
	// WorkflowLocked is set by openEnrollPicker(resume) so the
	// workflow pane never accepts cursor moves (only one row is
	// shown). OnPick fires from the Enter-on-step path with the
	// (workflow name, step name) pair the operator chose.
	Workflows      []datasource.Workflow
	WorkflowCursor int
	StepCursor     int
	ActivePane     pickerPane
	WorkflowLocked bool
	OnPick         func(wfName, stepName string) error

	// Cheatsheet fields (popupCheatsheet). CheatsheetItems is the
	// full unfiltered list of section markers + binding rows,
	// captured once at openCheatsheet time from the focused
	// panel's metadata view of bindingSpecs(). CheatsheetFilter is
	// the live case-insensitive substring filter the editor types
	// into. CheatsheetCursor is the index INTO THE FILTERED set of
	// non-header rows; the renderer translates it back to an
	// absolute index on every paint. CheatsheetFocused is the
	// panel that was focused at open time — used by the renderer
	// for the title-row hint.
	CheatsheetItems   []cheatsheetItem
	CheatsheetFilter  string
	CheatsheetCursor  int
	CheatsheetFocused panelID

	// Changelog-specific fields (popupChangelog).
	//
	// ChangelogBody is the rendered ANSI markdown blob (sized for
	// the popup's content width on the first layout pass).
	// ChangelogSource is the raw markdown body it was rendered
	// from; on resize we re-render against the new contentW from
	// the source instead of re-running glamour on a stale blob.
	// ChangelogWidth tracks the width the body was rendered at
	// so the layout pass can detect a resize and rebuild.
	// OnDismissChangelog fires once on Esc / Enter; the auto-popup
	// path uses it to stamp last_seen_changelog. nil for the
	// ctrl+w re-opener path.
	ChangelogBody      string
	ChangelogSource    string
	ChangelogWidth     int
	OnDismissChangelog func() error
}

// cheatsheetItem is one row of the cheatsheet body. A row is either
// a section header (IsHeader=true, Section set, Handler/Key/Desc
// empty) or a binding row (IsHeader=false, KeyLabel + Description
// set, Handler bound). The Section field on binding rows tracks
// which bucket the row belongs to so the renderer can re-group
// after filtering.
type cheatsheetItem struct {
	IsHeader    bool
	Section     string
	KeyLabel    string
	Description string
	Handler     func() error
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
	tasks    []datasource.Task
	sessions []datasource.Session // renamed from jobs
	// taskSessionIdx is the per-task session-presence index used by the
	// Tasks-panel marker column. Always computed from the
	// TaskID-UNFILTERED sessions read (workflow scope still applies)
	// so the ">" marker survives when scope.TaskID filters the
	// Sessions panel down to a single task — otherwise every other row
	// would lose its marker the moment Space was pressed.
	taskSessionIdx taskSessionIndex // renamed from taskJobIdx
	workflows      []datasource.Workflow
	agents         []datasource.Agent
	taskCursor     int
	sessionCursor  int // renamed from jobCursor
	workflowCursor int
	agentCursor    int

	scope   scope
	filter  filterState
	popup   popupState
	flash   flashState
	logBuf  []string
	logHide bool

	// sessionTranscript is a per-sessionID cache of the transcript shown in
	// the Detail pane. Bounded at sessionTranscriptCacheMax entries via
	// LRU eviction: every ensureTranscriptEntryLocked call stamps the
	// entry's touchedAt, and evictTranscriptIfNeeded picks the
	// minimum-stamp victim when the cap is hit.
	sessionTranscript map[string]*sessionTranscriptEntry // renamed from jobTranscript

	// sessionLive* hold the single active SSE subscription. Exactly one
	// session at a time may be streaming into the Detail pane;
	// switching selection to a different running session cancels the
	// current handle after the sessionLiveDebounce timer expires.
	sessionLiveSessionID string                 // renamed from jobLiveJobID
	sessionLiveHandle    *datasource.LiveHandle // renamed from jobLiveHandle
	sessionLiveCancel    context.CancelFunc     // renamed from jobLiveCancel
	sessionLiveTimer     *time.Timer            // renamed from jobLiveTimer

	// sessionInput is the cached contents of winSessionInput's textarea.
	// The view's Buffer() is authoritative once the view exists;
	// this is the model-side snapshot the renderer seeds the view
	// from on first creation and reads back on dispatch.
	//
	// sessionInputOwner is the sessionID whose draft is currently held in
	// sessionInput. Cursor moves between running sessions detect a mismatch
	// (via afterCursorMove / applyRefreshLocked) and clear both
	// fields so a draft typed for session-A doesn't leak into session-B's
	// textarea — nor get silently dispatched to the wrong session on
	// Ctrl-D.
	sessionInput      string // renamed from jobInput
	sessionInputOwner string // renamed from jobInputOwner

	health datasource.Health

	// fallbacksLast / fallbacksNow track the live datasource's
	// cumulative daemon-fallback counter so renderStatusBar can show
	// a 'flaky' chip when the counter advances since the last refresh
	// tick. Zero in pure offline mode (no Compose, no Live).
	fallbacksLast uint64
	fallbacksNow  uint64

	// comments is a per-task comment cache holding the full thread
	// per task (datasource.Comments is a pass-through over
	// Store.ListByTask), hydrated by refreshAll on cursor change
	// (RefreshHelper pattern). The rendered Tasks-detail pane reads
	// it; the comment count is authoritative on the Task struct.
	// Bounded at commentsCacheMax entries — one entry per task, NOT
	// per comment — and refresh.go evicts the LRU task on overflow.
	comments map[string][]datasource.Comment

	// NOTE: signals removed in v2
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
		detailFocus:       panelTasks,
		view:              StateDashboard,
		logBuf:            []string{"lazy started"},
		health:            datasource.Health{Daemon: "down"},
		comments:          map[string][]datasource.Comment{},
		sessionTranscript: map[string]*sessionTranscriptEntry{}, // renamed
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

func (s *state) selectedSession() (datasource.Session, bool) {
	if len(s.sessions) == 0 || s.sessionCursor < 0 || s.sessionCursor >= len(s.sessions) {
		return datasource.Session{}, false
	}
	return s.sessions[s.sessionCursor], true
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
func (s *state) selectedSessionLocked() (datasource.Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.selectedSession()
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
