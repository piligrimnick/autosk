package executor_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/runstore"
)

// TestRun_SkipsCorrectionsWhileAttached: when a client is attached the
// executor must not consume any of the kickback budget on a missed
// step_signals. The turn loop just iterates back to WaitForAgentEnd.
//
// We poke the stub's turn-end channel directly from the test so the
// executor sees three "agent_end without signal" events back-to-back.
// While attached, every one of those must be a no-op (no corrective
// prompt, no CorrectionsUsed bump). After detach + signal emit + one
// more poke, the run completes successfully.
func TestRun_SkipsCorrectionsWhileAttached(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "Attached", "dev")

	att := pirunners.NewAttachments()
	rel := att.Acquire(jobID)
	fx.deps.Attachments = att

	// Use a noop stub that does NOT auto-schedule a turn_end on
	// SendPrompt — the test drives turn-ends explicitly.
	stub := newStub()
	stub.onPrompt = nil

	// Drive three attached no-signal turn boundaries + one final
	// turn where we detach and emit the signal so the run completes.
	driver := make(chan struct{})
	go func() {
		<-driver // wait for the executor to have sent the initial prompt
		// 3 attached no-signal turns
		for i := 0; i < 3; i++ {
			time.Sleep(30 * time.Millisecond)
			stub.turnEnds <- struct{}{}
		}
		// Now detach + emit the signal + final turn_end so the loop exits.
		time.Sleep(30 * time.Millisecond)
		rel()
		if _, err := fx.deps.Signals.Emit(ctx, taskID, "review"); err != nil {
			t.Errorf("Emit: %v", err)
		}
		stub.turnEnds <- struct{}{}
	}()

	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			close(driver) // initial prompt landed; let the driver run
		}
	}

	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusDone {
		t.Fatalf("run.Status=%s want done", run.Status)
	}
	if run.CorrectionsUsed != 0 {
		t.Fatalf("CorrectionsUsed=%d want 0 (kickbacks should be skipped while attached)", run.CorrectionsUsed)
	}
	// Only the initial prompt should have been sent (no kickbacks while attached).
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.prompts) != 1 {
		t.Fatalf("prompts=%d (%v); want exactly 1 (initial), no kickbacks while attached",
			len(stub.prompts), stub.prompts)
	}
}

// TestRun_AttachedDisablesIdleTimeout: an attached run must keep
// waiting in WaitForAgentEnd well past the configured idle timeout.
// After (idle*3) we detach, emit the signal, and let the run finish.
func TestRun_AttachedDisablesIdleTimeout(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	fx.cfg.IdleTimeout = 50 * time.Millisecond
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "SlowAttached", "dev")

	att := pirunners.NewAttachments()
	rel := att.Acquire(jobID)
	fx.deps.Attachments = att

	stub := newStub()
	stub.onPrompt = nil

	driver := make(chan struct{})
	go func() {
		<-driver
		// Sleep well past the idle timeout to prove it's been disarmed.
		time.Sleep(3 * fx.cfg.IdleTimeout)
		rel()
		if _, err := fx.deps.Signals.Emit(ctx, taskID, "review"); err != nil {
			t.Errorf("Emit: %v", err)
		}
		stub.turnEnds <- struct{}{}
	}()
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			close(driver)
		}
	}

	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v (idle timeout should have been suspended)", err)
	}
	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusDone {
		t.Fatalf("run.Status=%s want done", run.Status)
	}
}

// TestRun_KickbackResumesAfterDetach: a baseline check that the
// correction loop is only paused, not disabled. After one attached
// no-signal turn (which must NOT consume a correction) and detach,
// the driver schedules MaxCorrections+1=3 further no-signal turns so
// the loop exhausts the budget and terminates with
// ErrAgentDidNotEmit + Status=failed + CorrectionsUsed==2.
//
// Synchronisation contract (avoid timing-fragility):
//
//   - The driver waits for CorrectionsUsed to advance after each
//     post-detach kickback (which is what the executor writes back
//     via Runs.IncCorrections) instead of guessing a sleep. The
//     attached-then-detach handoff still uses a small sleep to give
//     stubRunner's auto-scheduled 5ms turn_end goroutine a chance to
//     drain into the buffered channel before we release the attach
//     counter; that sleep is bounded only by SchedulerSlack and is
//     not part of the assertion.
func TestRun_KickbackResumesAfterDetach(t *testing.T) {
	const SchedulerSlack = 100 * time.Millisecond
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "DetachThenKickback", "dev")
	_ = taskID

	att := pirunners.NewAttachments()
	rel := att.Acquire(jobID)
	fx.deps.Attachments = att

	stub := newStub()
	stub.onPrompt = nil

	driver := make(chan struct{})
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			close(driver)
		}
	}
	waitForCorrections := func(target int, deadline time.Duration) error {
		end := time.Now().Add(deadline)
		for time.Now().Before(end) {
			run, err := fx.deps.Runs.GetRun(ctx, jobID)
			if err == nil && run.CorrectionsUsed >= target {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("timeout")
	}
	go func() {
		<-driver
		// One attached no-signal turn — must be a free skip. We give
		// stubRunner.SendPrompt's 5ms auto-scheduler enough slack to
		// land its own turn_end before we push ours, so the executor
		// can't accidentally consume the auto-end as "the attached
		// turn" and our explicit turn_end as a kickback.
		time.Sleep(SchedulerSlack)
		stub.turnEnds <- struct{}{}
		// Detach. Schedule three more turn_ends so the kickback budget
		// (2) is fully consumed: turn 2 → inc to 1 + kickback; turn 3
		// → inc to 2 + kickback; turn 4 → budget exceeded → fail.
		// Between turns wait for the executor's IncCorrections to land
		// before we push the next turn_end so the test stays
		// deterministic on slow CI.
		time.Sleep(SchedulerSlack)
		rel()
		stub.turnEnds <- struct{}{}
		if err := waitForCorrections(1, time.Second); err != nil {
			t.Errorf("waitForCorrections(1): %v", err)
			return
		}
		stub.turnEnds <- struct{}{}
		if err := waitForCorrections(2, time.Second); err != nil {
			t.Errorf("waitForCorrections(2): %v", err)
			return
		}
		stub.turnEnds <- struct{}{}
	}()

	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	err := exec.Run(ctx, jobID)
	if !errors.Is(err, executor.ErrAgentDidNotEmit) {
		t.Fatalf("want ErrAgentDidNotEmit, got %v", err)
	}
	run, _ := fx.deps.Runs.GetRun(ctx, jobID)
	if run.Status != runstore.StatusFailed {
		t.Fatalf("run.Status=%s (want failed)", run.Status)
	}
	if run.CorrectionsUsed != 2 {
		t.Fatalf("CorrectionsUsed=%d (want 2: attached turn skipped, then both kickbacks consumed)", run.CorrectionsUsed)
	}
}

// ---- registry handoff tests ---------------------------------------------
//
// These tests cover the executor's interaction with the runner Registry:
//  - Standard pi runners (RunnerHandle) are registered while live and
//    cleared once the executor returns.
//  - When the executor exits with an error (e.g. SendPrompt fails) the
//    deferred Unregister still runs.
//  - Custom JS runners (no RunnerHandle implementation) deliberately
//    skip registration so /input + /abort answer 409 cleanly.

// handleStub is a stubRunner extended with the RunnerHandle interface so
// it can land in the registry. SendCommand returns success=true; Abort
// is a no-op; IsStreaming returns false.
type handleStub struct {
	*stubRunner
	abortCount int
	mu         sync.Mutex
}

func newHandleStub() *handleStub {
	return &handleStub{stubRunner: newStub()}
}

func (h *handleStub) SendCommand(c pi.Command) (<-chan pi.Response, error) {
	ch := make(chan pi.Response, 1)
	ch <- pi.Response{ID: c.ID, Command: c.Type, Success: true}
	close(ch)
	return ch, nil
}

func (h *handleStub) Abort(ctx context.Context) error {
	h.mu.Lock()
	h.abortCount++
	h.mu.Unlock()
	return h.stubRunner.Abort(ctx)
}

func (h *handleStub) IsStreaming() bool { return false }

// TestRun_RegistersHandleWhileLive: a runner implementing RunnerHandle
// lands in the registry between spawn and cleanup. After Run returns the
// registry entry must be gone (defer Unregister).
func TestRun_RegistersHandleWhileLive(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "RegistryHappy", "dev")

	reg := pirunners.NewRegistry()
	fx.deps.Runners = reg

	stub := newHandleStub()
	observed := make(chan bool, 1)
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			_, err := reg.Get(jobID)
			observed <- err == nil
			if _, err := fx.deps.Signals.Emit(ctx, taskID, "review"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}

	exec := executor.New(fx.deps, func(ctx context.Context, _ pi.Opts) (executor.PiRunner, error) {
		return stub, nil
	}, fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case got := <-observed:
		if !got {
			t.Fatalf("runner not in registry while live")
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for in-flight registry probe")
	}
	if _, err := reg.Get(jobID); !errors.Is(err, pirunners.ErrNotRegistered) {
		t.Fatalf("runner still in registry after Run returned: err=%v", err)
	}
}

// TestRun_UnregistersOnError: the deferred Unregister still runs when
// the executor exits via the handleRunError path (here triggered by a
// SendPrompt failure injected through the stub).
func TestRun_UnregistersOnError(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	_, jobID := fx.makeRun(t, "RegistryError", "dev")

	reg := pirunners.NewRegistry()
	fx.deps.Runners = reg

	stub := newHandleStub()
	stub.onPrompt = nil
	// Inject a SendPrompt error via a sentinel that the wrapper below
	// translates into a real error. We re-use stubRunner.SendPrompt by
	// swapping the stub's onPrompt to a callback that closes the events
	// channel; but the simpler approach is to wrap the factory so the
	// returned runner reports a SendPrompt error.
	failing := &failingHandleStub{handleStub: stub, err: errors.New("forced send error")}
	exec := executor.New(fx.deps, func(ctx context.Context, _ pi.Opts) (executor.PiRunner, error) {
		return failing, nil
	}, fx.cfg)
	if err := exec.Run(ctx, jobID); err == nil {
		t.Fatalf("Run unexpectedly succeeded; want SendPrompt failure")
	}
	if _, err := reg.Get(jobID); !errors.Is(err, pirunners.ErrNotRegistered) {
		t.Fatalf("runner still in registry after error exit: err=%v", err)
	}
}

// failingHandleStub forces SendPrompt to return an error so the
// executor takes the handleRunError path.
type failingHandleStub struct {
	*handleStub
	err error
}

func (f *failingHandleStub) SendPrompt(ctx context.Context, m string) error {
	return f.err
}

// TestRun_DoesNotRegisterPlainStub: stubRunner (no RunnerHandle methods)
// must NOT end up in the registry; /input + /abort would then answer
// 409 "runner not registered for job". This documents the custom-JS-
// runner path, which deliberately skips registration.
func TestRun_DoesNotRegisterPlainStub(t *testing.T) {
	fx := newExecFixture(t)
	defer fx.close()
	ctx := context.Background()
	taskID, jobID := fx.makeRun(t, "RegistrySkip", "dev")

	reg := pirunners.NewRegistry()
	fx.deps.Runners = reg

	stub := newStub()
	probeDone := make(chan struct{})
	stub.onPrompt = func(prompt string, attempt int) {
		if attempt == 1 {
			defer close(probeDone)
			if _, err := reg.Get(jobID); !errors.Is(err, pirunners.ErrNotRegistered) {
				t.Errorf("plain stub leaked into registry: err=%v", err)
			}
			if _, err := fx.deps.Signals.Emit(ctx, taskID, "review"); err != nil {
				t.Errorf("Emit: %v", err)
			}
		}
	}
	exec := executor.New(fx.deps, stubFactory(stub), fx.cfg)
	if err := exec.Run(ctx, jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-probeDone
	if _, err := reg.Get(jobID); !errors.Is(err, pirunners.ErrNotRegistered) {
		t.Fatalf("registry not empty after Run: err=%v", err)
	}
}
