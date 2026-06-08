package rpcclient

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
)

// streamServer is a line-delimited JSON-RPC server that, unlike fakeServer,
// holds the connection open after the first request so it can model the
// persistent job.subscribe transport (subscribe ack → a tail of `job-event`
// notifications). It records every inbound request line so a test can assert
// the client sent job.unsubscribe on Close.
type streamServer struct {
	sock string
	mu   sync.Mutex
	reqs []map[string]any

	// gone closes the first time a served connection's read loop exits (the
	// client closed its end). It is the self-reap observable: the daemon keeps
	// the tail connection open, so the server only sees EOF when the client
	// itself releases the connection via JobStream.Close.
	goneOnce sync.Once
	gone     chan struct{}
}

// newStreamServer starts the server. onSubscribe is invoked once per accepted
// connection with the line encoder and the subscribe request's id; it writes
// the ack + notification frames. The server keeps reading (recording) until the
// client closes the connection.
func newStreamServer(t *testing.T, onSubscribe func(enc *json.Encoder, subID uint64)) *streamServer {
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
	s := &streamServer{sock: sock, gone: make(chan struct{})}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, onSubscribe)
		}
	}()
	return s
}

func (s *streamServer) serve(conn net.Conn, onSubscribe func(enc *json.Encoder, subID uint64)) {
	defer conn.Close()
	// Any return from serve means the client closed its end of the connection.
	defer s.goneOnce.Do(func() { close(s.gone) })
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	req := s.record(line)
	var subID uint64
	if v, ok := req["id"].(float64); ok {
		subID = uint64(v)
	}
	onSubscribe(enc, subID)
	// Drain (and record) any further requests — notably the job.unsubscribe
	// that Close() writes — until the client drops the connection.
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		s.record(line)
	}
}

func (s *streamServer) record(line []byte) map[string]any {
	var req map[string]any
	_ = json.Unmarshal(line, &req)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return req
}

// sawMethod reports whether any recorded request used the given method.
func (s *streamServer) sawMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reqs {
		if m, _ := r["method"].(string); m == method {
			return true
		}
	}
	return false
}

func jobEventFrame(kind, jobID string, eventID int, payload map[string]any) map[string]any {
	params := map[string]any{"kind": kind, "job_id": jobID, "event_id": eventID}
	for k, v := range payload {
		params[k] = v
	}
	return map[string]any{"method": "job-event", "params": params}
}

func mustClient(t *testing.T, sock string) *Client {
	t.Helper()
	cli, err := New(Options{Sock: sock, Cwd: "/repo", NoAutoSpawn: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return cli
}

// TestJobSubscribe_DemuxOrder asserts the readLoop forwards `job-event`
// notifications onto the channel in order (ignoring the subscribe ack) and that
// Close() is idempotent and emits job.unsubscribe.
func TestJobSubscribe_DemuxOrder(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		// Subscribe ack (a plain response) — readLoop must ignore it.
		_ = enc.Encode(map[string]any{"id": subID, "result": map[string]any{"ok": true}})
		// Two message frames, a status, then a terminal done.
		_ = enc.Encode(jobEventFrame("message", "job-1", 1, map[string]any{
			"event": map[string]any{"kind": "assistant", "text": "hello"}}))
		_ = enc.Encode(jobEventFrame("message", "job-1", 2, map[string]any{
			"event": map[string]any{"kind": "assistant", "text": "world"}}))
		_ = enc.Encode(jobEventFrame("status", "job-1", 0, map[string]any{
			"job": map[string]any{"job_id": "job-1", "status": "running"}}))
		_ = enc.Encode(jobEventFrame("done", "job-1", 0, map[string]any{
			"job": map[string]any{"job_id": "job-1", "status": "done"}}))
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := cli.JobSubscribe(ctx, "job-1", SubscribeOptions{Attach: true, Full: true})
	if err != nil {
		t.Fatalf("JobSubscribe: %v", err)
	}

	want := []struct {
		kind    string
		eventID int64
		text    string
		status  string
	}{
		{kind: "message", eventID: 1, text: "hello"},
		{kind: "message", eventID: 2, text: "world"},
		{kind: "status", status: "running"},
		{kind: "done", status: "done"},
	}
	for i, w := range want {
		select {
		case ev := <-stream.Events():
			if ev.Kind != w.kind {
				t.Fatalf("event %d kind = %q, want %q", i, ev.Kind, w.kind)
			}
			if w.eventID != 0 && ev.EventID != w.eventID {
				t.Errorf("event %d id = %d, want %d", i, ev.EventID, w.eventID)
			}
			if w.text != "" && (ev.Event == nil || ev.Event.Text != w.text) {
				t.Errorf("event %d text = %+v, want %q", i, ev.Event, w.text)
			}
			if w.status != "" && (ev.Job == nil || ev.Job.Status != w.status) {
				t.Errorf("event %d job = %+v, want status %q", i, ev.Job, w.status)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d (%s)", i, w.kind)
		}
	}

	// Close is idempotent and must emit job.unsubscribe to the server.
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return srv.sawMethod("job.unsubscribe") },
		"server never received job.unsubscribe after Close")
}

// TestJobSubscribe_SubscribeError asserts an error response to the subscribe
// surfaces as a synthetic Kind=="error" frame, the channel then closes, AND the
// stream self-reaps the underlying connection (readLoop's deferred Close) so the
// server observes the client dropping its end.
//
// The context is long-lived (WithCancel, never expires during the assertions).
// A WithTimeout ctx would MASK the leak: on expiry the ctx-watcher goroutine
// reaps the connection regardless of the self-reap, so the server would see EOF
// even without the fix. With a non-expiring ctx the ONLY path that releases the
// connection is readLoop's deferred Close — exactly the code under test. The
// deferred cancel() reaps anything still parked if an assertion fails.
func TestJobSubscribe_SubscribeError(t *testing.T) {
	srv := newStreamServer(t, func(enc *json.Encoder, subID uint64) {
		_ = enc.Encode(map[string]any{"id": subID, "error": map[string]any{
			"code": CodeNotFound, "message": "no such job"}})
	})
	cli := mustClient(t, srv.sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := cli.JobSubscribe(ctx, "ghost", SubscribeOptions{})
	if err != nil {
		t.Fatalf("JobSubscribe: %v", err)
	}
	select {
	case ev, ok := <-stream.Events():
		if !ok {
			t.Fatal("channel closed before the synthetic error frame")
		}
		if ev.Kind != "error" {
			t.Fatalf("kind = %q, want error", ev.Kind)
		}
		if ev.Error == "" || !contains(ev.Error, "no such job") {
			t.Errorf("error = %q, want it to mention 'no such job'", ev.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the synthetic error frame")
	}
	// The channel closes without an external Close.
	select {
	case _, ok := <-stream.Events():
		if ok {
			t.Fatal("expected channel to close after the error frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close the channel after a subscribe error")
	}
	// The real self-reap guard: readLoop's deferred Close releases the
	// connection (and the ctx-watcher via s.closed), so the server sees the
	// client drop its end. Without the self-reap this never happens under a
	// long-lived ctx — the connection + goroutines leak until an external Close.
	select {
	case <-srv.gone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not self-reap the connection after a subscribe error (leak)")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
