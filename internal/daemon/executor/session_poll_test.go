package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/pi"
)

// fakeSessionRunner satisfies sessionInfoRunner with a programmable
// per-call response, recording the attempt count atomically.
type fakeSessionRunner struct {
	attempts atomic.Int64
	fn       func(attempt int64) (pi.SessionInfo, error)
}

func (f *fakeSessionRunner) GetState(_ context.Context) (pi.SessionInfo, error) {
	n := f.attempts.Add(1)
	return f.fn(n)
}

// fakeSessionSink satisfies sessionInfoSink and records every
// SetPISession call so tests can assert call count + payload.
type fakeSessionSink struct {
	mu    sync.Mutex
	calls []sessionSinkCall
}

type sessionSinkCall struct {
	JobID, SessionID, SessionPath string
}

func (f *fakeSessionSink) SetPISession(_ context.Context, jobID, sessionID, sessionPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sessionSinkCall{jobID, sessionID, sessionPath})
	return nil
}

func (f *fakeSessionSink) snapshot() []sessionSinkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sessionSinkCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestPollSessionInfo_LateArrivingSessionFile is the load-bearing
// regression: pi 0.74-0.75 returns SessionFile="" until the first
// persist after SendPrompt. The poll helper must keep retrying with
// backoff and call SetPISession exactly once, with the late-arriving
// path, on the iteration the file lands.
func TestPollSessionInfo_LateArrivingSessionFile(t *testing.T) {
	const latePath = "/tmp/late.jsonl"
	runner := &fakeSessionRunner{
		fn: func(n int64) (pi.SessionInfo, error) {
			if n < 3 {
				return pi.SessionInfo{SessionID: "sess-x"}, nil
			}
			return pi.SessionInfo{SessionID: "sess-x", SessionFile: latePath}, nil
		},
	}
	sink := &fakeSessionSink{}
	pollSessionInfo(context.Background(), runner, sink, "job-late", pollSessionOpts{
		Budget:     500 * time.Millisecond,
		PerAttempt: 50 * time.Millisecond,
		InitDelay:  2 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
	})
	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("SetPISession calls=%d (want 1): %+v", len(calls), calls)
	}
	if calls[0].SessionPath != latePath {
		t.Errorf("SessionPath=%q (want %q)", calls[0].SessionPath, latePath)
	}
	if calls[0].JobID != "job-late" || calls[0].SessionID != "sess-x" {
		t.Errorf("payload mismatch: %+v", calls[0])
	}
	if got := runner.attempts.Load(); got != 3 {
		t.Errorf("attempts=%d (want 3 — two empties + one success)", got)
	}
}

// TestPollSessionInfo_ContextCancelled covers the "exit clean on
// ctx.Done()" branch: even if a subsequent GetState would yield a
// populated SessionFile, a cancelled run ctx must short-circuit the
// loop before any SetPISession write lands.
func TestPollSessionInfo_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeSessionRunner{
		fn: func(n int64) (pi.SessionInfo, error) {
			// First call: return empty AND cancel — the loop's next
			// ctx.Err() check (top of the for, OR the select on
			// ctx.Done() during the backoff sleep) must exit before
			// we ever return a populated SessionFile.
			cancel()
			return pi.SessionInfo{SessionID: "sess-cancel"}, nil
		},
	}
	sink := &fakeSessionSink{}
	start := time.Now()
	pollSessionInfo(ctx, runner, sink, "job-cancel", pollSessionOpts{
		Budget:     500 * time.Millisecond,
		PerAttempt: 50 * time.Millisecond,
		InitDelay:  50 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
	})
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("cancel didn't unblock backoff promptly: %v", elapsed)
	}
	if calls := sink.snapshot(); len(calls) != 0 {
		t.Fatalf("SetPISession called %d times on cancelled ctx (want 0): %+v", len(calls), calls)
	}
}

// TestPollSessionInfo_BudgetTimeout covers the "exit clean on budget
// elapsed" branch: a misbehaving pi that never populates SessionFile
// must not pin the goroutine forever, and must not invent a
// SetPISession call out of an empty response.
func TestPollSessionInfo_BudgetTimeout(t *testing.T) {
	runner := &fakeSessionRunner{
		fn: func(n int64) (pi.SessionInfo, error) {
			return pi.SessionInfo{SessionID: "sess-stuck"}, nil
		},
	}
	sink := &fakeSessionSink{}
	const budget = 120 * time.Millisecond
	start := time.Now()
	pollSessionInfo(context.Background(), runner, sink, "job-stuck", pollSessionOpts{
		Budget:     budget,
		PerAttempt: 20 * time.Millisecond,
		InitDelay:  10 * time.Millisecond,
		MaxDelay:   30 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if elapsed < budget {
		t.Errorf("returned before budget: elapsed=%v budget=%v", elapsed, budget)
	}
	if elapsed > 4*budget {
		t.Errorf("vastly overshot budget: elapsed=%v budget=%v", elapsed, budget)
	}
	if calls := sink.snapshot(); len(calls) != 0 {
		t.Fatalf("SetPISession called %d times on perma-empty SessionFile (want 0): %+v", len(calls), calls)
	}
	if got := runner.attempts.Load(); got < 2 {
		t.Errorf("attempts=%d (want \u22652 over the budget window)", got)
	}
}

// TestPollSessionInfo_FatalGetStateErrorExitsClean pins the
// "unrecoverable error → log + exit" branch: a pipe-gone style error
// must not loop forever and must not call SetPISession.
func TestPollSessionInfo_FatalGetStateErrorExitsClean(t *testing.T) {
	runner := &fakeSessionRunner{
		fn: func(n int64) (pi.SessionInfo, error) {
			return pi.SessionInfo{}, errors.New("pi: runner closed")
		},
	}
	sink := &fakeSessionSink{}
	start := time.Now()
	pollSessionInfo(context.Background(), runner, sink, "job-dead", pollSessionOpts{
		Budget:     2 * time.Second,
		PerAttempt: 50 * time.Millisecond,
		InitDelay:  5 * time.Millisecond,
		MaxDelay:   50 * time.Millisecond,
	})
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("fatal error didn't exit promptly: %v", elapsed)
	}
	if calls := sink.snapshot(); len(calls) != 0 {
		t.Fatalf("SetPISession called on fatal-error path: %+v", calls)
	}
	if got := runner.attempts.Load(); got != 1 {
		t.Errorf("attempts=%d (want 1: exit on first fatal err)", got)
	}
}
