package datasource

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDatasourceVerbsTimeout (risk #7 from the impl plan) wraps an
// arbitrary Datasource implementation behind a timing wrapper that
// fails the test if any read verb takes longer than 100ms. The
// real-world concern: a future Live impl that synchronously calls
// into network code without honouring the caller's ctx can hang the
// gocui main thread. This wrapper, when used in CI smoke tests,
// surfaces the regression at test time instead of in production.
//
// We exercise it against the single RPC datasource impl backed by the
// in-process fakeDaemon (deterministic timing). The test doubles as a usage
// example for future maintainers.
func TestDatasourceVerbsTimeout(t *testing.T) {
	ctx := context.Background()
	ds := fakeDaemon(t, map[string]any{
		"meta.healthz": map[string]any{"ok": true, "workers": 0, "queued": 0, "running": 0,
			"projects": []any{}},
	})

	w := &timeoutWrapper{ds: ds, limit: 100 * time.Millisecond, t: t}
	// Drive every read verb in a row; the wrapper fails the test on
	// any individual call that exceeds the limit.
	if _, err := w.Tasks(ctx, DefaultTaskFilter()); err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if _, err := w.Sessions(ctx, ""); err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if _, err := w.Workflows(ctx); err != nil {
		t.Fatalf("Workflows: %v", err)
	}
	if _, err := w.Agents(ctx); err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if _, err := w.Healthz(ctx); err != nil {
		t.Fatalf("Healthz: %v", err)
	}
}

// timeoutWrapper is a Datasource adapter that fails the enclosing
// test if any read verb takes longer than `limit`. Use it in tests
// that want to assert UI-thread responsiveness invariants. (Not
// exposed in the production type to keep production code clean.)
type timeoutWrapper struct {
	ds    Datasource
	limit time.Duration
	t     *testing.T
}

func (w *timeoutWrapper) check(name string, fn func() error) error {
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if d := time.Since(start); d > w.limit {
			w.t.Errorf("verb %s took %s > %s; ui-thread responsiveness contract broken", name, d, w.limit)
		}
		return err
	case <-time.After(w.limit * 2):
		w.t.Fatalf("verb %s blocked beyond 2x limit (%s); hang likely", name, w.limit)
		return errors.New("timed out")
	}
}

func (w *timeoutWrapper) Tasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	var out []Task
	err := w.check("Tasks", func() error {
		var e error
		out, e = w.ds.Tasks(ctx, f)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Sessions(ctx context.Context, taskID string) ([]Session, error) {
	var out []Session
	err := w.check("Sessions", func() error {
		var e error
		out, e = w.ds.Sessions(ctx, taskID)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Workflows(ctx context.Context) ([]Workflow, error) {
	var out []Workflow
	err := w.check("Workflows", func() error {
		var e error
		out, e = w.ds.Workflows(ctx)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Agents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	err := w.check("Agents", func() error {
		var e error
		out, e = w.ds.Agents(ctx)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Healthz(ctx context.Context) (Health, error) {
	var out Health
	err := w.check("Healthz", func() error {
		var e error
		out, e = w.ds.Healthz(ctx)
		return e
	})
	return out, err
}

// Stub implementations for remaining v2 interface methods
func (w *timeoutWrapper) GetTask(ctx context.Context, id string) (Task, error) {
	return w.ds.GetTask(ctx, id)
}
func (w *timeoutWrapper) GetSession(ctx context.Context, id string) (Session, error) {
	return w.ds.GetSession(ctx, id)
}
func (w *timeoutWrapper) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	return w.ds.Comments(ctx, taskID)
}
func (w *timeoutWrapper) CreateTask(ctx context.Context, title, description string) (string, error) {
	return w.ds.CreateTask(ctx, title, description)
}
func (w *timeoutWrapper) TaskDone(ctx context.Context, id string) error {
	return w.ds.TaskDone(ctx, id)
}
func (w *timeoutWrapper) TaskCancel(ctx context.Context, id string) error {
	return w.ds.TaskCancel(ctx, id)
}
func (w *timeoutWrapper) TaskReopen(ctx context.Context, id string) error {
	return w.ds.TaskReopen(ctx, id)
}
func (w *timeoutWrapper) UpdateTask(ctx context.Context, id string, title, description *string) error {
	return w.ds.UpdateTask(ctx, id, title, description)
}
func (w *timeoutWrapper) EnrollWorkflow(ctx context.Context, id, workflow string) error {
	return w.ds.EnrollWorkflow(ctx, id, workflow)
}
func (w *timeoutWrapper) Resume(ctx context.Context, id, toStep string) error {
	return w.ds.Resume(ctx, id, toStep)
}
func (w *timeoutWrapper) Block(ctx context.Context, id, blocker string) error {
	return w.ds.Block(ctx, id, blocker)
}
func (w *timeoutWrapper) Unblock(ctx context.Context, id, blocker string) error {
	return w.ds.Unblock(ctx, id, blocker)
}
func (w *timeoutWrapper) AddComment(ctx context.Context, taskID, text string) error {
	return w.ds.AddComment(ctx, taskID, text)
}
func (w *timeoutWrapper) AbortSession(ctx context.Context, id string) error {
	return w.ds.AbortSession(ctx, id)
}
func (w *timeoutWrapper) SessionInput(ctx context.Context, id, message, kind string) error {
	return w.ds.SessionInput(ctx, id, message, kind)
}
func (w *timeoutWrapper) Reconnect(ctx context.Context) error {
	return w.ds.Reconnect(ctx)
}
func (w *timeoutWrapper) SessionTranscript(ctx context.Context, sessionID string) ([]LiveEvent, error) {
	return w.ds.SessionTranscript(ctx, sessionID)
}
func (w *timeoutWrapper) StreamSession(ctx context.Context, sessionID string) (*LiveHandle, error) {
	return w.ds.StreamSession(ctx, sessionID)
}
