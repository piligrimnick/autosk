package tui

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// TestPumpLiveLoop_ThrottleCoalescesBursts verifies the 30ms-window
// throttle (risk #4 from the impl plan): a tight burst of N events
// must coalesce into ≪ N flush calls. Lazygit's pkg/tasks pattern.
//
// We push 50 events 1ms apart and assert the flush callback ran
// AT MOST 3 times (typically 1, sometimes 2 if the timer fires once
// mid-burst, never 50). Compared to the un-throttled baseline (50
// flushes for 50 events) this is the regression we care about.
func TestPumpLiveLoop_ThrottleCoalescesBursts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan datasource.LiveEvent, 64)
	var (
		flushes   atomic.Int32
		total     atomic.Int32
		mu        sync.Mutex
		batches   [][]int // sizes of each flush's batch
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpLiveLoop(ctx, events, 30*time.Millisecond, func(b []datasource.LiveEvent) {
			flushes.Add(1)
			total.Add(int32(len(b)))
			mu.Lock()
			batches = append(batches, []int{len(b)})
			mu.Unlock()
		})
	}()

	// Push 50 events as fast as possible. Even with select-loop
	// overhead they all land before the 30ms timer fires.
	for i := 0; i < 50; i++ {
		events <- datasource.LiveEvent{Kind: "message"}
	}
	// Give the throttle plenty of time to fire and the goroutine to
	// process. 80ms > 30ms throttle + scheduler jitter.
	time.Sleep(80 * time.Millisecond)
	// Close the channel so the loop exits.
	close(events)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pumpLiveLoop did not return after channel close")
	}

	if got := flushes.Load(); got < 1 || got > 3 {
		mu.Lock()
		t.Fatalf("flushes=%d want 1..3 (burst should coalesce); batches=%v", got, batches)
	}
	if total.Load() != 50 {
		t.Fatalf("total events flushed=%d want 50 (none dropped)", total.Load())
	}
}

// TestPumpLiveLoop_FlushOnChannelClose: when the events channel
// closes with a pending batch, the loop MUST flush it before
// returning (otherwise the last event is silently dropped).
func TestPumpLiveLoop_FlushOnChannelClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan datasource.LiveEvent, 4)
	var flushes atomic.Int32
	var total atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpLiveLoop(ctx, events, time.Hour, func(b []datasource.LiveEvent) {
			flushes.Add(1)
			total.Add(int32(len(b)))
		})
	}()
	events <- datasource.LiveEvent{Kind: "message"}
	events <- datasource.LiveEvent{Kind: "message"}
	close(events)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not return on close")
	}
	if got := flushes.Load(); got != 1 {
		t.Fatalf("flushes=%d want 1 (drain-on-close)", got)
	}
	if total.Load() != 2 {
		t.Fatalf("total=%d want 2", total.Load())
	}
}

// TestPumpLiveLoop_FlushOnCtxCancel: ctx cancellation also flushes
// the pending batch before exit.
func TestPumpLiveLoop_FlushOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan datasource.LiveEvent, 4)
	var flushes atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		pumpLiveLoop(ctx, events, time.Hour, func(b []datasource.LiveEvent) {
			flushes.Add(1)
		})
	}()
	events <- datasource.LiveEvent{Kind: "message"}
	// Give the loop time to queue the event.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not return on ctx cancel")
	}
	if got := flushes.Load(); got != 1 {
		t.Fatalf("flushes=%d want 1 (drain-on-cancel)", got)
	}
}
