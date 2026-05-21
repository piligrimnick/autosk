package datasource_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// TestDatasourceVerbsTimeout (risk #7 from the impl plan) wraps an
// arbitrary Datasource implementation behind a timing wrapper that
// fails the test if any read verb takes longer than 100ms. The
// real-world concern: a future Live impl that synchronously calls
// into network code without honouring the caller's ctx can hang the
// gocui main thread. This wrapper, when used in CI smoke tests,
// surfaces the regression at test time instead of in production.
//
// We exercise it against the Offline impl (deterministic timing).
// The test doubles as a usage example for future maintainers.
func TestDatasourceVerbsTimeout(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()

	w := &timeoutWrapper{ds: ds, limit: 100 * time.Millisecond, t: t}
	// Drive every read verb in a row; the wrapper fails the test on
	// any individual call that exceeds the limit.
	if _, err := w.Tasks(ctx, datasource.DefaultTaskFilter()); err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if _, err := w.Jobs(ctx, datasource.JobFilter{}); err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if _, err := w.Workflows(ctx, true); err != nil {
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
	ds    datasource.Datasource
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

func (w *timeoutWrapper) Tasks(ctx context.Context, f datasource.TaskFilter) ([]datasource.Task, error) {
	var out []datasource.Task
	err := w.check("Tasks", func() error {
		var e error
		out, e = w.ds.Tasks(ctx, f)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Jobs(ctx context.Context, f datasource.JobFilter) ([]datasource.Job, error) {
	var out []datasource.Job
	err := w.check("Jobs", func() error {
		var e error
		out, e = w.ds.Jobs(ctx, f)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Workflows(ctx context.Context, syn bool) ([]datasource.Workflow, error) {
	var out []datasource.Workflow
	err := w.check("Workflows", func() error {
		var e error
		out, e = w.ds.Workflows(ctx, syn)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Agents(ctx context.Context) ([]datasource.Agent, error) {
	var out []datasource.Agent
	err := w.check("Agents", func() error {
		var e error
		out, e = w.ds.Agents(ctx)
		return e
	})
	return out, err
}
func (w *timeoutWrapper) Healthz(ctx context.Context) (datasource.Health, error) {
	var out datasource.Health
	err := w.check("Healthz", func() error {
		var e error
		out, e = w.ds.Healthz(ctx)
		return e
	})
	return out, err
}
