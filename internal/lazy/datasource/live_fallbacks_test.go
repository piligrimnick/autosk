package datasource_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
	"autosk/internal/lazy/datasource"
)

// TestLive_FallbacksCounterIncrementsOnError pins the counter that
// feeds the lazy TUI's status-bar 'flaky' chip. Every daemon-preferred
// read that errors must increment l.Fallbacks() so renderStatusBar
// can render a delta-based indicator between refresh ticks.
//
// Previously the counter was wired but had no caller; this test
// guards against a future regression that drops the .Add(1) at one
// of the fallback sites.
func TestLive_FallbacksCounterIncrementsOnError(t *testing.T) {
	// Fake daemon that 200s /v1/healthz but 500s /v1/jobs and
	// /v1/jobs/{id} and /v1/messages — so the live reads all fall
	// back to the offline base.
	sock := filepath.Join(shortTmp(t), "flaky.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: true})
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /v1/jobs/{id}/messages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Close() })

	off := mkOfflineFor(t)
	cli, err := client.New(client.Options{Sock: sock})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	live := datasource.NewLive(off, cli)

	if got := live.Fallbacks(); got != 0 {
		t.Fatalf("initial Fallbacks=%d want 0", got)
	}

	ctx := context.Background()
	// Each call should increment by 1. Note GetJob hits the offline
	// fallback which tries to read the job from the DB — that errors
	// for a non-existent job id but does NOT touch the counter.
	if _, err := live.Jobs(ctx, datasource.JobFilter{}); err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if got := live.Fallbacks(); got != 1 {
		t.Fatalf("Fallbacks after Jobs=%d want 1", got)
	}
	if _, _ = live.GetJob(ctx, "job-xxxx"); true {
	}
	if got := live.Fallbacks(); got != 2 {
		t.Fatalf("Fallbacks after GetJob=%d want 2", got)
	}
	if _, _ = live.Messages(ctx, "job-xxxx", true, 0); true {
	}
	if got := live.Fallbacks(); got != 3 {
		t.Fatalf("Fallbacks after Messages=%d want 3", got)
	}
}

// TestCompose_ExposesFallbacks pins the bridge from Live.Fallbacks
// to Compose.Fallbacks the status bar consumes. (The interface
// assertion in refresh.go is satisfied only if Compose has the
// matching method.)
func TestCompose_ExposesFallbacks(t *testing.T) {
	sock := filepath.Join(shortTmp(t), "compfb.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HealthResponse{OK: true})
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Close() })

	off := mkOfflineFor(t)
	cli, _ := client.New(client.Options{Sock: sock})
	c := datasource.NewCompose(off, cli, 30*time.Millisecond)
	defer c.Close()

	if got := c.Fallbacks(); got != 0 {
		t.Fatalf("initial=%d want 0", got)
	}
	// Drive a fallback through Compose.Jobs (which routes to
	// Live.Jobs when daemon=ok, and Live.Jobs errors → counter++).
	_, _ = c.Jobs(context.Background(), datasource.JobFilter{})
	if got := c.Fallbacks(); got != 1 {
		t.Fatalf("after one Jobs() error: Fallbacks=%d want 1", got)
	}
}
