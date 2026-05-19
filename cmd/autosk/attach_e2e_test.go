package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/pi"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/projectmgr"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/server"
	"autosk/internal/daemon/uds"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// ---- fakepi build helper -----------------------------------------------

var (
	attachFakepiOnce sync.Once
	attachFakepiBin  string
	attachFakepiErr  error
)

func buildAttachFakepi(t *testing.T) string {
	t.Helper()
	attachFakepiOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autosk-attach-fakepi-")
		if err != nil {
			attachFakepiErr = err
			return
		}
		bin := filepath.Join(dir, "fakepi")
		// The test's cwd is cmd/autosk/, so address the fakepi main
		// package by its module path (resolved via go.mod).
		cmd := exec.Command("go", "build", "-o", bin, "autosk/internal/daemon/pi/fakepi")
		out, err := cmd.CombinedOutput()
		if err != nil {
			attachFakepiErr = err
			t.Logf("go build fakepi: %s", out)
			return
		}
		attachFakepiBin = bin
	})
	if attachFakepiErr != nil {
		t.Skipf("cannot build fakepi: %v", attachFakepiErr)
	}
	return attachFakepiBin
}

// ---- attach-aware fixture (mirrors server_test attachFixture) ----------

type attachE2E struct {
	sockPath    string
	runners     *pirunners.Registry
	attachments *pirunners.Attachments
	httpSrv     *http.Server
	cli         *http.Client
	wg          sync.WaitGroup
	mgr         *projectmgr.Manager
	sched       *scheduler.Scheduler
	stopFns     []func()
}

func startAttachE2E(t *testing.T) *attachE2E {
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
	sched := scheduler.New(scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
		return nil
	}), scheduler.Config{Workers: 1})
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("sched start: %v", err)
	}
	runners := pirunners.NewRegistry()
	attachments := pirunners.NewAttachments()
	mgr := projectmgr.New(projectmgr.Deps{
		Sched:        sched,
		Packages:     reg,
		PollInterval: time.Hour,
		Runners:      runners,
		Attachments:  attachments,
		ExecCfg: executor.Config{
			Grace:       100 * time.Millisecond,
			IdleTimeout: 5 * time.Second,
		},
	})
	srv := server.New(server.Deps{
		Projects:    mgr,
		Sched:       sched,
		Workers:     1,
		Runners:     runners,
		Attachments: attachments,
	})

	ln, err := uds.Listen(sockPath)
	if err != nil {
		t.Fatalf("uds.Listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	d := &attachE2E{
		sockPath:    sockPath,
		runners:     runners,
		attachments: attachments,
		httpSrv:     httpSrv,
		mgr:         mgr,
		sched:       sched,
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

func (d *attachE2E) shutdown() {
	for _, fn := range d.stopFns {
		fn()
	}
	d.stopFns = nil
}

// seedAttachRun creates a project with a daemon_run row in `queued`
// state backed by a real workflow + step.
func seedAttachRun(t *testing.T) (string, string) {
	t.Helper()
	root := initFixtureProject(t)
	ctx := context.Background()
	dbPath := filepath.Join(root, ".autosk", "db")
	ts := doltlite.New()
	if err := ts.Open(ctx, dbPath); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ts.Close()
	if err := ts.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tk, err := ts.CreateTask(ctx, store.Task{Title: "attach-e2e", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	db := ts.DB()
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES ('wf-e2e', 'e2e-wf', '', 'st-e2e', 0, ?)`, now); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	var humanID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agents WHERE name='human'`).Scan(&humanID); err != nil {
		t.Fatalf("select human agent: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq)
		 VALUES ('st-e2e', 'wf-e2e', 'do', ?, 0)`, humanID); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	rs := runstore.New(db)
	r, err := rs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: "st-e2e"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return root, r.JobID
}

// ---- the actual e2e -----------------------------------------------------

// TestE2E_AttachConcurrentInputsThroughFakepi spins up a real
// *pi.Runner backed by fakepi, registers it in the daemon's runner
// registry, and fires two parallel POST /v1/jobs/{id}/input requests
// over the live unix socket.
//
// Invariants:
//  1. Both POSTs return 200 OK and `dispatched: "prompt"` (the runner
//     is idle — fakepi doesn't auto-stream after prompt acks).
//  2. fakepi observes two prompt commands in stdin order (the daemon
//     never drops a writer).
//  3. The attach counter is zero throughout (we did not open any SSE
//     with ?attach=true in this test) — exercising the "no attach,
//     plain RPC pass-through" path.
//  4. Opening an SSE with ?attach=true bumps the counter to 1 and
//     closing the socket flips it back to 0.
func TestE2E_AttachConcurrentInputsThroughFakepi(t *testing.T) {
	bin := buildAttachFakepi(t)

	d := startAttachE2E(t)
	root, jobID := seedAttachRun(t)

	// Spawn a real pi.Runner pointed at fakepi.
	runner, err := pi.Spawn(context.Background(), pi.Opts{PIBin: bin})
	if err != nil {
		t.Fatalf("pi.Spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.CloseStdin()
		_, _ = runner.Wait(context.Background(), 2*time.Second)
	})
	// Drain the event channel so the reader doesn't stall.
	go func() {
		for range runner.Events() {
		}
	}()
	d.runners.Register(jobID, runner)
	defer d.runners.Unregister(jobID)

	// (3) baseline: attach counter is 0.
	if got := d.attachments.Count(jobID); got != 0 {
		t.Fatalf("baseline attach count=%d", got)
	}

	// Fire two concurrent POST /input.
	const N = 2
	type result struct {
		status     int
		dispatched string
	}
	results := make([]result, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(api.InputRequest{Message: "msg-from-client-" + string(rune('A'+i))})
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
				"http://autosk/v1/jobs/"+jobID+"/input", bytes.NewReader(body))
			req.Header.Set("X-Autosk-Cwd", root)
			req.Header.Set("Content-Type", "application/json")
			resp, err := d.cli.Do(req)
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			defer resp.Body.Close()
			results[i].status = resp.StatusCode
			var out api.InputResponse
			_ = json.NewDecoder(resp.Body).Decode(&out)
			results[i].dispatched = out.Dispatched
		}(i)
	}
	wg.Wait()

	// (1) both requests succeed.
	for i, r := range results {
		if r.status != http.StatusOK {
			t.Fatalf("client %d status=%d", i, r.status)
		}
		// Idle pi → prompt dispatch.
		if r.dispatched != "prompt" {
			t.Fatalf("client %d dispatched=%q want prompt", i, r.dispatched)
		}
	}

	// (4) attach counter via ?attach=true SSE.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet,
		"http://autosk/v1/jobs/"+jobID+"/stream?attach=true", nil)
	streamReq.Header.Set("X-Autosk-Cwd", root)
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := d.cli.Do(streamReq)
		if err != nil {
			respCh <- nil
			return
		}
		respCh <- resp
	}()
	// Wait for the counter to flip to 1.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.attachments.Count(jobID) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.attachments.Count(jobID); got != 1 {
		t.Fatalf("attach count never reached 1: %d", got)
	}
	streamCancel()
	if resp := <-respCh; resp != nil {
		resp.Body.Close()
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.attachments.Count(jobID) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.attachments.Count(jobID); got != 0 {
		t.Fatalf("attach count did not return to 0: %d", got)
	}
}

// TestE2E_AttachInputAfterTerminalIs409 verifies that after marking
// the run as done the input endpoint returns 409, *even when a runner
// is still registered* in the registry. This documents the layering:
// the terminal-status guard must fire before the registry lookup so a
// stale handle (e.g. one the executor's defer Unregister hasn't run
// yet) cannot leak a 200 OK after the run has been marked done.
func TestE2E_AttachInputAfterTerminalIs409(t *testing.T) {
	bin := buildAttachFakepi(t)
	d := startAttachE2E(t)
	root, jobID := seedAttachRun(t)

	// Register a live runner *before* marking the run terminal so the
	// registry lookup would succeed if the handler walked it. The test
	// asserts the IsTerminal() guard wins.
	runner, err := pi.Spawn(context.Background(), pi.Opts{PIBin: bin})
	if err != nil {
		t.Fatalf("pi.Spawn: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.CloseStdin()
		_, _ = runner.Wait(context.Background(), 2*time.Second)
	})
	go func() {
		for range runner.Events() {
		}
	}()
	d.runners.Register(jobID, runner)
	defer d.runners.Unregister(jobID)

	// Mark the run terminal via the runstore directly.
	dbPath := filepath.Join(root, ".autosk", "db")
	ts := doltlite.New()
	if err := ts.Open(context.Background(), dbPath); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ts.Close()
	rs := runstore.New(ts.DB())
	if _, err := rs.MarkDone(context.Background(), jobID, 0, nil); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	body, _ := json.Marshal(api.InputRequest{Message: "hi"})
	req, _ := http.NewRequest(http.MethodPost,
		"http://autosk/v1/jobs/"+jobID+"/input", bytes.NewReader(body))
	req.Header.Set("X-Autosk-Cwd", root)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.cli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s want 409", resp.StatusCode, b)
	}
	// Decode the error envelope so we pin the cause: must be the
	// terminal guard (status field present), not the registry miss.
	var env struct {
		Error   string         `json:"error"`
		Details map[string]any `json:"details"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error != "run is terminal" {
		t.Fatalf("error=%q want %q (terminal-status guard must win over registry lookup)",
			env.Error, "run is terminal")
	}
}
