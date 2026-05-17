package executor_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// ---- stubs ----------------------------------------------------------------

type stubRunner struct {
	pid             int
	state           pi.SessionInfo
	events          chan pi.Event
	turnEnds        chan struct{}
	exitCode        int
	promptCalls     atomic.Int32
	lastPrompt      atomic.Value // string
	terminateCalled atomic.Bool
	closed          atomic.Bool
}

func newStubRunner() *stubRunner {
	r := &stubRunner{
		pid:      4242,
		events:   make(chan pi.Event, 8),
		turnEnds: make(chan struct{}, 8),
	}
	r.state = pi.SessionInfo{SessionID: "sess-stub", SessionFile: "/tmp/stub.jsonl"}
	return r
}

func (r *stubRunner) PID() int                                            { return r.pid }
func (r *stubRunner) Events() <-chan pi.Event                             { return r.events }
func (r *stubRunner) GetState(ctx context.Context) (pi.SessionInfo, error) { return r.state, nil }
func (r *stubRunner) SendPrompt(ctx context.Context, m string) error {
	r.promptCalls.Add(1)
	r.lastPrompt.Store(m)
	// Schedule an agent_end shortly so the executor can advance.
	go func() {
		time.Sleep(5 * time.Millisecond)
		select {
		case r.turnEnds <- struct{}{}:
		default:
		}
	}()
	return nil
}
func (r *stubRunner) WaitForAgentEnd(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.turnEnds:
		return nil
	}
}
func (r *stubRunner) Abort(ctx context.Context) error    { return nil }
func (r *stubRunner) CloseStdin() error                  { r.closed.Store(true); return nil }
func (r *stubRunner) Terminate() error                   { r.terminateCalled.Store(true); return nil }
func (r *stubRunner) Kill() error                        { return nil }
func (r *stubRunner) Wait(ctx context.Context, _ time.Duration) (int, error) {
	return r.exitCode, nil
}

// stubFactory returns a fixed runner.
func stubFactory(r *stubRunner) executor.Factory {
	return func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		return r, nil
	}
}

// ---- helpers --------------------------------------------------------------

func newStores(t *testing.T) (*doltlite.Store, *runstore.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ts := doltlite.New()
	if err := ts.Open(ctx, dbPath); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	return ts, runstore.New(ts.DB()), func() { _ = ts.Close() }
}

func mkTask(t *testing.T, ts *doltlite.Store, title string) store.Task {
	t.Helper()
	tk, err := ts.CreateTask(context.Background(), store.Task{Title: title, Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// ---- closure verification -------------------------------------------------

func TestVerifyClosure_Done(t *testing.T) {
	ctx := context.Background()
	ts, _, cleanup := newStores(t)
	defer cleanup()
	tk := mkTask(t, ts, "x")
	_, _ = ts.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: ptrStatus(store.StatusDone)})
	got, err := executor.VerifyClosure(ctx, ts, tk.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != runstore.ClosureDone {
		t.Fatalf("got %q want done", got)
	}
}

func TestVerifyClosure_Cancelled(t *testing.T) {
	ctx := context.Background()
	ts, _, cleanup := newStores(t)
	defer cleanup()
	tk := mkTask(t, ts, "x")
	_, _ = ts.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: ptrStatus(store.StatusCancelled)})
	got, _ := executor.VerifyClosure(ctx, ts, tk.ID, nil)
	if got != runstore.ClosureCancelled {
		t.Fatalf("got %q want cancelled", got)
	}
}

func TestVerifyClosure_Decomposed(t *testing.T) {
	ctx := context.Background()
	ts, _, cleanup := newStores(t)
	defer cleanup()
	parent := mkTask(t, ts, "parent")
	blocker := mkTask(t, ts, "new blocker")
	if err := ts.Block(ctx, parent.ID, blocker.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := executor.VerifyClosure(ctx, ts, parent.ID, nil)
	if got != runstore.ClosureDecomposed {
		t.Fatalf("got %q want decomposed", got)
	}
}

func TestVerifyClosure_Invalid_NoChange(t *testing.T) {
	ctx := context.Background()
	ts, _, cleanup := newStores(t)
	defer cleanup()
	tk := mkTask(t, ts, "x")
	got, _ := executor.VerifyClosure(ctx, ts, tk.ID, nil)
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestVerifyClosure_ExistingBlocker_NotDecomposed(t *testing.T) {
	// If a blocker existed BEFORE the run started, it doesn't count as
	// "decomposed".
	ctx := context.Background()
	ts, _, cleanup := newStores(t)
	defer cleanup()
	parent := mkTask(t, ts, "parent")
	existing := mkTask(t, ts, "existing blocker")
	if err := ts.Block(ctx, parent.ID, existing.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := executor.VerifyClosure(ctx, ts, parent.ID, []string{existing.ID})
	if got != "" {
		t.Fatalf("got %q, want empty (existing blocker only)", got)
	}
}

// ---- end-to-end via stub runner ------------------------------------------

func TestExecutor_AdHocPromptOneTurnSucceeds(t *testing.T) {
	ctx := context.Background()
	_, runs, cleanup := newStores(t)
	defer cleanup()
	r, _ := runs.CreateRun(ctx, runstore.NewRun{Prompt: "say hi", Cwd: t.TempDir()})

	stub := newStubRunner()
	ex := executor.New(runs, nopTaskStore{}, stubFactory(stub), executor.Config{Grace: time.Second})

	if err := ex.Run(ctx, r.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := runs.GetRun(ctx, r.JobID)
	if got.Status != runstore.StatusDone {
		t.Fatalf("status: %q, want done\nrow: %+v", got.Status, got)
	}
	if got.PISessionID != "sess-stub" || got.SessionPath != "/tmp/stub.jsonl" {
		t.Errorf("session not recorded: %+v", got)
	}
	if stub.promptCalls.Load() != 1 {
		t.Errorf("prompt count: %d", stub.promptCalls.Load())
	}
}

func TestExecutor_TaskClosedAfterFirstTurn(t *testing.T) {
	ctx := context.Background()
	ts, runs, cleanup := newStores(t)
	defer cleanup()
	tk := mkTask(t, ts, "do thing")
	r, _ := runs.CreateRun(ctx, runstore.NewRun{
		TaskID:         tk.ID,
		Prompt:         "do it",
		Cwd:            t.TempDir(),
		AutoClaim:      true,
		MaxCorrections: 3,
	})

	// Close the task after one turn by having the stub's WaitForAgentEnd
	// race the closure: simpler — close the task BEFORE Run starts.
	// To exercise the "after-turn check" path, we close just before
	// SendPrompt returns. The stub signals an agent_end 5ms after SendPrompt.
	// We close the task synchronously here; first turn passes verification.
	_, _ = ts.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: ptrStatus(store.StatusDone)})

	stub := newStubRunner()
	ex := executor.New(runs, ts, stubFactory(stub), executor.Config{Grace: time.Second})
	if err := ex.Run(ctx, r.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := runs.GetRun(ctx, r.JobID)
	if got.Status != runstore.StatusDone {
		t.Fatalf("status: %q", got.Status)
	}
	if got.ClosureKind != runstore.ClosureDone {
		t.Fatalf("closure_kind: %q", got.ClosureKind)
	}
	if got.CorrectionsUsed != 0 {
		t.Errorf("corrections_used: %d", got.CorrectionsUsed)
	}
}

func TestExecutor_KickbackThenSuccess(t *testing.T) {
	ctx := context.Background()
	ts, runs, cleanup := newStores(t)
	defer cleanup()
	tk := mkTask(t, ts, "do thing")
	r, _ := runs.CreateRun(ctx, runstore.NewRun{
		TaskID:         tk.ID,
		Prompt:         "do it",
		Cwd:            t.TempDir(),
		AutoClaim:      true,
		MaxCorrections: 3,
	})

	stub := newStubRunner()
	// On the 2nd SendPrompt (the corrective one), simulate the agent
	// closing the task before the next end-of-turn arrives.
	origStub := stub
	stub2 := *origStub
	// We use a small inline factory to attach close-after-prompt behavior.
	factory := func(ctx context.Context, opts pi.Opts) (executor.PiRunner, error) {
		return &tweakedStub{stubRunner: origStub, onPromptN: func(n int32) {
			if n == 2 {
				_, _ = ts.UpdateTask(ctx, tk.ID, store.TaskPatch{Status: ptrStatus(store.StatusDone)})
			}
		}}, nil
	}
	ex := executor.New(runs, ts, factory, executor.Config{Grace: time.Second})
	if err := ex.Run(ctx, r.JobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := runs.GetRun(ctx, r.JobID)
	if got.Status != runstore.StatusDone {
		t.Fatalf("status: %q", got.Status)
	}
	if got.ClosureKind != runstore.ClosureDone {
		t.Fatalf("closure_kind: %q", got.ClosureKind)
	}
	if got.CorrectionsUsed != 1 {
		t.Errorf("corrections_used: %d, want 1", got.CorrectionsUsed)
	}
	_ = stub2
}

func TestExecutor_CorrectionsExhausted_Fails(t *testing.T) {
	ctx := context.Background()
	ts, runs, cleanup := newStores(t)
	defer cleanup()
	tk := mkTask(t, ts, "do thing")
	r, _ := runs.CreateRun(ctx, runstore.NewRun{
		TaskID:         tk.ID,
		Prompt:         "do it",
		Cwd:            t.TempDir(),
		AutoClaim:      true,
		MaxCorrections: 2,
	})
	stub := newStubRunner()
	ex := executor.New(runs, ts, stubFactory(stub), executor.Config{Grace: time.Second})
	if err := ex.Run(ctx, r.JobID); !errors.Is(err, executor.ErrAgentDidNotClose) {
		t.Fatalf("err: %v", err)
	}
	got, _ := runs.GetRun(ctx, r.JobID)
	if got.Status != runstore.StatusFailed {
		t.Fatalf("status: %q", got.Status)
	}
	if got.Error != executor.ErrAgentDidNotClose.Error() {
		t.Fatalf("error: %q", got.Error)
	}
	if got.CorrectionsUsed != 2 {
		t.Errorf("corrections_used: %d", got.CorrectionsUsed)
	}
}

func TestCorrectiveMessage_MentionsTaskAndProtocol(t *testing.T) {
	msg := executor.CorrectiveMessage("as-1234", 1, 3)
	for _, want := range []string{"as-1234", "autosk done as-1234", "autosk cancel as-1234", "--blocks as-1234"} {
		if !contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

// ---- helpers --------------------------------------------------------------

type nopTaskStore struct{}

func (nopTaskStore) GetTask(ctx context.Context, id string) (store.Task, error) {
	return store.Task{}, errors.New("nopTaskStore.GetTask called")
}
func (nopTaskStore) Claim(ctx context.Context, id string) (store.Task, error) {
	return store.Task{}, errors.New("nopTaskStore.Claim called")
}
func (nopTaskStore) Deps(ctx context.Context, id string) ([]string, []string, error) {
	return nil, nil, errors.New("nopTaskStore.Deps called")
}

func ptrStatus(s store.Status) *store.Status { return &s }

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// tweakedStub wraps stubRunner so we can run a hook on each SendPrompt.
type tweakedStub struct {
	*stubRunner
	onPromptN func(int32)
}

func (s *tweakedStub) SendPrompt(ctx context.Context, m string) error {
	n := s.stubRunner.promptCalls.Add(1)
	s.stubRunner.lastPrompt.Store(m)
	if s.onPromptN != nil {
		s.onPromptN(n)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		select {
		case s.stubRunner.turnEnds <- struct{}{}:
		default:
		}
	}()
	return nil
}
