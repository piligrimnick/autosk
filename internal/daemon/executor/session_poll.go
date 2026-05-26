package executor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"autosk/internal/daemon/pi"
)

// pi 0.74-0.75 creates session.jsonl lazily inside its session
// manager: the file path is stamped on the first persist (after the
// first prompt is preflight-accepted), not at spawn time. The
// executor used to query get_state synchronously right after spawn,
// before SendPrompt, so SessionFile came back empty and daemon_runs
// .session_path stayed unset forever.
//
// pollSessionInfo re-polls get_state in a background goroutine
// kicked off after SendPrompt. It retries with exponential backoff
// until pi reports a populated SessionFile, then writes through
// runs.SetPISession exactly once. The retry budget covers slow
// first-token latencies on big models; individual sleeps are capped
// so an early success doesn't strand the goroutine on a multi-second
// timer.
const (
	// sessionPollBudget bounds the total wall-clock the goroutine
	// spends waiting for pi to stamp sessionFile.
	sessionPollBudget = 30 * time.Second
	// sessionPollAttemptCap bounds a single get_state RPC. If pi is
	// busy on a turn, the per-attempt deadline fires and we sleep +
	// retry rather than treating the slowness as fatal.
	sessionPollAttemptCap = 2 * time.Second
	// sessionPollInitDelay is the first inter-attempt sleep, doubled
	// after every empty response: 100ms, 200ms, 400ms, 800ms, 1.6s,
	// 3.2s, capped at sessionPollMaxDelay.
	sessionPollInitDelay = 100 * time.Millisecond
	// sessionPollMaxDelay caps a single sleep so we don't park on a
	// 16s timer once the backoff exponent grows.
	sessionPollMaxDelay = 5 * time.Second
)

// sessionInfoRunner is the slice of PiRunner the poller depends on.
// Splitting it out lets the unit test drive the helper directly
// without standing up the rest of the executor wiring.
type sessionInfoRunner interface {
	GetState(ctx context.Context) (pi.SessionInfo, error)
}

// sessionInfoSink is the slice of *runstore.Store the poller writes
// through. The real store satisfies it by SetPISession's signature.
type sessionInfoSink interface {
	SetPISession(ctx context.Context, jobID, sessionID, sessionPath string) error
}

// pollSessionOpts tunes pollSessionInfo. Zero values fall back to the
// sessionPoll* package constants. Used by tests to keep the poll
// short and deterministic.
type pollSessionOpts struct {
	Budget     time.Duration
	PerAttempt time.Duration
	InitDelay  time.Duration
	MaxDelay   time.Duration
}

func (o pollSessionOpts) withDefaults() pollSessionOpts {
	if o.Budget <= 0 {
		o.Budget = sessionPollBudget
	}
	if o.PerAttempt <= 0 {
		o.PerAttempt = sessionPollAttemptCap
	}
	if o.InitDelay <= 0 {
		o.InitDelay = sessionPollInitDelay
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = sessionPollMaxDelay
	}
	return o
}

// pollSessionInfo retries runner.GetState with exponential backoff
// until SessionFile populates or the budget runs out. On a populated
// SessionFile it writes through sink.SetPISession exactly once.
//
// Termination contract:
//   - SessionFile populated → one SetPISession call, then return.
//   - ctx.Done() at any point → no write, return.
//   - opts.Budget elapsed without success → no write, return.
//   - Non-deadline get_state error (pipe gone, decode failure,
//     "runner closed", get_state Success=false) → log warn, return.
//
// The SetPISession write uses context.Background() rather than the
// run ctx so a cancellation racing the write (e.g. the executor is
// shutting down at the same instant pi finally stamps the file)
// still persists what we just learned. This mirrors the bg-context
// pattern used by failTerminal / MarkDone for terminal-state writes.
//
// Defensive: SessionFile=="" is the loop's exit-only-via-backoff
// branch, so a buggy pi that never populates the field cannot
// clobber a previously-recorded path here.
func pollSessionInfo(ctx context.Context, runner sessionInfoRunner, sink sessionInfoSink, jobID string, opts pollSessionOpts) {
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.Budget)
	delay := opts.InitDelay
	for {
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}

		attemptCtx, cancel := context.WithTimeout(ctx, opts.PerAttempt)
		info, err := runner.GetState(attemptCtx)
		cancel()

		if err != nil {
			// Outer ctx died — exit clean. Filtering on ctx.Err()
			// first also covers the case where ctx.Cancel ran while
			// the attempt was in flight (attemptCtx then surfaces
			// Canceled, not DeadlineExceeded).
			if ctx.Err() != nil {
				return
			}
			// Per-attempt deadline → transient (pi was just slow on
			// this turn); fall through into the backoff sleep.
			// Anything else (pipe closed mid-call, decode failure,
			// pi reporting Success=false) means the runner is gone
			// or fundamentally broken and we won't recover.
			if !errors.Is(err, context.DeadlineExceeded) {
				slog.Default().Warn("executor: session info poll: get_state failed",
					"job", jobID,
					"err", err.Error())
				return
			}
		} else if info.SessionFile != "" {
			if werr := sink.SetPISession(context.Background(), jobID, info.SessionID, info.SessionFile); werr != nil {
				slog.Default().Warn("executor: session info poll: SetPISession failed",
					"job", jobID,
					"err", werr.Error())
			}
			return
		}

		// Sleep with exponential backoff, honoring ctx + remaining
		// budget. We trim the sleep to whatever budget is left so a
		// "100ms backoff then 30s deadline" doesn't burn a final
		// retry past the documented budget.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		sleep := delay
		if sleep > remaining {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay *= 2
		if delay > opts.MaxDelay {
			delay = opts.MaxDelay
		}
	}
}
