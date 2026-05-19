// Package pirunners holds the daemon-wide, in-memory registry of live
// pi runner handles plus the in-memory attach-counter that the SSE
// handler and the executor consult.
//
// Both data structures intentionally have no persistence: a daemon
// restart wipes any live runner (the child process is gone anyway)
// and the existing daemon_restart recovery in runstore handles the
// abandoned daemon_runs rows. Attach counters reset to zero on each
// daemon boot for the same reason.
//
// Plan: docs/plans/20260519-Attach-Plan.md §4.1 / §4.2.
package pirunners

import (
	"context"
	"errors"
	"sync"

	"autosk/internal/daemon/pi"
)

// RunnerHandle is the narrow subset of *pi.Runner that the daemon's
// HTTP handlers and executor need to drive the live pi process. The
// executor's PiRunner interface is a superset (it also covers the
// turn loop's WaitForAgentEnd / shutdown plumbing).
type RunnerHandle interface {
	// SendCommand encodes a JSON command onto pi's stdin and returns
	// the response channel registered for the command id. Used by the
	// input handler to dispatch prompt|steer|follow_up.
	SendCommand(c pi.Command) (<-chan pi.Response, error)
	// Abort tells pi to stop the in-flight run.
	Abort(ctx context.Context) error
	// IsStreaming reports whether pi is currently between agent_start
	// and agent_end. The input handler dispatches `prompt` when idle
	// and `steer` (or follow_up) when streaming.
	IsStreaming() bool
}

// ErrNotRegistered is returned by Registry.Get / Attachments.Acquire
// when no live runner exists for the requested jobID.
var ErrNotRegistered = errors.New("pirunners: job not registered")

// Registry maps jobID -> live RunnerHandle for every job currently
// running inside this daemon. The executor registers on spawn and
// unregisters in cleanup. Handlers read.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]RunnerHandle
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]RunnerHandle)}
}

// Register associates h with jobID. Overwrites any previous handle for
// the same id (this can only happen on a bug — runs have unique ids).
func (r *Registry) Register(jobID string, h RunnerHandle) {
	if r == nil || jobID == "" || h == nil {
		return
	}
	r.mu.Lock()
	r.entries[jobID] = h
	r.mu.Unlock()
}

// Unregister drops the entry for jobID if present. Idempotent.
func (r *Registry) Unregister(jobID string) {
	if r == nil || jobID == "" {
		return
	}
	r.mu.Lock()
	delete(r.entries, jobID)
	r.mu.Unlock()
}

// Get returns the registered handle for jobID, or ErrNotRegistered.
func (r *Registry) Get(jobID string) (RunnerHandle, error) {
	if r == nil {
		return nil, ErrNotRegistered
	}
	r.mu.RLock()
	h, ok := r.entries[jobID]
	r.mu.RUnlock()
	if !ok {
		return nil, ErrNotRegistered
	}
	return h, nil
}

// Len returns the number of currently-registered runners. For tests
// and the /healthz observability path.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Attachments is the in-memory attach counter. Each call to Acquire
// increments the counter for jobID by one and returns a release
// function that decrements it once (idempotent).
//
// The SSE handler invokes Acquire on connection open with `?attach=true`
// and defers release on disconnect — that is the entire attach
// lifecycle (no separate /attach POST endpoint). The executor consults
// Attached(jobID) on every turn boundary to decide whether to skip
// correction prompts and disarm the idle timeout.
//
// Subscribe is the SSE-side hook: a subscriber receives the new count
// on every transition (and an initial snapshot at subscription time)
// so the handler can re-emit a status event without polling.
type Attachments struct {
	mu     sync.Mutex
	counts map[string]int
	// subs is the per-job list of active subscribers. Mutations of
	// counts and subs share the same mutex so a subscriber added
	// after an Acquire never misses a transition.
	subs map[string][]*attachSub
}

// attachSub is one Subscribe() handle. ch is buffered (cap=1) and
// uses non-blocking sends: a slow consumer just keeps the most recent
// count, which is exactly the right semantics for a UI status bar.
type attachSub struct {
	ch chan int
}

// NewAttachments constructs an empty attach counter.
func NewAttachments() *Attachments {
	return &Attachments{
		counts: make(map[string]int),
		subs:   make(map[string][]*attachSub),
	}
}

// Acquire bumps the counter for jobID and returns a one-shot release
// function. Release is safe to call from any goroutine; a second call
// is a no-op.
func (a *Attachments) Acquire(jobID string) (release func()) {
	if a == nil || jobID == "" {
		return func() {}
	}
	a.mu.Lock()
	a.counts[jobID]++
	count := a.counts[jobID]
	a.notifyLocked(jobID, count)
	a.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			n := a.counts[jobID]
			if n <= 1 {
				delete(a.counts, jobID)
				a.notifyLocked(jobID, 0)
			} else {
				a.counts[jobID] = n - 1
				a.notifyLocked(jobID, n-1)
			}
		})
	}
}

// Attached reports whether at least one client is currently attached
// to jobID.
func (a *Attachments) Attached(jobID string) bool {
	if a == nil || jobID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[jobID] > 0
}

// Count returns the current attach count for jobID. For tests and
// observability.
func (a *Attachments) Count(jobID string) int {
	if a == nil || jobID == "" {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[jobID]
}

// Subscribe returns a channel that receives the attach count for
// jobID on every Acquire/release transition, plus an initial snapshot
// emitted synchronously before Subscribe returns. The channel is
// closed by the returned cancel function (and only by it) — call
// cancel exactly once, typically via defer, to detach the subscriber.
//
// The channel has buffer cap=1 and uses non-blocking sends: under
// burst transitions a slow consumer reads only the most recent count.
// That is the right shape for a status-bar consumer, which only cares
// about the latest value at render time.
func (a *Attachments) Subscribe(jobID string) (<-chan int, func()) {
	ch := make(chan int, 1)
	if a == nil || jobID == "" {
		close(ch)
		return ch, func() {}
	}
	sub := &attachSub{ch: ch}
	a.mu.Lock()
	a.subs[jobID] = append(a.subs[jobID], sub)
	initial := a.counts[jobID]
	a.mu.Unlock()
	// Seed the initial value via the non-blocking primitive so
	// Subscribe-then-immediate-read returns the current count even
	// when no transition has happened.
	sendNonBlocking(ch, initial)

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			a.mu.Lock()
			list := a.subs[jobID]
			for i, s := range list {
				if s == sub {
					a.subs[jobID] = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(a.subs[jobID]) == 0 {
				delete(a.subs, jobID)
			}
			a.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// notifyLocked fans the new count out to every subscriber for jobID.
// Must be called with a.mu held; sends are non-blocking so a slow or
// dropped consumer cannot block Acquire/release.
func (a *Attachments) notifyLocked(jobID string, count int) {
	for _, s := range a.subs[jobID] {
		sendNonBlocking(s.ch, count)
	}
}

// sendNonBlocking attempts to send v on ch; if ch's buffer is full it
// first drains one slot and retries. This collapses bursts to the
// most recent value without ever blocking the caller.
func sendNonBlocking(ch chan int, v int) {
	select {
	case ch <- v:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- v:
		default:
		}
	}
}
