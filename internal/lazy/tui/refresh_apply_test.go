package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// fakeDS is a minimal Datasource stub used to drive refreshAll's
// fetch/apply path without spinning up a real DB. Only the verbs
// fetchRefresh calls need real implementations; the rest return
// ErrDaemonRequired or zero values.
type refreshFakeDS struct {
	tasks        []datasource.Task
	tasksErr     error
	jobs         []datasource.Job
	jobsErr      error
	workflows    []datasource.Workflow
	workflowsErr error
	agents       []datasource.Agent
	agentsErr    error
	health       datasource.Health
	comments     []datasource.Comment
	commentsErr  error
	signals      []datasource.Signal
	signalsErr   error
}

func (f *refreshFakeDS) Tasks(_ context.Context, _ datasource.TaskFilter) ([]datasource.Task, error) {
	return f.tasks, f.tasksErr
}
func (f *refreshFakeDS) GetTask(_ context.Context, _ string) (datasource.Task, error) {
	return datasource.Task{}, nil
}
func (f *refreshFakeDS) Jobs(_ context.Context, _ datasource.JobFilter) ([]datasource.Job, error) {
	return f.jobs, f.jobsErr
}
func (f *refreshFakeDS) GetJob(_ context.Context, _ string) (datasource.Job, error) {
	return datasource.Job{}, nil
}
func (f *refreshFakeDS) Workflows(_ context.Context, _ bool) ([]datasource.Workflow, error) {
	return f.workflows, f.workflowsErr
}
func (f *refreshFakeDS) Agents(_ context.Context) ([]datasource.Agent, error) {
	return f.agents, f.agentsErr
}
func (f *refreshFakeDS) Comments(_ context.Context, _ string) ([]datasource.Comment, error) {
	return f.comments, f.commentsErr
}
func (f *refreshFakeDS) Signals(_ context.Context, _ string) ([]datasource.Signal, error) {
	return nil, nil
}
func (f *refreshFakeDS) SignalsForTask(_ context.Context, _ string) ([]datasource.Signal, error) {
	return f.signals, f.signalsErr
}
func (f *refreshFakeDS) Messages(_ context.Context, _ string, _ bool, _ int) ([]datasource.MessageEvent, error) {
	return nil, nil
}
func (f *refreshFakeDS) Healthz(_ context.Context) (datasource.Health, error) {
	return f.health, nil
}
func (f *refreshFakeDS) Reconnect(_ context.Context) error { return nil }
func (f *refreshFakeDS) CreateTask(_ context.Context, _, _ string, _ int) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdateStatus(_ context.Context, _ string, _ store.Status) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdatePriority(_ context.Context, _ string, _ int) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UpdateTitleDescription(_ context.Context, _, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) Enroll(_ context.Context, _, _ string) error {
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
func (f *refreshFakeDS) SetMetadata(_ context.Context, _ string, _ map[string]any) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) CreateWorkflow(_ context.Context, _ string) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) DeleteWorkflow(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) InstallAgent(_ context.Context, _, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) UninstallAgent(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) CancelJob(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) SendInput(_ context.Context, _, _, _ string) (string, error) {
	return "", datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) AbortJob(_ context.Context, _ string) error {
	return datasource.ErrDaemonRequired
}
func (f *refreshFakeDS) StreamLive(_ context.Context, _ string) (*datasource.LiveHandle, error) {
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

// TestRefreshApply_SignalsErrorPreservesCache mirrors the comments
// test for the signals cache.
func TestRefreshApply_SignalsErrorPreservesCache(t *testing.T) {
	gu := &Gui{st: newState()}
	gu.st.tasks = []datasource.Task{{ID: "ask-bbbbbb", Title: "y"}}
	gu.st.taskCursor = 0
	gu.st.focused = panelTasks
	gu.st.signals["ask-bbbbbb"] = []datasource.Signal{{StepName: "stale"}}

	fakeErr := &refreshFakeDS{
		tasks:      []datasource.Task{{ID: "ask-bbbbbb", Title: "y"}},
		signalsErr: errors.New("boom"),
	}
	gu.ds = fakeErr
	r := gu.fetchRefresh(context.Background())
	if !r.hasSignalsErr {
		t.Fatalf("expected hasSignalsErr")
	}
	gu.applyRefreshLocked(r)
	if got := gu.st.signals["ask-bbbbbb"]; len(got) != 1 || got[0].StepName != "stale" {
		t.Fatalf("err path overwrote signals cache: %+v", got)
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
