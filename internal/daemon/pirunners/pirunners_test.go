package pirunners_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/pirunners"
)

type fakeHandle struct{}

func (fakeHandle) SendCommand(pi.Command) (<-chan pi.Response, error) {
	ch := make(chan pi.Response, 1)
	close(ch)
	return ch, nil
}
func (fakeHandle) Abort(context.Context) error { return nil }
func (fakeHandle) IsStreaming() bool           { return false }

func TestRegistry_RegisterGetUnregister(t *testing.T) {
	r := pirunners.NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("fresh len=%d", r.Len())
	}
	if _, err := r.Get("missing"); err == nil {
		t.Fatalf("expected ErrNotRegistered")
	}
	r.Register("job-1", fakeHandle{})
	if r.Len() != 1 {
		t.Fatalf("len after Register=%d", r.Len())
	}
	h, err := r.Get("job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if h == nil {
		t.Fatal("handle nil")
	}
	r.Unregister("job-1")
	if r.Len() != 0 {
		t.Fatalf("len after Unregister=%d", r.Len())
	}
	// Idempotent.
	r.Unregister("job-1")
}

func TestRegistry_NilSafe(t *testing.T) {
	var r *pirunners.Registry
	r.Register("x", fakeHandle{}) // no panic
	r.Unregister("x")
	if r.Len() != 0 {
		t.Fatalf("nil len=%d", r.Len())
	}
	if _, err := r.Get("x"); err == nil {
		t.Fatalf("expected error on nil registry")
	}
}

func TestAttachments_AcquireReleaseCounts(t *testing.T) {
	a := pirunners.NewAttachments()
	if a.Attached("job-1") {
		t.Fatal("Attached on empty counter")
	}
	rel1 := a.Acquire("job-1")
	if !a.Attached("job-1") {
		t.Fatal("not Attached after Acquire")
	}
	if got := a.Count("job-1"); got != 1 {
		t.Fatalf("Count=%d", got)
	}
	rel2 := a.Acquire("job-1")
	if got := a.Count("job-1"); got != 2 {
		t.Fatalf("Count=%d after second Acquire", got)
	}
	rel1()
	if !a.Attached("job-1") {
		t.Fatal("dropped to detached too early")
	}
	rel1() // double release is a no-op
	if got := a.Count("job-1"); got != 1 {
		t.Fatalf("Count=%d after duplicate release", got)
	}
	rel2()
	if a.Attached("job-1") {
		t.Fatal("still attached after both released")
	}
	if got := a.Count("job-1"); got != 0 {
		t.Fatalf("Count=%d after both released", got)
	}
}

func TestAttachments_ConcurrentAcquireRelease(t *testing.T) {
	a := pirunners.NewAttachments()
	const N = 200
	var wg sync.WaitGroup
	releases := make(chan func(), N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			releases <- a.Acquire("job-1")
		}()
	}
	wg.Wait()
	close(releases)
	if got := a.Count("job-1"); got != N {
		t.Fatalf("Count=%d (want %d)", got, N)
	}
	var rwg sync.WaitGroup
	for rel := range releases {
		rwg.Add(1)
		go func(r func()) {
			defer rwg.Done()
			r()
		}(rel)
	}
	rwg.Wait()
	if a.Attached("job-1") {
		t.Fatalf("counter not back to zero: %d", a.Count("job-1"))
	}
}

func TestAttachments_NilSafe(t *testing.T) {
	var a *pirunners.Attachments
	rel := a.Acquire("x") // no panic
	rel()
	if a.Attached("x") {
		t.Fatal("nil attachments reported attached")
	}
	ch, cancel := a.Subscribe("x")
	if _, ok := <-ch; ok {
		t.Fatal("nil Subscribe channel must be closed, not yield a value")
	}
	cancel() // no panic on second close path either.
}

// TestAttachments_SubscribeInitialSnapshot: Subscribe must always
// produce the *current* count as the first read, even when no later
// transitions happen — otherwise a UI that subscribes mid-attach
// would render attach_count=0 until the next acquire/release.
func TestAttachments_SubscribeInitialSnapshot(t *testing.T) {
	a := pirunners.NewAttachments()
	defer a.Acquire("job-1")() // count = 1
	rel := a.Acquire("job-1")  // count = 2
	defer rel()

	ch, cancel := a.Subscribe("job-1")
	defer cancel()
	select {
	case v := <-ch:
		if v != 2 {
			t.Fatalf("initial snapshot=%d want 2", v)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial snapshot")
	}
}

// TestAttachments_SubscribeNotifiesTransitions: a subscriber sees
// every distinct attach_count value (collapsed under burst by the
// non-blocking sender, which is fine for status-bar semantics).
func TestAttachments_SubscribeNotifiesTransitions(t *testing.T) {
	a := pirunners.NewAttachments()
	ch, cancel := a.Subscribe("job-1")
	defer cancel()

	// Drain initial snapshot (0).
	select {
	case v := <-ch:
		if v != 0 {
			t.Fatalf("initial=%d want 0", v)
		}
	case <-time.After(time.Second):
		t.Fatal("initial timeout")
	}

	rel1 := a.Acquire("job-1")
	waitValue(t, ch, 1)
	rel2 := a.Acquire("job-1")
	waitValue(t, ch, 2)
	rel1()
	waitValue(t, ch, 1)
	rel2()
	waitValue(t, ch, 0)
}

// TestAttachments_SubscribeCancelStopsDelivery: after cancel(), the
// channel is closed and subsequent transitions are not delivered.
func TestAttachments_SubscribeCancelStopsDelivery(t *testing.T) {
	a := pirunners.NewAttachments()
	ch, cancel := a.Subscribe("job-1")
	// Drain initial 0.
	<-ch
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel must be closed after cancel")
	}
	// Subsequent acquire must not panic (no zombie subscriber).
	rel := a.Acquire("job-1")
	rel()
	// Idempotent cancel.
	cancel()
}

// TestAttachments_SubscribeIsolatedPerJob: notifications for job-A
// don't reach a subscriber to job-B.
func TestAttachments_SubscribeIsolatedPerJob(t *testing.T) {
	a := pirunners.NewAttachments()
	chB, cancelB := a.Subscribe("job-B")
	defer cancelB()
	// Drain initial 0.
	<-chB
	rel := a.Acquire("job-A")
	defer rel()
	// Give the notifier a beat to deliver if it were going to (it shouldn't).
	time.Sleep(50 * time.Millisecond)
	select {
	case v := <-chB:
		t.Fatalf("job-B subscriber saw value %d from job-A", v)
	default:
	}
}

// waitValue is a tiny helper that lets the test poll the subscribe
// channel for the next value with a bounded timeout. We loop because
// the non-blocking sender may collapse bursts but a single transition
// is always delivered (the channel has cap=1).
func waitValue(t *testing.T, ch <-chan int, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed waiting for %d", want)
			}
			if v == want {
				return
			}
			// Wrong value (a stale snapshot under burst). Keep reading.
		case <-deadline:
			t.Fatalf("timed out waiting for value %d", want)
		}
	}
}
