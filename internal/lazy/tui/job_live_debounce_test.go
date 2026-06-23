package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// debounceFakeDS is a refreshFakeDS specialisation that records every
// StreamSession call so the debounce tests can assert on the call count
// + the final session id.
type debounceFakeDS struct {
	refreshFakeDS
	calls atomic.Int32
	last  atomic.Value // string
}

func (f *debounceFakeDS) StreamSession(_ context.Context, sessionID string) (*datasource.LiveHandle, error) {
	f.calls.Add(1)
	f.last.Store(sessionID)
	// Return a handle whose channel never produces (the pump loop is
	// irrelevant for the debounce tests).
	ch := make(chan datasource.LiveEvent)
	return datasource.NewLiveHandle(ch, func() error { close(ch); return nil }), nil
}

func newDebounceGui(t *testing.T) (*Gui, *debounceFakeDS) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ds := &debounceFakeDS{}
	gu := &Gui{
		st:  newState(),
		ds:  ds,
		ctx: ctx,
	}
	return gu, ds
}

// TestScheduleSessionLive_DebounceCoalesces pins acceptance criterion 10:
// 20 calls in <1s collapse into ONE StreamSession invocation, made
// sessionLiveDebounce after the last call. Test scope: the real
// scheduleSessionLive (using the real timer) — we shorten the wait by
// just sleeping past the debounce window.
func TestScheduleSessionLive_DebounceCoalesces(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce test sleeps for 2s+; -short skips")
	}
	gu, ds := newDebounceGui(t)
	// Seed sessions so the selectedSession check inside openSessionLive sees a
	// running session.
	gu.st.withLock(func() {
		gu.st.sessions = []datasource.Session{{
			ID:     "session-final",
			Status: "running",
		}}
		gu.st.sessionCursor = 0
	})

	for i := 0; i < 20; i++ {
		gu.scheduleSessionLive("session-final", true)
		time.Sleep(5 * time.Millisecond) // 20 * 5ms = 100ms total burst
	}
	// Burst total = 100ms. Wait the debounce window + slack.
	time.Sleep(sessionLiveDebounce + 200*time.Millisecond)

	if got := ds.calls.Load(); got != 1 {
		t.Errorf("StreamSession calls = %d, want exactly 1 (20 schedules in 100ms must collapse)", got)
	}
	if got, _ := ds.last.Load().(string); got != "session-final" {
		t.Errorf("StreamSession last sessionID = %q, want session-final", got)
	}
	gu.stopSessionLive()
}

// TestScheduleSessionLive_CursorMoveCancelsPrevious: scheduling for sessionA
// then sessionB within the debounce window must open ONLY sessionB.
func TestScheduleSessionLive_CursorMoveCancelsPrevious(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce test sleeps for 2s+; -short skips")
	}
	gu, ds := newDebounceGui(t)
	gu.st.withLock(func() {
		gu.st.sessions = []datasource.Session{
			{ID: "session-A", Status: "running"},
			{ID: "session-B", Status: "running"},
		}
		gu.st.sessionCursor = 1
	})
	// Schedule sessionA — within the window, schedule sessionB. Only sessionB
	// should win.
	gu.scheduleSessionLive("session-A", true)
	time.Sleep(100 * time.Millisecond)
	gu.scheduleSessionLive("session-B", true)
	time.Sleep(sessionLiveDebounce + 200*time.Millisecond)

	if got := ds.calls.Load(); got != 1 {
		t.Errorf("StreamSession calls = %d, want 1", got)
	}
	if got, _ := ds.last.Load().(string); got != "session-B" {
		t.Errorf("StreamSession last sessionID = %q, want session-B (sessionA was superseded)", got)
	}
	gu.stopSessionLive()
}

// TestStopSessionLive_Idempotent: calling stopSessionLive when nothing is
// active is a no-op (no panic, no spurious StreamSession call).
func TestStopSessionLive_Idempotent(t *testing.T) {
	gu, _ := newDebounceGui(t)
	// First call on an empty state.
	gu.stopSessionLive()
	// Second call still empty.
	gu.stopSessionLive()
	// And once more.
	gu.stopSessionLive()
}

// TestScheduleSessionLive_TerminalSessionDoesNotOpen: running=false short-
// circuits without opening — and tears down any prior active stream.
func TestScheduleSessionLive_TerminalSessionDoesNotOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce test sleeps; -short skips")
	}
	gu, ds := newDebounceGui(t)
	// Pretend the cursor never points at a running session.
	gu.scheduleSessionLive("session-done", false)
	// Even after a long wait, no StreamSession call.
	time.Sleep(sessionLiveDebounce + 200*time.Millisecond)
	if got := ds.calls.Load(); got != 0 {
		t.Errorf("StreamSession calls = %d, want 0 for !running", got)
	}
}
