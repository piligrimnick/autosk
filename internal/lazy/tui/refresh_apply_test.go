package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// refreshFakeDS is a minimal Datasource stub used to drive refreshAll's
// fetch/apply path without spinning up a real DB. Only the verbs
// fetchRefresh calls need real implementations; the rest return
// ErrDaemonRequired or zero values.
type refreshFakeDS struct {
	tasks        []datasource.Task
	tasksErr     error
	sessions     []datasource.Session
	sessionsErr  error
	workflows    []datasource.Workflow
	workflowsErr error
	agents       []datasource.Agent
	agentsErr    error
	health       datasource.Health
	comments     []datasource.Comment
	commentsErr  error
}

func (f *refreshFakeDS) Tasks(_ context.Context, _ datasource.TaskFilter) ([]datasource.Task, error) {
	return f.tasks, f.tasksErr
}
func (f *refreshFakeDS) GetTask(_ context.Context, _ string) (datasource.Task, error) {
	return datasource.Task{}, nil
}
func (f *refreshFakeDS) Sessions(_ context.Context, _ string) ([]datasource.Session, error) {
	return f.sessions, f.sessionsErr
}
func (f *refreshFakeDS) GetSession(_ context.Context, _ string) (datasource.Session, error) {
	return datasource.Session{}, nil
}
func (f *refreshFakeDS) Workflows(_ context.Context) ([]datasource.Workflow, error) {
	return f.workflows, f.workflowsErr
}
func (f *refreshFakeDS) Agents(_ context.Context) ([]datasource.Agent, error) {
	return f.agents, f.agentsErr
}
func (f *refreshFakeDS) Comments(_ context.Context, _ string) ([]datasource.Comment, error) {
	return f.comments, f.commentsErr
}
func (f *refreshFakeDS) Healthz(_ context.Context) (datasource.Health, error) {
	return f.health, nil
}
func (f *refreshFakeDS) Reconnect(_ context.Context) error { return nil }
func (f *refreshFakeDS) CreateTask(_ context.Context, _, _ string) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) TaskDone(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) TaskCancel(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) TaskReopen(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdateTask(_ context.Context, _ string, _, _ *string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) EnrollWorkflow(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) EnrollAgent(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Resume(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Block(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Unblock(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) AddComment(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) AbortSession(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) SessionInput(_ context.Context, _, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) SessionTranscript(_ context.Context, _ string) ([]datasource.LiveEvent, error) {
	return nil, datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) StreamSession(_ context.Context, _ string) (*datasource.LiveHandle, error) {
	return nil, datasource.ErrDaemonRequired
}

// TestRefreshApply_CommentsErrorPreservesCache pins the regression
// the previous review flagged: when ds.Comments fails for the
// currently-selected task, refreshAll must NOT overwrite the existing
// cache entry — stale-but-shown is better than empty-and-flickering.
//
// We drive fetchRefresh + applyRefreshLocked directly (no gocui
// needed) and exercise both branches: success populates the cache,
// error preserves the prior cache entry.
func TestRefreshApply_CommentsErrorPreservesCache(t *testing.T) {
	gu := &Gui{st: newState()}
	// Seed the task list so selectedTask() returns something.
	gu.st.tasks = []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks
	// Pre-seed the cache with a "stale" value.
	gu.st.comments["ask-aaaaaa"] = []datasource.Comment{{Text: "stale-cached"}}

	// First refresh: success path replaces the stale value.
	fakeOK := &refreshFakeDS{
		tasks:    []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}},
		comments: []datasource.Comment{{Text: "fresh"}},
	}
	gu.ds = fakeOK
	r := gu.fetchRefresh(context.Background())
	if r.hasCommentsErr {
		t.Fatalf("ok path produced hasCommentsErr")
	}
	gu.applyRefreshLocked(r)
	if got := gu.st.comments["ask-aaaaaa"]; len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("ok path didn't update cache: %+v", got)
	}

	// Second refresh: Comments() errors. The cache MUST stay at "fresh"
	// (not get cleared, not be overwritten with nil).
	wantErr := errors.New("db lock")
	fakeErr := &refreshFakeDS{
		tasks:       []datasource.Task{{ID: "ask-aaaaaa", Title: "x"}},
		commentsErr: wantErr,
	}
	gu.ds = fakeErr

	// Also pin dlog firing on error.
	var dlogBuf strings.Builder
	SetDebugLogger(func(format string, args ...any) {
		dlogBuf.WriteString(format)
	})
	defer SetDebugLogger(nil)

	r2 := gu.fetchRefresh(context.Background())
	if !r2.hasCommentsErr {
		t.Fatalf("err path didn't set hasCommentsErr (err=%v)", wantErr)
	}
	if r2.selectedComments != nil {
		t.Fatalf("err path leaked stale comments into result: %+v", r2.selectedComments)
	}
	gu.applyRefreshLocked(r2)
	if got := gu.st.comments["ask-aaaaaa"]; len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("err path overwrote cache: %+v (want %q)", got, "fresh")
	}
	if !strings.Contains(dlogBuf.String(), "ds.Comments") {
		t.Fatalf("dlog did not fire on comments error; buf=%q", dlogBuf.String())
	}
}

// TestRefreshApply_FallbacksCounterAdvances pins the fallback chip
// machinery: a Datasource that implements Fallbacks() must drive the
// state.fallbacksNow / fallbacksLast pair so the status bar can
// detect delta > 0 between ticks.
func TestRefreshApply_FallbacksCounterAdvances(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &fallbackFakeDS{
		refreshFakeDS: refreshFakeDS{},
		count:         5,
	}
	r := gu.fetchRefresh(context.Background())
	if r.fallbacksNow != 5 {
		t.Fatalf("fallbacksNow=%d want 5", r.fallbacksNow)
	}
	gu.applyRefreshLocked(r)
	if gu.st.fallbacksNow != 5 {
		t.Fatalf("applied fallbacksNow=%d want 5", gu.st.fallbacksNow)
	}
	// Second tick at 8 → delta = 3.
	gu.ds.(*fallbackFakeDS).count = 8
	r2 := gu.fetchRefresh(context.Background())
	gu.applyRefreshLocked(r2)
	if delta := gu.st.fallbacksNow - gu.st.fallbacksLast; delta != 3 {
		t.Fatalf("delta=%d want 3 (now=%d last=%d)", delta, gu.st.fallbacksNow, gu.st.fallbacksLast)
	}
}

// fallbackFakeDS wraps refreshFakeDS with a Fallbacks() method so
// fetchRefresh's interface assertion picks it up.
type fallbackFakeDS struct {
	refreshFakeDS
	count uint64
}

func (f *fallbackFakeDS) Fallbacks() uint64 { return f.count }

// TestRefreshApply_RunningToTerminalInvalidatesArchive pins the
// hand-wired transition path in applyRefreshLocked: when the
// currently-selected session's status flips from running to terminal
// between refresh snapshots, the existing transcript cache entry
// must have its loadedAt zeroed so the next selection-driven
// hydration (or the queued OnWorker dispatch) re-fetches the
// archive.
func TestRefreshApply_RunningToTerminalInvalidatesArchive(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}
	const sessionID = "session-trans-XYZ"

	// Seed: sessions slice has one running session, cursor on it, cache
	// entry exists with a non-zero loadedAt.
	gu.st.withLock(func() {
		gu.st.sessions = []datasource.Session{{
			ID: sessionID, Status: "running",
		}}
		gu.st.sessionCursor = 0
		gu.st.focused = panelSessions
		te := gu.ensureTranscriptEntryLocked(sessionID)
		te.loadedAt = time.Now().Add(-time.Second) // already loaded
	})

	// Drive applyRefreshLocked with a refreshResult that flips the
	// status to terminal.
	r := refreshResult{
		sessions: []datasource.Session{{
			ID: sessionID, Status: "done",
		}},
		taskSessionIdx: taskSessionIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var loadedAt time.Time
	gu.st.withRLock(func() {
		te := gu.st.sessionTranscript[sessionID]
		if te != nil {
			loadedAt = te.loadedAt
		}
	})
	if !loadedAt.IsZero() {
		t.Errorf("running→terminal transition did not zero loadedAt; got %v", loadedAt)
	}
	// sessionArchiveStale must report the entry as stale now.
	if !gu.sessionArchiveStale(sessionID, false) {
		t.Errorf("entry should report stale after loadedAt zeroing")
	}
}

// TestRefreshApply_RunningToTerminal_RevertsFocusFromSessionInput
// pins the second-half of the running→terminal transition: if the
// operator was typing in winSessionInput when the session finished
// (state.focused == panelSessionInput), the layout pass that follows
// will delete the input view because showSessionInput now reads false.
// applyRefreshLocked must therefore also revert focused to
// panelSessions so SetCurrentView lands the caret on a view that
// still exists, instead of leaving it in phantom-focus on a
// deleted view.
func TestRefreshApply_RunningToTerminal_RevertsFocusFromSessionInput(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}
	const sessionID = "session-typing"

	gu.st.withLock(func() {
		gu.st.sessions = []datasource.Session{{
			ID: sessionID, Status: "running",
		}}
		gu.st.sessionCursor = 0
		gu.st.focused = panelSessionInput // operator is typing
	})

	r := refreshResult{
		sessions: []datasource.Session{{
			ID: sessionID, Status: "done",
		}},
		taskSessionIdx: taskSessionIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelSessions {
		t.Errorf("focused after running→terminal = %v, want panelSessions (input view goes away; revert)", focused)
	}
}

// TestRefreshApply_TasksCursorByID pins ID-based cursor restore for
// the Tasks panel: a re-sort or re-filter that changes positional
// indices must keep the selection anchored to the same task.
func TestRefreshApply_TasksCursorByID(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{
			{ID: "ask-aaaaaa", Title: "first", UpdatedAt: time.Now().Add(-time.Hour)},
			{ID: "ask-bbbbbb", Title: "second", UpdatedAt: time.Now()},
		}
		gu.st.taskCursor = 0
	})

	// Refresh re-orders: ask-bbbbbb (newer) lands at index 0.
	r := refreshResult{
		tasks: []datasource.Task{
			{ID: "ask-bbbbbb", Title: "second", UpdatedAt: time.Now()},
			{ID: "ask-aaaaaa", Title: "first", UpdatedAt: time.Now().Add(-time.Hour)},
		},
	}
	gu.applyRefreshLocked(r)

	var cursor int
	var id string
	gu.st.withRLock(func() {
		cursor = gu.st.taskCursor
		if t, ok := gu.st.selectedTask(); ok {
			id = t.ID
		}
	})
	if id != "ask-aaaaaa" {
		t.Errorf("selected task after reshuffle = %q, want ask-aaaaaa", id)
	}
	if cursor != 1 {
		t.Errorf("taskCursor after reshuffle = %d, want 1", cursor)
	}
}

// TestRefreshApply_AgentsCursorByKey mirrors the tasks test for
// the Agents panel (keyed by Name).
func TestRefreshApply_AgentsCursorByKey(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.ds = &refreshFakeDS{}

	gu.st.withLock(func() {
		gu.st.agents = []datasource.Agent{
			{Name: "alice"},
			{Name: "bob"},
		}
		gu.st.agentCursor = 0
	})

	// Refresh re-orders: bob lands at index 0.
	r := refreshResult{
		agents: []datasource.Agent{
			{Name: "bob"},
			{Name: "alice"},
		},
	}
	gu.applyRefreshLocked(r)

	var cursor int
	var name string
	gu.st.withRLock(func() {
		cursor = gu.st.agentCursor
		if a, ok := gu.st.selectedAgent(); ok {
			name = a.Name
		}
	})
	if name != "alice" {
		t.Errorf("selected agent after reshuffle = %q, want alice", name)
	}
	if cursor != 1 {
		t.Errorf("agentCursor after reshuffle = %d, want 1", cursor)
	}
}
