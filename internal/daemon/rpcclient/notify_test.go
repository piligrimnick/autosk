package rpcclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func noteFrame(method string, params map[string]any) map[string]any {
	return map[string]any{"method": method, "params": params}
}

// TestSubscribe_ForwardsNotifications asserts the readLoop forwards
// `task-changed` / `project-changed` notifications onto the channel in order
// (ignoring the subscribe ack) and that Close() is idempotent and emits
// task.unsubscribe.
func TestSubscribe_ForwardsNotifications(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		// Subscribe ack (a plain response) — readLoop must ignore it.
		_ = enc.Encode(map[string]any{"id": subID, "result": map[string]any{"subscribed": true}})
		_ = enc.Encode(noteFrame("task-changed", map[string]any{
			"root": "/repo", "db_path": "/repo/.autosk/db"}))
		_ = enc.Encode(noteFrame("project-changed", map[string]any{}))
		_ = enc.Encode(noteFrame("task-changed", map[string]any{
			"root": "/repo", "db_path": "/repo/.autosk/db"}))
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := cli.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := []string{"task-changed", "project-changed", "task-changed"}
	for i, wm := range want {
		select {
		case n := <-stream.Events():
			if n.Method != wm {
				t.Fatalf("note %d method = %q, want %q", i, n.Method, wm)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for notification %d (%s)", i, wm)
		}
	}

	// Close is idempotent and must emit task.unsubscribe to the server.
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return srv.sawMethod("task.unsubscribe") },
		"server never received task.unsubscribe after Close")
}

// TestSubscribe_SelfReapsOnError asserts an error response to the subscribe
// ends the stream (closes the channel) AND self-reaps the underlying connection
// (readLoop's deferred Close) so the server observes the client dropping its
// end. The context is long-lived (WithCancel, never expires) so the teardown is
// driven solely by the error response, not by ctx expiry — mirroring
// TestJobSubscribe_SubscribeError.
func TestSubscribe_SelfReapsOnError(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		_ = enc.Encode(map[string]any{"id": subID, "error": map[string]any{
			"code": CodeMethodNotFound, "message": "unknown method: task.subscribe"}})
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := cli.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// The channel closes without an external Close (error response ends it).
	select {
	case _, ok := <-stream.Events():
		if ok {
			t.Fatal("expected channel to close on subscribe error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close the channel after a subscribe error")
	}
	// The self-reap guard: readLoop's deferred Close releases the connection, so
	// the server sees the client drop its end under a long-lived ctx.
	select {
	case <-srv.gone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not self-reap the connection after a subscribe error (leak)")
	}
}
