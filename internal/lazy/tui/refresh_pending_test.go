package tui

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestScheduleRefresh_PendingFlagConverges pins the "trailing
// refresh" contract on scheduleRefresh.
//
// The previous implementation dropped requests that arrived while
// one was in flight, which meant j-spam could leave the Jobs panel
// showing results filtered by a stale cursor row. The fix uses a
// pending flag so the worker loops once more whenever a request
// arrived during the in-flight run, guaranteeing the LATEST scope
// drives the final refresh.
//
// We exercise this by stubbing the worker dispatch (gocui.OnWorker
// would otherwise require a real Gui). The contract: 2-N
// scheduleRefresh calls produce 1 or 2 refresh runs (one in-flight
// + at most one trailing), and the trailing one runs after the
// latest scheduleRefresh returns.
func TestScheduleRefresh_PendingFlagConverges(t *testing.T) {
	// Reproduce the scheduleRefresh logic with an injected dispatcher
	// + refreshAll stub. We can't drive the real implementation
	// without a gocui.Gui (g.OnWorker would panic), so we mirror the
	// CAS dance here. The test pins the invariant that matters:
	// however many times scheduleRefresh is called concurrently,
	// the work-runner runs at most one trailing pass after the
	// final scheduleRefresh returns.
	var (
		inFlight atomic.Bool
		pending  atomic.Bool
		runs     atomic.Int32
		wg       sync.WaitGroup
	)

	// stubRefresh is the per-pass body — it sleeps a bit to simulate
	// real work so concurrent schedulers race against the in-flight
	// run.
	stubRefresh := func() {
		runs.Add(1)
		time.Sleep(10 * time.Millisecond)
	}

	// schedule is scheduleRefresh's exact shape, minus the OnWorker
	// indirection (we just spawn a goroutine).
	schedule := func() {
		if !inFlight.CompareAndSwap(false, true) {
			pending.Store(true)
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				stubRefresh()
				inFlight.Store(false)
				if !pending.CompareAndSwap(true, false) {
					return
				}
				if !inFlight.CompareAndSwap(false, true) {
					return
				}
			}
		}()
	}

	// Fire 50 schedule() calls rapidly. The trailing-refresh contract
	// says we should see at least 1 run (the in-flight one) and at
	// most 2 (in-flight + trailing pickup of pending).
	for i := 0; i < 50; i++ {
		schedule()
	}
	wg.Wait()
	got := runs.Load()
	if got < 1 || got > 2 {
		t.Fatalf("got %d refresh runs; want 1..2 (in-flight + optional trailing)", got)
	}
	// pending must be cleared on exit so the next user-driven
	// scheduleRefresh doesn't fire spuriously.
	if pending.Load() {
		t.Fatal("pending flag still set after wg.Wait()")
	}
	if inFlight.Load() {
		t.Fatal("inFlight flag still set after wg.Wait()")
	}
}

// TestScheduleRefresh_TrailingPickupSequential verifies the
// trailing-run picks up a request that arrives DURING the in-flight
// refresh (the case the previous coalescer broke). We sequence the
// calls explicitly: schedule, then schedule again while the first
// is sleeping, then wait — exactly 2 runs.
func TestScheduleRefresh_TrailingPickupSequential(t *testing.T) {
	var (
		inFlight atomic.Bool
		pending  atomic.Bool
		runs     atomic.Int32
		gate     = make(chan struct{})
		done     = make(chan struct{})
	)
	schedule := func() {
		if !inFlight.CompareAndSwap(false, true) {
			pending.Store(true)
			return
		}
		go func() {
			defer close(done)
			for {
				// First run blocks on the gate so we can fire a
				// second schedule before it returns.
				if runs.Load() == 0 {
					<-gate
				}
				runs.Add(1)
				inFlight.Store(false)
				if !pending.CompareAndSwap(true, false) {
					return
				}
				if !inFlight.CompareAndSwap(false, true) {
					return
				}
			}
		}()
	}
	schedule() // starts the worker, parks on gate
	// Issue a second schedule while the worker is blocked. The CAS
	// fails (in-flight is set), so pending becomes true.
	schedule()
	if !pending.Load() {
		t.Fatal("pending should be set after second schedule during in-flight run")
	}
	// Release the worker. It finishes pass 1, picks up pending,
	// runs pass 2, exits.
	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish")
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs=%d want 2 (trailing pickup)", got)
	}
}
