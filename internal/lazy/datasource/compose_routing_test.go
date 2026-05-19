package datasource_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store/doltlite"
)

// fakeDaemon serves a minimal subset of the daemon API over a UDS so
// the compose layer's routing (and its 2s health probe) can be
// exercised without spinning up the full daemon stack.
type fakeDaemon struct {
	sock string

	ok      atomic.Bool
	workers atomic.Int32
	jobsHit atomic.Uint64

	srv *http.Server
	ln  net.Listener
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	sock := filepath.Join(shortTmp(t), "fake.sock")
	fd := &fakeDaemon{sock: sock}
	fd.ok.Store(true)
	fd.workers.Store(3)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HealthResponse{
			OK:      fd.ok.Load(),
			Workers: int(fd.workers.Load()),
		})
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		fd.jobsHit.Add(1)
		_ = json.NewEncoder(w).Encode(api.ListResponse{
			Jobs: []api.JobResponse{
				{JobID: "job-1", TaskID: "as-aaaa", Status: "running", Streaming: true, AttachCount: 2},
			},
		})
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		_ = json.NewEncoder(w).Encode(api.JobResponse{
			JobID: id, TaskID: "as-aaaa", Status: "running", Streaming: true, AttachCount: 1,
		})
	})

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	fd.srv = srv
	fd.ln = ln
	t.Cleanup(func() {
		_ = srv.Close()
	})
	return fd
}

// shortTmp returns a tmp dir short enough for a unix-socket name on
// macOS (sun_path = 104). Same trick as cmd/autosk's e2e fixture.
func shortTmp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if len(d) < 80 {
		return d
	}
	return "/tmp"
}

func mkOfflineFor(t *testing.T) *datasource.Offline {
	t.Helper()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(context.Background(), filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	if err := ts.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	off, err := datasource.NewOffline(ts, dir, nil)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	return off
}

// TestCompose_RoutesJobsViaLiveWhenDaemonOK proves that when the
// fake daemon is up, c.Jobs() hits the live HTTP endpoint (so
// Streaming / AttachCount on JobResponse carry the daemon's values)
// instead of the DB.
func TestCompose_RoutesJobsViaLiveWhenDaemonOK(t *testing.T) {
	fd := newFakeDaemon(t)
	off := mkOfflineFor(t)
	cli, err := client.New(client.Options{Sock: fd.sock})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	c := datasource.NewCompose(off, cli, 30*time.Millisecond)
	defer c.Close()

	// Compose's initial probe runs synchronously in NewCompose, so by
	// the time we get here the health should already be ok.
	if h := c.Health(); h.Daemon != "ok" {
		t.Fatalf("daemon=%q want ok (workers=%d)", h.Daemon, h.Workers)
	}
	jobs, err := c.Jobs(context.Background(), datasource.JobFilter{})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].JobID != "job-1" || !jobs[0].Streaming || jobs[0].AttachCount != 2 {
		t.Fatalf("expected live-decorated job, got %+v", jobs)
	}
	if fd.jobsHit.Load() == 0 {
		t.Fatalf("fake daemon /v1/jobs never hit; compose routed offline")
	}
}

// TestCompose_FlipsLiveOffline simulates a daemon going down between
// two probe ticks. After the flip, Jobs must come from the offline
// source (no DB rows so empty list, but no error).
func TestCompose_FlipsLiveOffline(t *testing.T) {
	fd := newFakeDaemon(t)
	off := mkOfflineFor(t)
	cli, err := client.New(client.Options{Sock: fd.sock})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	c := datasource.NewCompose(off, cli, 20*time.Millisecond)
	defer c.Close()
	if h := c.Health(); h.Daemon != "ok" {
		t.Fatalf("expected initial ok, got %q", h.Daemon)
	}
	// Take the daemon offline by stopping the server. Close() also
	// closes the listener so future dials get ECONNREFUSED.
	_ = fd.srv.Close()
	deadline := time.Now().Add(1 * time.Second)
	for {
		if c.Health().Daemon == "down" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never flipped to down (still %q)", c.Health().Daemon)
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs, err := c.Jobs(context.Background(), datasource.JobFilter{})
	if err != nil {
		t.Fatalf("Jobs after flip: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("offline returned %d jobs against an empty DB", len(jobs))
	}
}

// TestCompose_BoundedInitialProbe pins the 500ms bound on the initial
// health probe: a socket whose listener accepts the dial but never
// answers must not wedge NewCompose. (See review comment "Compose's
// initial probe is unbounded".)
func TestCompose_BoundedInitialProbe(t *testing.T) {
	sock := filepath.Join(shortTmp(t), "hung.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Accept connections but never read/write; the dial succeeds, the
	// HTTP read hangs.
	doneAcc := make(chan struct{})
	go func() {
		defer close(doneAcc)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // hold the connection open; never read
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-doneAcc })

	off := mkOfflineFor(t)
	cli, err := client.New(client.Options{Sock: sock})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	start := time.Now()
	c := datasource.NewCompose(off, cli, 2*time.Second)
	defer c.Close()
	took := time.Since(start)
	if took > 1500*time.Millisecond {
		t.Fatalf("NewCompose took %s with hung daemon; initial probe should bound at ~500ms", took)
	}
	if h := c.Health(); h.Daemon == "ok" {
		t.Fatalf("expected daemon!=ok against hung listener, got %q", h.Daemon)
	}
}

// silence unused
var _ = fmt.Sprintf
