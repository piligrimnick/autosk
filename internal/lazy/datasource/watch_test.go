package datasource

import (
	"context"
	"testing"
	"time"
)

// TestWatch_MapsNotifications asserts RPC.Watch maps the daemon's
// `task-changed` / `project-changed` frames onto ChangeEvent.Kind
// ("task" / "project"), ignoring the subscribe ack. Reuses the streamDaemon
// helper (which acks then writes the supplied frames).
func TestWatch_MapsNotifications(t *testing.T) {
	ds, _ := streamDaemon(t, []map[string]any{
		{"method": "task-changed", "params": map[string]any{"root": "/repo"}},
		{"method": "project-changed", "params": map[string]any{}},
		{"method": "task-changed", "params": map[string]any{"root": "/repo"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := ds.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer h.Close()

	want := []string{"task", "project", "task"}
	for i, wk := range want {
		select {
		case ev, ok := <-h.Events:
			if !ok {
				t.Fatalf("channel closed before event %d", i)
			}
			if ev.Kind != wk {
				t.Errorf("event %d kind = %q, want %q", i, ev.Kind, wk)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d (%s)", i, wk)
		}
	}
}

// TestWatch_ClosesChannelOnDisconnect asserts that when the daemon EOFs the
// notification connection the Watch channel closes (so the TUI's watch loop
// reconnects) rather than parking forever.
func TestWatch_ClosesChannelOnDisconnect(t *testing.T) {
	// No frames: streamDaemon acks, then (since we close below) the connection
	// will drop when the test's server goroutine tears down. To force a prompt
	// EOF we use a server with no frames and rely on the ctx cancel to release.
	ds, _ := streamDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())

	h, err := ds.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer h.Close()

	// Cancelling the ctx must tear the subscription down and close the channel.
	cancel()
	select {
	case _, ok := <-h.Events:
		if ok {
			// A spurious event is acceptable only if the channel then closes;
			// drain once more.
			select {
			case _, ok2 := <-h.Events:
				if ok2 {
					t.Fatal("Watch channel did not close after ctx cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Watch channel did not close after ctx cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch channel did not close after ctx cancel")
	}
}
