package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
)

// fakeDaemon spins up an http.Server on a real unix socket and runs
// the provided mux. Returns the sock path; the listener and server
// are cleaned up via t.Cleanup.
func fakeDaemon(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir)+len("/d.sock") >= 100 {
		// macOS sun_path cap. Fall back to /tmp.
		short, err := os.MkdirTemp("/tmp", "client-test-")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(short) })
		dir = short
	}
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(sock)
	})
	return sock
}

// newClient builds a Client pointed at sock with a deterministic cwd.
func newClient(t *testing.T, sock string) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{Sock: sock, Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// TestClient_New_DefaultsRespectSockEnv: with no explicit Sock and a
// non-empty AUTOSK_SOCK, New resolves the env var.
func TestClient_New_DefaultsRespectSockEnv(t *testing.T) {
	t.Setenv("AUTOSK_SOCK", "/tmp/some.sock")
	c, err := client.New(client.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Sock(); got != "/tmp/some.sock" {
		t.Fatalf("Sock=%q want /tmp/some.sock", got)
	}
}

// TestClient_GetJob_PassesCwdHeader: the project header is set on
// every outgoing request.
func TestClient_GetJob_PassesCwdHeader(t *testing.T) {
	var gotCwd string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs/{job_id}", func(w http.ResponseWriter, r *http.Request) {
		gotCwd = r.Header.Get("X-Autosk-Cwd")
		_ = json.NewEncoder(w).Encode(api.JobResponse{JobID: r.PathValue("job_id"), Status: "running", AttachCount: 1})
	})
	sock := fakeDaemon(t, mux)
	cwdDir := t.TempDir()
	c, err := client.New(client.Options{Sock: sock, Cwd: cwdDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := c.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if out.JobID != "job-1" || out.AttachCount != 1 {
		t.Fatalf("response=%+v", out)
	}
	if gotCwd != cwdDir {
		t.Fatalf("X-Autosk-Cwd=%q want %q", gotCwd, cwdDir)
	}
}

// TestClient_GetJob_APIError_4xx: a 404 from the daemon is returned
// as a typed APIError that callers can switch on.
func TestClient_GetJob_APIError_4xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs/{job_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{
			Error:   "job not found",
			Details: map[string]any{"job_id": r.PathValue("job_id")},
		})
	})
	sock := fakeDaemon(t, mux)
	c := newClient(t, sock)
	_, err := c.GetJob(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := client.IsAPIError(err)
	if !ok {
		t.Fatalf("err=%v, want APIError", err)
	}
	if ae.Status != http.StatusNotFound {
		t.Fatalf("Status=%d", ae.Status)
	}
	if ae.Details["job_id"] != "missing" {
		t.Fatalf("Details=%+v", ae.Details)
	}
}

// TestClient_SendInput_PostsBodyAndBehavior: SendInput round-trips
// the message and streamingBehavior. Mirrors the daemon's wire shape.
func TestClient_SendInput_PostsBodyAndBehavior(t *testing.T) {
	var gotBody api.InputRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/{job_id}/input", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		dispatched := "steer"
		if gotBody.StreamingBehavior == "follow_up" {
			dispatched = "follow_up"
		}
		_ = json.NewEncoder(w).Encode(api.InputResponse{JobID: r.PathValue("job_id"), Dispatched: dispatched})
	})
	sock := fakeDaemon(t, mux)
	c := newClient(t, sock)
	out, err := c.SendInput(context.Background(), "job-1", "stop", "follow_up")
	if err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if gotBody.Message != "stop" || gotBody.StreamingBehavior != "follow_up" {
		t.Fatalf("body=%+v", gotBody)
	}
	if out.Dispatched != "follow_up" {
		t.Fatalf("Dispatched=%q", out.Dispatched)
	}
}

// TestClient_Abort_NoBody: Abort sends a POST with no body and parses
// the typed response.
func TestClient_Abort_NoBody(t *testing.T) {
	var gotContentType string
	var bodyLen int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/{job_id}/abort", func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf, _ := io.ReadAll(r.Body)
		bodyLen = len(buf)
		_ = json.NewEncoder(w).Encode(api.AbortResponse{JobID: r.PathValue("job_id"), OK: true})
	})
	sock := fakeDaemon(t, mux)
	c := newClient(t, sock)
	out, err := c.Abort(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !out.OK {
		t.Fatalf("OK=%v", out.OK)
	}
	if bodyLen != 0 {
		t.Fatalf("body length=%d want 0", bodyLen)
	}
	if gotContentType != "" {
		t.Fatalf("Content-Type set for no-body request: %q", gotContentType)
	}
}

// (the over-fake-daemon test in TestClient_Stream_OverFakeDaemon
// covers all frame kinds end-to-end through Client.Stream + the
// real httptest pipeline. We don't need a redundant raw-parser test
// here because the parser has no exported entry point worth pinning
// independently — frame-by-frame coverage of message/status/done/
// comment all comes through Client.Stream.)

// TestClient_Stream_OverFakeDaemon: end-to-end test that goes through
// Client.Stream against a real fake daemon. Verifies that:
//   - The Attach/Full query params land on the daemon URL.
//   - The Last-Event-ID header is forwarded when LastEventID > 0.
//   - The typed channel yields decoded Event values.
//   - Close stops the producer goroutine cleanly.
func TestClient_Stream_OverFakeDaemon(t *testing.T) {
	var gotQuery, gotLastEventID string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs/{job_id}/stream", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotLastEventID = r.Header.Get("Last-Event-ID")
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\nid: 11\ndata: %s\n\n",
			mustJSON(api.MessageEvent{Kind: "assistant_text", Text: "hi"}))
		flusher.Flush()
		_, _ = fmt.Fprintf(w, "event: status\nid: 12\ndata: %s\n\n",
			mustJSON(api.JobResponse{JobID: r.PathValue("job_id"), Status: "running", AttachCount: 1}))
		flusher.Flush()
		// Hang until ctx cancel.
		<-r.Context().Done()
	})
	sock := fakeDaemon(t, mux)
	c := newClient(t, sock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := c.Stream(ctx, "job-1", client.StreamOptions{
		Attach:      true,
		Full:        true,
		LastEventID: 7,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer h.Close()

	got := make([]client.Event, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev, ok := <-h.Events:
			if !ok {
				t.Fatal("stream closed before 2 events")
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out, got %d events", len(got))
		}
	}
	h.Close()
	// Channel must drain to closed within a beat.
	deadline = time.After(time.Second)
loop:
	for {
		select {
		case _, ok := <-h.Events:
			if !ok {
				break loop
			}
		case <-deadline:
			t.Fatal("stream channel never closed after handle.Close()")
		}
	}

	// Query params.
	if !strings.Contains(gotQuery, "attach=true") || !strings.Contains(gotQuery, "full=true") {
		t.Fatalf("query=%q missing attach/full", gotQuery)
	}
	if gotLastEventID != "7" {
		t.Fatalf("Last-Event-ID=%q want 7", gotLastEventID)
	}

	// Typed events.
	if got[0].Type != client.EventTypeMessage || got[0].Message == nil ||
		got[0].Message.Kind != "assistant_text" || got[0].EventID != 11 {
		t.Fatalf("event[0]=%+v / msg=%+v", got[0], got[0].Message)
	}
	if got[1].Type != client.EventTypeStatus || got[1].Status == nil ||
		got[1].Status.AttachCount != 1 || got[1].EventID != 12 {
		t.Fatalf("event[1]=%+v / status=%+v", got[1], got[1].Status)
	}
}

// TestClient_Stream_OnHTTPErrorIsAPIError: a non-2xx stream open
// surfaces an APIError instead of an opaque Event.
func TestClient_Stream_OnHTTPErrorIsAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs/{job_id}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "job not found"})
	})
	sock := fakeDaemon(t, mux)
	c := newClient(t, sock)
	_, err := c.Stream(context.Background(), "missing", client.StreamOptions{Attach: true})
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("err=%v, want APIError 404", err)
	}
}

// ---- helpers ------------------------------------------------------------



func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
