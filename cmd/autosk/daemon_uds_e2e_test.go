package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/projectmgr"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/server"
	"autosk/internal/daemon/uds"
	"autosk/internal/store/doltlite"
)

// e2eFakeNpmUDS is the no-op npm runner for the UDS e2e test.
type e2eFakeNpmUDS struct{}

func (e2eFakeNpmUDS) Install(_ context.Context, _, _ string) error   { return nil }
func (e2eFakeNpmUDS) Uninstall(_ context.Context, _, _ string) error { return nil }

// shortTmp returns a directory short enough to host a unix socket
// (macOS sun_path = 104). Falls back to /tmp when t.TempDir() is too
// long.
func shortTmp(t *testing.T, suffix string) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir)+len("/"+suffix) < 100 {
		return dir
	}
	short, err := os.MkdirTemp("/tmp", "uds-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	return short
}

// initFixtureProject creates a tmp dir with a migrated .autosk/db.
func initFixtureProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".autosk"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(root, ".autosk", "db")
	ts := doltlite.New()
	if err := ts.Open(context.Background(), dbPath); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(context.Background()); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return root
}

// e2eDaemon brings up a real http.Server on a unix socket with all the
// daemon wiring (projectmgr + scheduler + server). It mirrors what
// `autosk daemon serve` does but lets the test poke at the live state.
type e2eDaemon struct {
	sockPath string
	mgr      *projectmgr.Manager
	sched    *scheduler.Scheduler
	httpSrv  *http.Server
	cli      *http.Client
	wg       sync.WaitGroup
	stopFns  []func()
}

func startE2EDaemon(t *testing.T) *e2eDaemon {
	t.Helper()
	sockDir := shortTmp(t, "daemon.sock")
	sockPath := filepath.Join(sockDir, "daemon.sock")

	regDir := t.TempDir()
	reg, err := pkgregistry.Open(filepath.Join(regDir, "packages"), pkgregistry.WithNpm(e2eFakeNpmUDS{}))
	if err != nil {
		t.Fatalf("pkgregistry: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}

	var mgr *projectmgr.Manager
	sched := scheduler.New(scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		proj, ok := mgr.Get(projectmgr.Key(job.Project))
		if !ok {
			t.Errorf("scheduler closure: project not loaded: %s", job.Project)
			return nil
		}
		return proj.Executor.Run(ctx, job.ID)
	}), scheduler.Config{Workers: 1})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("sched start: %v", err)
	}
	mgr = projectmgr.New(projectmgr.Deps{
		Sched:        sched,
		Packages:     reg,
		PollInterval: time.Hour, // disable scans for this test
		ExecCfg: executor.Config{
			Grace:       100 * time.Millisecond,
			IdleTimeout: 5 * time.Second,
		},
	})
	srv := server.New(server.Deps{Projects: mgr, Sched: sched, Workers: 1})

	ln, err := uds.Listen(sockPath)
	if err != nil {
		t.Fatalf("uds.Listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	d := &e2eDaemon{
		sockPath: sockPath,
		mgr:      mgr,
		sched:    sched,
		httpSrv:  httpSrv,
		cli: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 5 * time.Second,
		},
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		_ = httpSrv.Serve(ln)
	}()
	d.stopFns = []func(){
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
		},
		func() { d.wg.Wait() },
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = mgr.CloseAll(ctx)
		},
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = sched.Stop(ctx)
		},
		func() { _ = uds.Cleanup(sockPath) },
	}
	t.Cleanup(d.shutdown)
	return d
}

func (d *e2eDaemon) shutdown() {
	for _, fn := range d.stopFns {
		fn()
	}
	d.stopFns = nil
}

func (d *e2eDaemon) get(t *testing.T, path, cwd string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://autosk"+path, nil)
	if cwd != "" {
		req.Header.Set("X-Autosk-Cwd", cwd)
	}
	resp, err := d.cli.Do(req)
	if err != nil {
		t.Fatalf("Do %s: %v", path, err)
	}
	return resp
}

// ---- tests ---------------------------------------------------------------

// TestUDS_E2E_TwoProjectsIsolated verifies that two projects served from
// the same daemon do not see each other's jobs (list scopes by project)
// and that --all-projects sees both.
func TestUDS_E2E_TwoProjectsIsolated(t *testing.T) {
	d := startE2EDaemon(t)
	rootA := initFixtureProject(t)
	rootB := initFixtureProject(t)

	// List from A — empty.
	respA := d.get(t, "/v1/jobs", rootA)
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("list A: %d", respA.StatusCode)
	}
	var listA api.ListResponse
	_ = json.NewDecoder(respA.Body).Decode(&listA)
	respA.Body.Close()
	if len(listA.Jobs) != 0 {
		t.Fatalf("expected 0 jobs for A, got %d", len(listA.Jobs))
	}

	// List from B — empty.
	respB := d.get(t, "/v1/jobs", rootB)
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("list B: %d", respB.StatusCode)
	}
	respB.Body.Close()

	// Aggregated health sees both loaded projects.
	respAll := d.get(t, "/v1/healthz?all=true", "")
	if respAll.StatusCode != http.StatusOK {
		t.Fatalf("health?all: %d", respAll.StatusCode)
	}
	var h api.HealthResponse
	if err := json.NewDecoder(respAll.Body).Decode(&h); err != nil {
		respAll.Body.Close()
		t.Fatalf("decode: %v", err)
	}
	respAll.Body.Close()
	roots := map[string]bool{}
	for _, p := range h.Projects {
		roots[p.Root] = true
	}
	if len(h.Projects) < 2 {
		t.Fatalf("expected ≥2 loaded projects, got %d", len(h.Projects))
	}
	canonA, _ := filepath.EvalSymlinks(rootA)
	canonB, _ := filepath.EvalSymlinks(rootB)
	if !roots[canonA] {
		t.Errorf("aggregated health missing project A (%s)", canonA)
	}
	if !roots[canonB] {
		t.Errorf("aggregated health missing project B (%s)", canonB)
	}
}

func TestUDS_E2E_MissingCwdIs400(t *testing.T) {
	d := startE2EDaemon(t)
	resp := d.get(t, "/v1/jobs", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d (%s)", resp.StatusCode, body)
	}
}

func TestUDS_E2E_UnknownProjectIs404(t *testing.T) {
	d := startE2EDaemon(t)
	missing := t.TempDir() // no .autosk
	resp := d.get(t, "/v1/jobs", missing)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d (%s)", resp.StatusCode, body)
	}
}

func TestUDS_E2E_PostJobsIsRemoved(t *testing.T) {
	d := startE2EDaemon(t)
	root := initFixtureProject(t)
	req, _ := http.NewRequest(http.MethodPost, "http://autosk/v1/jobs", strings.NewReader(`{}`))
	req.Header.Set("X-Autosk-Cwd", root)
	resp, err := d.cli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404/405 for POST /v1/jobs, got %d (%s)", resp.StatusCode, body)
	}
}

// TestUDS_E2E_SingleInstance: starting a second daemon on the same
// socket while the first is alive must fail with ErrAlreadyRunning;
// after the first goes away the socket can be reused.
func TestUDS_E2E_SingleInstance(t *testing.T) {
	sockDir := shortTmp(t, "daemon.sock")
	sockPath := filepath.Join(sockDir, "daemon.sock")

	ln1, err := uds.Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen #1: %v", err)
	}
	// Spin a tiny accept loop so the live-peer probe succeeds.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln1.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, err = uds.Listen(sockPath)
	if err == nil {
		t.Fatal("expected second Listen to fail (daemon already running)")
	}

	// Shut down the first, then verify the socket can be reused.
	_ = ln1.Close()
	<-done
	if err := uds.Cleanup(sockPath); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	ln3, err := uds.Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen post-cleanup: %v", err)
	}
	defer ln3.Close()
	defer uds.Cleanup(sockPath)
}

// TestUDS_E2E_XAutoskDBOverridesWalkUp: pointing X-Autosk-DB at an
// arbitrary tmp .autosk/db wins over walk-up resolution from the cwd.
func TestUDS_E2E_XAutoskDBOverridesWalkUp(t *testing.T) {
	d := startE2EDaemon(t)
	rootA := initFixtureProject(t)
	rootB := initFixtureProject(t)
	// Use cwd=rootA but db=rootB/.autosk/db: the resolved project must
	// be B.
	dbB := filepath.Join(rootB, ".autosk", "db")
	req, _ := http.NewRequest(http.MethodGet, "http://autosk/v1/healthz", nil)
	req.Header.Set("X-Autosk-Cwd", rootA)
	req.Header.Set("X-Autosk-DB", dbB)
	resp, err := d.cli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var h api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	canonB, _ := filepath.EvalSymlinks(rootB)
	if h.ProjectRoot != canonB {
		t.Fatalf("expected ProjectRoot=%s (from X-Autosk-DB override), got %s",
			canonB, h.ProjectRoot)
	}
	if h.DBPath != dbB {
		t.Fatalf("expected DBPath=%s, got %s", dbB, h.DBPath)
	}
}

// TestUDS_E2E_DaemonIgnoresAutoskDBEnv: the daemon process must not
// consult its own AUTOSK_DB. This test sets the env in-process before
// starting the daemon and verifies that requesting a project still
// resolves through the per-request headers.
func TestUDS_E2E_DaemonIgnoresAutoskDBEnv(t *testing.T) {
	// projectmgr always uses the headers, not the daemon's env. We
	// simulate by setting AUTOSK_DB to something bogus and asserting
	// the request still resolves to the cwd-derived project.
	t.Setenv("AUTOSK_DB", "/no/such/path/db")
	d := startE2EDaemon(t)
	root := initFixtureProject(t)
	resp := d.get(t, "/v1/healthz", root)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var h api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	canon, _ := filepath.EvalSymlinks(root)
	if h.ProjectRoot != canon {
		t.Fatalf("expected ProjectRoot=%s, got %s (daemon's AUTOSK_DB leaked through?)",
			canon, h.ProjectRoot)
	}
}
