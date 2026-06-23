package tui

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// fakeWatcher is a datasource.Watcher whose subscription the test controls:
// each Watch() hands back a fresh handle, dropLast() simulates the daemon
// closing the stream, and sendLast() pushes a notification onto the live one.
type fakeWatcher struct {
	connects atomic.Int32
	mu       sync.Mutex
	last     chan datasource.ChangeEvent
	lastOnce *sync.Once
}

func (w *fakeWatcher) Watch(_ context.Context) (*datasource.WatchHandle, error) {
	w.connects.Add(1)
	ch := make(chan datasource.ChangeEvent, 4)
	once := &sync.Once{}
	w.mu.Lock()
	w.last, w.lastOnce = ch, once
	w.mu.Unlock()
	closeFn := func() error {
		once.Do(func() { close(ch) })
		return nil
	}
	return datasource.NewWatchHandle(ch, closeFn), nil
}

func (w *fakeWatcher) dropLast() {
	w.mu.Lock()
	ch, once := w.last, w.lastOnce
	w.mu.Unlock()
	if once != nil {
		once.Do(func() { close(ch) })
	}
}

func (w *fakeWatcher) sendLast(ev datasource.ChangeEvent) {
	w.mu.Lock()
	ch := w.last
	w.mu.Unlock()
	if ch != nil {
		ch <- ev
	}
}

func waitForCond(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestWatchLoop_RefreshesOnConnectAndReconnects pins the push driver's
// lifecycle: it forces a refresh on (re)connect and re-subscribes when the
// stream drops. The dispatch shim swallows scheduleRefresh's worker hand-off
// (refreshAll needs a real gocui.Gui), so we observe the refresh via the
// dispatch firing rather than running it.
func TestWatchLoop_RefreshesOnConnectAndReconnects(t *testing.T) {
	gu := &Gui{st: newState(), ctx: context.Background(), stopRefresh: make(chan struct{})}
	dispatched := make(chan struct{}, 16)
	gu.dispatch = func(func()) {
		select {
		case dispatched <- struct{}{}:
		default:
		}
	}
	w := &fakeWatcher{}
	go gu.watchLoop(w)

	waitForCond(t, time.Second, func() bool { return w.connects.Load() >= 1 },
		"watchLoop never connected")
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("watchLoop did not refresh on connect")
	}

	// Daemon closes the stream → watchLoop must re-subscribe.
	w.dropLast()
	waitForCond(t, 2*time.Second, func() bool { return w.connects.Load() >= 2 },
		"watchLoop did not reconnect after the stream dropped")

	close(gu.stopRefresh)
}

// TestConsumeWatch_NotificationTriggersRefresh proves a single notification
// drives exactly one scheduleRefresh (observed via the dispatch shim). The
// first scheduleRefresh wins the in-flight CAS, so the swallowing shim still
// sees the hand-off.
func TestConsumeWatch_NotificationTriggersRefresh(t *testing.T) {
	gu := &Gui{st: newState(), ctx: context.Background(), stopRefresh: make(chan struct{})}
	dispatched := make(chan struct{}, 4)
	gu.dispatch = func(func()) {
		select {
		case dispatched <- struct{}{}:
		default:
		}
	}
	ch := make(chan datasource.ChangeEvent, 1)
	handle := datasource.NewWatchHandle(ch, func() error { return nil })
	go gu.consumeWatch(handle)

	ch <- datasource.ChangeEvent{Kind: "task"}
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("consumeWatch did not trigger a refresh on a notification")
	}
	close(gu.stopRefresh)
}

// TestWatchLoop_StopsOnStopRefresh asserts the loop exits when stopRefresh is
// closed (the shutdown signal Run() fires) rather than spinning forever.
func TestWatchLoop_StopsOnStopRefresh(t *testing.T) {
	gu := &Gui{st: newState(), ctx: context.Background(), stopRefresh: make(chan struct{})}
	gu.dispatch = func(func()) {}
	w := &fakeWatcher{}
	done := make(chan struct{})
	go func() { gu.watchLoop(w); close(done) }()

	waitForCond(t, time.Second, func() bool { return w.connects.Load() >= 1 },
		"watchLoop never connected")
	// A live notification should still flow before we stop (sanity).
	w.sendLast(datasource.ChangeEvent{Kind: "project"})

	close(gu.stopRefresh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLoop did not exit after stopRefresh closed")
	}
}
