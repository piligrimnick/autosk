package datasource

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/daemon/rpcclient"
)

// streamDaemon is a persistent line-delimited JSON-RPC server for job.subscribe:
// it answers the subscribe with an ack, writes a tail of `job-event`
// notifications, then keeps the connection open (mirroring autoskd, which never
// EOFs the tail on `done`). Returns an RPC datasource wired to it and a `gone`
// channel that closes when a served connection's read loop exits — i.e. when the
// client releases its end (the self-reap observable: the daemon never EOFs the
// tail, so the server only sees EOF when the client closes the connection).
func streamDaemon(t *testing.T, frames []map[string]any) (*RPC, <-chan struct{}) {
	t.Helper()
	// A short dir (not t.TempDir(), whose path embeds the long test name) so the
	// socket path stays under the macOS 104-byte sun_path limit.
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gone := make(chan struct{})
	var goneOnce sync.Once
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				defer goneOnce.Do(func() { close(gone) })
				r := bufio.NewReader(c)
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID uint64 `json:"id"`
				}
				_ = json.Unmarshal(line, &req)
				enc := json.NewEncoder(c)
				_ = enc.Encode(map[string]any{"id": req.ID, "result": map[string]any{"ok": true}})
				for _, f := range frames {
					_ = enc.Encode(f)
				}
				// Hold the connection open (do not EOF after `done`): the client
				// must tear down on the terminal frame itself.
				for {
					if _, err := r.ReadBytes('\n'); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	cli, err := rpcclient.New(rpcclient.Options{Sock: sock, Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return NewRPC(cli), gone
}

func notifyFrame(kind string, payload map[string]any) map[string]any {
	params := map[string]any{"kind": kind, "job_id": "job-1"}
	for k, v := range payload {
		params[k] = v
	}
	return map[string]any{"method": "job-event", "params": params}
}

// TestStreamLive_MapsAndTearsDownOnDone asserts StreamLive maps each job-event
// frame to the right LiveEvent AND that the handle's channel CLOSES on the
// terminal `done` frame (the daemon keeps the socket open, so without the
// close-on-done teardown the channel would never close — a goroutine + fd leak).
func TestStreamLive_MapsAndTearsDownOnDone(t *testing.T) {
	ds, _ := streamDaemon(t, []map[string]any{
		notifyFrame("message", map[string]any{
			"event_id": 7, "event": map[string]any{"kind": "assistant", "text": "hello"}}),
		notifyFrame("status", map[string]any{
			"job": map[string]any{"job_id": "job-1", "status": "running"}}),
		notifyFrame("done", map[string]any{
			"job": map[string]any{"job_id": "job-1", "status": "done"}}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := ds.StreamLive(ctx, "job-1")
	if err != nil {
		t.Fatalf("StreamLive: %v", err)
	}
	defer h.Close()

	var got []LiveEvent
	closed := false
	for !closed {
		select {
		case ev, ok := <-h.Events:
			if !ok {
				closed = true
				break
			}
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("StreamLive did not tear down on `done` (channel never closed) — leak")
		}
	}

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[0].Kind != "message" || got[0].EventID != 7 || got[0].Message.Text != "hello" {
		t.Errorf("event 0 = %+v, want message/7/hello", got[0])
	}
	if got[1].Kind != "status" || got[1].Status.Status != "running" {
		t.Errorf("event 1 = %+v, want status/running", got[1])
	}
	if got[2].Kind != "done" || got[2].Status.Status != "done" {
		t.Errorf("event 2 = %+v, want done/done", got[2])
	}
}

// TestStreamLive_ReleasesConnOnDone asserts that the terminal `done` frame does
// not just close the LiveEvent channel but actually RELEASES the underlying
// connection: the streamDaemon keeps its socket open, so the server only sees
// the client drop its end if StreamLive's close-on-done teardown ran
// (StreamLive → JobStream.Close → conn.Close). The context is long-lived
// (WithCancel, never expires) so the teardown is driven solely by the terminal
// frame, not by ctx expiry — without the close-on-done the connection +
// goroutines leak past `done`.
func TestStreamLive_ReleasesConnOnDone(t *testing.T) {
	ds, gone := streamDaemon(t, []map[string]any{
		notifyFrame("status", map[string]any{
			"job": map[string]any{"job_id": "job-1", "status": "running"}}),
		notifyFrame("done", map[string]any{
			"job": map[string]any{"job_id": "job-1", "status": "done"}}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := ds.StreamLive(ctx, "job-1")
	if err != nil {
		t.Fatalf("StreamLive: %v", err)
	}
	defer h.Close()

	// Drain until the channel closes on `done`.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-h.Events:
			if ok {
				continue
			}
		case <-deadline:
			t.Fatal("StreamLive did not close the channel on `done`")
		}
		break
	}
	// The connection must have been released by the close-on-done teardown.
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLive did not release the connection on `done` (leak)")
	}
}

// TestStreamLive_ErrorFrame asserts a job-event error frame maps to a LiveEvent
// with Kind=="error" and a non-nil Err.
func TestStreamLive_ErrorFrame(t *testing.T) {
	ds, _ := streamDaemon(t, []map[string]any{
		notifyFrame("error", map[string]any{"error": "boom"}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := ds.StreamLive(ctx, "job-1")
	if err != nil {
		t.Fatalf("StreamLive: %v", err)
	}
	defer h.Close()

	select {
	case ev, ok := <-h.Events:
		if !ok {
			t.Fatal("channel closed before the error frame")
		}
		if ev.Kind != "error" || ev.Err == nil || ev.Err.Error() != "boom" {
			t.Errorf("event = %+v, want error/boom", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the error frame")
	}
}
