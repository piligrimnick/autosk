package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// ---- attach-aware fixture -----------------------------------------------

// attachFixture mirrors srvFixture but also wires Runners + Attachments
// into the server.Deps so the attach plumbing is exercised.
type attachFixture struct {
	sockPath    string
	mgr         *projectmgr.Manager
	sched       *scheduler.Scheduler
	runners     *pirunners.Registry
	attachments *pirunners.Attachments
	httpSrv     *http.Server
	httpCli     *http.Client
	wg          sync.WaitGroup
	closeFns    []func()
}

func newAttachFixture(t *testing.T) *attachFixture {
	t.Helper()
	sockDir := tempUDSDir(t)
	sockPath := filepath.Join(sockDir, "d.sock")

	regDir := t.TempDir()
	reg, err := pkgregistry.Open(filepath.Join(regDir, "packages"), pkgregistry.WithNpm(fakeNpm{}))
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

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	fx := &attachFixture{
		sockPath:    sockPath,
		mgr:         mgr,
		sched:       sched,
		runners:     runners,
		attachments: attachments,
		httpSrv:     httpSrv,
		httpCli: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 5 * time.Second,
		},
	}
	fx.wg.Add(1)
	go func() {
		defer fx.wg.Done()
		_ = httpSrv.Serve(ln)
	}()
	fx.closeFns = []func(){
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(ctx)
		},
		func() { fx.wg.Wait() },
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
		func() { _ = os.Remove(sockPath) },
	}
	t.Cleanup(fx.shutdown)
	return fx
}

func (fx *attachFixture) shutdown() {
	for _, fn := range fx.closeFns {
		fn()
	}
	fx.closeFns = nil
}

// seedRunningRun creates a project with one running run and returns
// (projectRoot, jobID). The run row has a real workflow+step backing
// it so FK constraints pass.
func (fx *attachFixture) seedRunningRun(t *testing.T) (string, string) {
	t.Helper()
	root := initProject(t)
	dbPath := filepath.Join(root, ".autosk", "db")
	ts := doltlite.New()
	if err := ts.Open(context.Background(), dbPath); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ts.Close()
	if err := ts.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	tk, err := ts.CreateTask(ctx, store.Task{Title: "demo", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// Seed a minimal workflow + step so daemon_runs FK passes.
	db := ts.DB()
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, created_at)
		 VALUES ('wf-attach', 'attach-wf', '', 'st-attach', 0, ?)`, now); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	var humanID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agents WHERE name='human'`).Scan(&humanID); err != nil {
		t.Fatalf("select human agent: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO steps(id, workflow_id, name, agent_id, seq)
		 VALUES ('st-attach', 'wf-attach', 'do', ?, 0)`, humanID); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	rs := runstore.New(ts.DB())
	r, err := rs.CreateRun(ctx, runstore.NewRun{TaskID: tk.ID, StepID: "st-attach"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The run is left in status=queued. We don't mark it as running
	// because the daemon's restart-recovery sweep that fires on the
	// first project open would rewrite a "running" row to failed
	// (no live pi process backs it). The handlers only inspect for
	// IsTerminal, which queued is not, so the test path is identical.
	return root, r.JobID
}

// post sends an HTTP POST against the daemon socket and returns the
// raw response.
func (fx *attachFixture) post(t *testing.T, path, cwd string, body any) *http.Response {
	t.Helper()
	var br io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		br = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://autosk"+path, br)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if cwd != "" {
		req.Header.Set("X-Autosk-Cwd", cwd)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := fx.httpCli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// ---- recordingHandle: in-memory RunnerHandle used by handler tests -----

type recordingHandle struct {
	mu        sync.Mutex
	commands  []pi.Command
	aborts    int
	streaming atomic.Bool
	respOK    bool
	respErr   string
}

func newRecordingHandle() *recordingHandle {
	return &recordingHandle{respOK: true}
}

func (h *recordingHandle) SendCommand(c pi.Command) (<-chan pi.Response, error) {
	h.mu.Lock()
	h.commands = append(h.commands, c)
	h.mu.Unlock()
	ch := make(chan pi.Response, 1)
	ch <- pi.Response{ID: c.ID, Command: c.Type, Success: h.respOK, Error: h.respErr}
	close(ch)
	return ch, nil
}

func (h *recordingHandle) Abort(context.Context) error {
	h.mu.Lock()
	h.aborts++
	h.mu.Unlock()
	return nil
}

func (h *recordingHandle) IsStreaming() bool { return h.streaming.Load() }

func (h *recordingHandle) seenCommands() []pi.Command {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]pi.Command, len(h.commands))
	copy(out, h.commands)
	return out
}

// flakyHandle answers success=false for any command kind listed in
// rejectKinds and success=true for everything else. The error string
// returned for a rejected command is pulled from rejectErrors[kind]
// (default: a state-mismatch token, so the dispatchInput retry fires).
// Used to drive both the TOCTOU retry path AND the non-state error
// path in handleInput.
type flakyHandle struct {
	mu           sync.Mutex
	commands     []pi.Command
	streaming    bool
	rejectKinds  map[string]bool
	rejectErrors map[string]string
}

func (h *flakyHandle) SendCommand(c pi.Command) (<-chan pi.Response, error) {
	h.mu.Lock()
	h.commands = append(h.commands, c)
	reject := h.rejectKinds[c.Type]
	errMsg := h.rejectErrors[c.Type]
	if reject && errMsg == "" {
		// Default to a state-mismatch-looking error so the retry path
		// is exercised. Tests that want the non-retry path set an
		// explicit rejectErrors entry (e.g. "message too long").
		errMsg = "not streaming"
	}
	h.mu.Unlock()
	ch := make(chan pi.Response, 1)
	if reject {
		ch <- pi.Response{ID: c.ID, Command: c.Type, Success: false, Error: errMsg}
	} else {
		ch <- pi.Response{ID: c.ID, Command: c.Type, Success: true}
	}
	close(ch)
	return ch, nil
}

func (h *flakyHandle) Abort(context.Context) error { return nil }

func (h *flakyHandle) IsStreaming() bool { return h.streaming }

func (h *flakyHandle) recorded() []pi.Command {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]pi.Command, len(h.commands))
	copy(out, h.commands)
	return out
}

// ---- tests --------------------------------------------------------------

// TestBuildInputCommand picks the right pi command shape.
func TestBuildInputCommand(t *testing.T) {
	cases := []struct {
		name      string
		req       api.InputRequest
		streaming bool
		wantCmd   string
		wantText  string
	}{
		{"idle_prompt", api.InputRequest{Message: "hi"}, false, "prompt", "hi"},
		{"streaming_default_steer", api.InputRequest{Message: "stop"}, true, "steer", "stop"},
		{"streaming_explicit_steer", api.InputRequest{Message: "stop", StreamingBehavior: "steer"}, true, "steer", "stop"},
		{"streaming_followup", api.InputRequest{Message: "after", StreamingBehavior: "follow_up"}, true, "follow_up", "after"},
		{"idle_ignores_behavior", api.InputRequest{Message: "go", StreamingBehavior: "follow_up"}, false, "prompt", "go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, dispatched := server.BuildInputCommand(tc.req, tc.streaming)
			if cmd.Type != tc.wantCmd {
				t.Fatalf("cmd.Type=%q want %q", cmd.Type, tc.wantCmd)
			}
			if dispatched != tc.wantCmd {
				t.Fatalf("dispatched=%q want %q", dispatched, tc.wantCmd)
			}
			if cmd.Message != tc.wantText {
				t.Fatalf("cmd.Message=%q", cmd.Message)
			}
		})
	}
}

// TestHandleInput_PromptWhenIdle: idle runner → daemon dispatches plain prompt.
func TestHandleInput_PromptWhenIdle(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := newRecordingHandle()
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "hello"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var out api.InputResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Dispatched != "prompt" {
		t.Fatalf("Dispatched=%q want prompt", out.Dispatched)
	}
	cmds := h.seenCommands()
	if len(cmds) != 1 || cmds[0].Type != "prompt" || cmds[0].Message != "hello" {
		t.Fatalf("commands=%+v", cmds)
	}
}

// TestHandleInput_SteerWhenStreaming: streaming runner → steer.
func TestHandleInput_SteerWhenStreaming(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := newRecordingHandle()
	h.streaming.Store(true)
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "stop"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out api.InputResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Dispatched != "steer" {
		t.Fatalf("Dispatched=%q want steer", out.Dispatched)
	}
	cmds := h.seenCommands()
	if cmds[0].Type != "steer" {
		t.Fatalf("first command type=%q", cmds[0].Type)
	}
}

// TestHandleInput_FollowUp explicit follow_up.
func TestHandleInput_FollowUp(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := newRecordingHandle()
	h.streaming.Store(true)
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{
		Message:           "after you finish",
		StreamingBehavior: "follow_up",
	})
	defer resp.Body.Close()
	var out api.InputResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Dispatched != "follow_up" {
		t.Fatalf("Dispatched=%q want follow_up", out.Dispatched)
	}
}

// TestHandleInput_BadBehavior rejects unknown streamingBehavior.
func TestHandleInput_BadBehavior(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	h := newRecordingHandle()
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{
		Message:           "x",
		StreamingBehavior: "yolo",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d (want 400)", resp.StatusCode)
	}
}

// TestHandleInput_EmptyMessage rejects empty body.
func TestHandleInput_EmptyMessage(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	h := newRecordingHandle()
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d (want 400)", resp.StatusCode)
	}
}

// TestHandleInput_NoRunner returns 409 if no runner registered.
func TestHandleInput_NoRunner(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d (want 409)", resp.StatusCode)
	}
}

// TestHandleInput_UnknownJob returns 404.
func TestHandleInput_UnknownJob(t *testing.T) {
	fx := newAttachFixture(t)
	root := initProject(t)
	resp := fx.post(t, "/v1/jobs/job-missing/input", root, api.InputRequest{Message: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d (want 404)", resp.StatusCode)
	}
}

// TestHandleInput_RetryOnStateMismatch: simulates a TOCTOU where pi is
// streaming when handleInput samples IsStreaming() (=> dispatch=steer)
// but rejects the steer (e.g. it flipped to idle between sample and
// SendCommand). The handler must retry once with the opposite shape
// (prompt) and the operator's input lands without them having to retry.
func TestHandleInput_RetryOnStateMismatch(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := &flakyHandle{streaming: true, rejectKinds: map[string]bool{"steer": true}}
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s want 200 (retry should have succeeded)", resp.StatusCode, body)
	}
	var out api.InputResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Dispatched != "prompt" {
		t.Fatalf("Dispatched=%q want prompt (the retry shape)", out.Dispatched)
	}
	got := h.recorded()
	if len(got) != 2 || got[0].Type != "steer" || got[1].Type != "prompt" {
		t.Fatalf("commands=%+v want [steer, prompt]", got)
	}
}

// TestHandleInput_RejectionAfterRetryIs422: when pi rejects both
// dispatch shapes with state-mismatch errors, the daemon returns 422
// (not 409 — reserved for "no runner registered"/"run terminal") with
// both errors in details.
func TestHandleInput_RejectionAfterRetryIs422(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := &flakyHandle{streaming: true, rejectKinds: map[string]bool{"steer": true, "prompt": true}}
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s want 422", resp.StatusCode, body)
	}
	var env struct {
		Error   string         `json:"error"`
		Details map[string]any `json:"details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Details should expose both the first attempt + the retry so the
	// client can debug the race.
	if env.Details["first_attempt"] != "steer" || env.Details["retry_attempt"] != "prompt" {
		t.Fatalf("details missing first/retry: %+v", env.Details)
	}
}

// TestHandleInput_NoRetryForNonStateError: when pi rejects with an
// error that does NOT look like a streaming-state mismatch (message
// too long, content rejected, abort already in flight, ...), the
// daemon must NOT issue a wasted retry with the opposite shape. The
// operator would just see the same error twice with extra latency,
// and the retry shape (prompt sent while pi is idle? steer accepted
// into an empty queue?) could silently misroute the input. Returns
// 422 on the first attempt with retry=false / retry_reason
// in details for client-side debugging.
func TestHandleInput_NoRetryForNonStateError(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := &flakyHandle{
		streaming:    true,
		rejectKinds:  map[string]bool{"steer": true},
		rejectErrors: map[string]string{"steer": "message too long"},
	}
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s want 422 (no retry for non-state error)", resp.StatusCode, body)
	}
	got := h.recorded()
	if len(got) != 1 || got[0].Type != "steer" {
		t.Fatalf("commands=%+v want exactly [steer] (no retry); double round-trip on non-state error is the regression", got)
	}
	var env struct {
		Error   string         `json:"error"`
		Details map[string]any `json:"details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r, ok := env.Details["retry"].(bool); !ok || r {
		t.Fatalf("details.retry=%v want false", env.Details["retry"])
	}
	if env.Details["pi_error"] != "message too long" {
		t.Fatalf("details.pi_error=%v want %q", env.Details["pi_error"], "message too long")
	}
}

// TestIsStateMismatchError pins the recognised pi error-string tokens.
// Kept here so any future broadening of the retry policy is an
// explicit code change visible in this table.
func TestIsStateMismatchError(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"message too long", false},
		{"abort already in flight", false},
		{"agent rejected the content", false},
		{"not streaming", true},
		{"Not Streaming", true},
		{"already streaming", true},
		{"runner is idle", true},
		{"no active run", true},
		{"no_active_run", true},
		{"state_mismatch: not streaming", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := server.IsStateMismatchError(tc.in); got != tc.want {
				t.Fatalf("IsStateMismatchError(%q) = %v want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestReplayStartIndex pins the SSE replay-start-index branching so
// the off-by-one fix that powers the attach-extension's
// /messages?full=true + Last-Event-ID:N handoff stays correct. Without
// this test, a refactor that re-introduces the bug would only surface
// as "operator sees the full transcript twice on attach" — a
// user-visible regression with no test coverage.
func TestReplayStartIndex(t *testing.T) {
	cases := []struct {
		name  string
		skip  int
		full  bool
		limit int
		total int
		want  int
	}{
		{"empty_no_skip", 0, false, 20, 0, 0},
		{"full_no_skip", 0, true, 20, 50, 0},
		{"limit_tails", 0, false, 5, 12, 7},
		{"limit_larger_than_total", 0, false, 50, 10, 0},
		{"limit_equal_to_total", 0, false, 10, 10, 0},
		{"skip_partial", 5, false, 20, 12, 5},
		{"skip_partial_with_full", 5, true, 20, 12, 5},
		{"skip_caught_up", 40, false, 20, 40, 40}, // the off-by-one case
		{"skip_caught_up_with_full", 40, true, 20, 40, 40},
		{"skip_beyond_total", 50, false, 20, 40, 40},
		{"skip_beyond_total_with_full", 50, true, 20, 40, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := server.ReplayStartIndex(tc.skip, tc.full, tc.limit, tc.total); got != tc.want {
				t.Fatalf("replayStartIndex(skip=%d, full=%v, limit=%d, total=%d) = %d want %d",
					tc.skip, tc.full, tc.limit, tc.total, got, tc.want)
			}
		})
	}
}

// TestHandleInput_TerminalRunIs409.
func TestHandleInput_TerminalRunIs409(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	// Flip the run to done directly.
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
	resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{Message: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d (want 409)", resp.StatusCode)
	}
}

// TestHandleInput_ConcurrentWriters: two parallel POSTs both succeed and
// are recorded by the runner in some order.
func TestHandleInput_ConcurrentWriters(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	h := newRecordingHandle()
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	const N = 5
	var wg sync.WaitGroup
	statuses := make([]int, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := fx.post(t, "/v1/jobs/"+jobID+"/input", root, api.InputRequest{
				Message: "msg-" + string(rune('A'+i)),
			})
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("post %d status=%d", i, s)
		}
	}
	if got := len(h.seenCommands()); got != N {
		t.Fatalf("commands=%d want %d", got, N)
	}
}

// TestHandleAbort_Success.
func TestHandleAbort_Success(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	h := newRecordingHandle()
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	resp := fx.post(t, "/v1/jobs/"+jobID+"/abort", root, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if h.aborts != 1 {
		t.Fatalf("aborts=%d want 1", h.aborts)
	}
}

// TestHandleAbort_UnknownJob.
func TestHandleAbort_UnknownJob(t *testing.T) {
	fx := newAttachFixture(t)
	root := initProject(t)
	resp := fx.post(t, "/v1/jobs/job-x/abort", root, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d (want 404)", resp.StatusCode)
	}
}

// TestHandleAbort_NoRunner.
func TestHandleAbort_NoRunner(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)
	resp := fx.post(t, "/v1/jobs/"+jobID+"/abort", root, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d (want 409)", resp.StatusCode)
	}
}

// TestSSE_AttachAcquiresOnConnect verifies that opening an SSE stream
// with ?attach=true bumps the attach counter, and closing the
// connection (client cancels its context) decrements it.
func TestSSE_AttachAcquiresOnConnect(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	if got := fx.attachments.Count(jobID); got != 0 {
		t.Fatalf("baseline count=%d", got)
	}

	streamCtx, streamCancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, "http://autosk/v1/jobs/"+jobID+"/stream?attach=true", nil)
	req.Header.Set("X-Autosk-Cwd", root)

	// Tail the stream from a goroutine; we only need it open.
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := fx.httpCli.Do(req)
		if err != nil {
			t.Logf("stream Do err: %v", err)
			respCh <- nil
			return
		}
		respCh <- resp
	}()

	// Wait up to 2s for the attach counter to flip to 1.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fx.attachments.Count(jobID) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fx.attachments.Count(jobID); got != 1 {
		t.Fatalf("attach count never reached 1, got %d", got)
	}

	streamCancel()
	resp := <-respCh
	if resp != nil {
		resp.Body.Close()
	}
	// Wait for the deferred release to fire.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fx.attachments.Count(jobID) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fx.attachments.Count(jobID); got != 0 {
		t.Fatalf("attach count never returned to 0, got %d", got)
	}
}

// TestSSE_AttachOnUnknownJobDoesNotBumpCounter: an unknown / typo'd
// jobID must 404 *without* briefly polluting the attach-counter map
// (counts[jobID] going 0→1→0). Regression for the review note on
// sse.go acquiring before validating the run.
func TestSSE_AttachOnUnknownJobDoesNotBumpCounter(t *testing.T) {
	fx := newAttachFixture(t)
	root := initProject(t)

	req := httptest.NewRequest(http.MethodGet, "http://autosk/v1/jobs/job-missing/stream?attach=true", nil)
	req.Header.Set("X-Autosk-Cwd", root)

	rw := httptest.NewRecorder()
	fx.httpSrv.Handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status=%d (want 404)", rw.Code)
	}
	if got := fx.attachments.Count("job-missing"); got != 0 {
		t.Fatalf("attach counter for unknown job=%d (want 0; counter was acquired before run validation)", got)
	}
}

// TestSSE_AttachCountSurfacedInStatusEvents: a first client opens
// a stream with ?attach=true and reads SSE events; a second client
// then opens its own attach stream. The first client must see a
// fresh `status` SSE event whose attach_count == 2, and a follow-up
// event with attach_count == 1 after the second client disconnects.
// This pins the contract that the TUI status bar relies on.
func TestSSE_AttachCountSurfacedInStatusEvents(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	// First client.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	reqA, _ := http.NewRequestWithContext(ctxA, http.MethodGet,
		"http://autosk/v1/jobs/"+jobID+"/stream?attach=true", nil)
	reqA.Header.Set("X-Autosk-Cwd", root)
	respA, err := fx.httpCli.Do(reqA)
	if err != nil {
		t.Fatalf("clientA Do: %v", err)
	}
	defer respA.Body.Close()

	// Read the initial status event from client A. attach_count must be 1.
	eventsA := make(chan parsedSSE, 8)
	go readSSE(t, respA.Body, eventsA)
	waitStatusWithAttach(t, eventsA, 1)

	// Second client attaches.
	ctxB, cancelB := context.WithCancel(context.Background())
	reqB, _ := http.NewRequestWithContext(ctxB, http.MethodGet,
		"http://autosk/v1/jobs/"+jobID+"/stream?attach=true", nil)
	reqB.Header.Set("X-Autosk-Cwd", root)
	respB, err := fx.httpCli.Do(reqB)
	if err != nil {
		t.Fatalf("clientB Do: %v", err)
	}
	defer respB.Body.Close()

	// Client A should see attach_count flip to 2.
	waitStatusWithAttach(t, eventsA, 2)

	// Detach client B.
	cancelB()
	_ = respB.Body.Close()

	// Client A should see attach_count flip back to 1.
	waitStatusWithAttach(t, eventsA, 1)
}

// parsedSSE is a minimal parsed SSE frame used by the tests below.
// HasID records whether the raw frame carried an `id:` line at all
// (the writer omits id: for status/done frames so a client doing
// `if ev.id <= last` cannot drop them).
type parsedSSE struct {
	Event string
	Data  string
	ID    string
	HasID bool
}

// readSSE parses SSE frames off body and forwards them on out. It
// shuts down when the body closes (test cancels the context).
func readSSE(t *testing.T, body io.Reader, out chan<- parsedSSE) {
	t.Helper()
	defer close(out)
	sc := bufio.NewScanner(body)
	// SSE frames replay full transcripts (the entire MessageEvent goes
	// into a single `data:` line) and pi can legitimately emit JSON
	// objects >>1 MiB on long sessions. Keep this in sync with the
	// production client (internal/daemon/client/stream.go).
	sc.Buffer(make([]byte, 0, 1<<14), 1<<22)
	var cur parsedSSE
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Event != "" || cur.Data != "" || cur.HasID {
				out <- cur
			}
			cur = parsedSSE{}
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
			cur.HasID = true
		}
	}
}

// waitStatusWithAttach reads SSE events from ch and returns once it
// observes an `event: status` whose `attach_count` field equals want.
// Times out after 3s to keep the test bounded.
func waitStatusWithAttach(t *testing.T, ch <-chan parsedSSE, want int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("SSE stream closed before status with attach_count=%d", want)
			}
			if ev.Event != "status" {
				continue
			}
			var js struct {
				AttachCount int `json:"attach_count"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &js); err != nil {
				t.Fatalf("unmarshal status data %q: %v", ev.Data, err)
			}
			if js.AttachCount == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for status with attach_count=%d", want)
		}
	}
}

// TestSSE_StatusFrameCarriesStreamingFromRunner pins the contract
// that the daemon samples *pi.Runner.IsStreaming() and surfaces it
// on every status frame. A client that flips its TUI "streaming"
// indicator off only when api.JobResponse.Streaming is false (rather
// than via heuristic event-kind sniffing) relies on this.
func TestSSE_StatusFrameCarriesStreamingFromRunner(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	h := newRecordingHandle()
	h.streaming.Store(true)
	fx.runners.Register(jobID, h)
	defer fx.runners.Unregister(jobID)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	reqA, _ := http.NewRequestWithContext(ctxA, http.MethodGet,
		"http://autosk/v1/jobs/"+jobID+"/stream?attach=true", nil)
	reqA.Header.Set("X-Autosk-Cwd", root)
	respA, err := fx.httpCli.Do(reqA)
	if err != nil {
		t.Fatalf("client Do: %v", err)
	}
	defer respA.Body.Close()

	events := make(chan parsedSSE, 16)
	go readSSE(t, respA.Body, events)

	// First status frame: streaming=true.
	waitStatusWithStreaming(t, events, true)

	// Flip the runner to idle (simulates pi emitting agent_end
	// outside of the test's event loop). The daemon's tail loop
	// must observe the IsStreaming() flip and emit a fresh status
	// frame with streaming=false. Without the prevStreaming guard
	// in sse.go the status frame would never re-emit and the
	// client's indicator would stay on "streaming" until the run
	// went terminal.
	h.streaming.Store(false)
	waitStatusWithStreaming(t, events, false)
}

// waitStatusWithStreaming reads SSE events from ch and returns once
// it observes an `event: status` whose `streaming` field equals want.
func waitStatusWithStreaming(t *testing.T, ch <-chan parsedSSE, want bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("SSE stream closed before status with streaming=%v", want)
			}
			if ev.Event != "status" {
				continue
			}
			var js struct {
				Streaming bool `json:"streaming"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &js); err != nil {
				t.Fatalf("unmarshal status data %q: %v", ev.Data, err)
			}
			if js.Streaming == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for status with streaming=%v", want)
		}
	}
}

// TestSSE_StatusFrameOmitsEventID pins the SSE wire contract that
// status (and done) frames do NOT carry an `id:` line. Message
// frames keep their monotonic id (the Last-Event-ID resume cursor),
// but status events share that namespace at id=0 and the writer
// suppresses the id line entirely so a client doing `if (ev.id <=
// last) skip` cannot drop a status frame because of an id
// collision with the last replayed message. See review on
// sse.go:135 / sse.go:195.
func TestSSE_StatusFrameOmitsEventID(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://autosk/v1/jobs/"+jobID+"/stream?attach=true", nil)
	req.Header.Set("X-Autosk-Cwd", root)
	resp, err := fx.httpCli.Do(req)
	if err != nil {
		t.Fatalf("client Do: %v", err)
	}
	defer resp.Body.Close()

	events := make(chan parsedSSE, 16)
	go readSSE(t, resp.Body, events)

	// Drain frames until we have seen at least one status frame,
	// asserting along the way that every status/done frame omits
	// the `id:` line and that message frames do carry an id.
	deadline := time.After(3 * time.Second)
	sawStatus := false
	sawMessage := false
drain:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break drain
			}
			switch ev.Event {
			case "status", "done":
				if ev.HasID {
					t.Fatalf("%s frame carries an id line: id=%q data=%q",
						ev.Event, ev.ID, ev.Data)
				}
				if ev.Event == "status" {
					sawStatus = true
				}
			case "message":
				if !ev.HasID {
					t.Fatalf("message frame missing id line: data=%q", ev.Data)
				}
				sawMessage = true
			}
			if sawStatus {
				break drain
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a status frame (sawMessage=%v)", sawMessage)
		}
	}
	if !sawStatus {
		t.Fatalf("stream closed without observing a status frame")
	}
}

// TestSSE_NoAttachQueryDoesNotAcquire: same SSE handler without
// ?attach=true must not touch the counter.
func TestSSE_NoAttachQueryDoesNotAcquire(t *testing.T) {
	fx := newAttachFixture(t)
	root, jobID := fx.seedRunningRun(t)

	// Use httptest to invoke handler directly without going through UDS,
	// so we can check the counter while the request is in progress.
	req := httptest.NewRequest(http.MethodGet, "http://autosk/v1/jobs/"+jobID+"/stream", nil)
	req.Header.Set("X-Autosk-Cwd", root)

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	req = req.WithContext(streamCtx)

	rw := httptest.NewRecorder()
	go fx.httpSrv.Handler.ServeHTTP(rw, req)
	time.Sleep(100 * time.Millisecond)
	if got := fx.attachments.Count(jobID); got != 0 {
		t.Fatalf("non-attach stream bumped counter to %d", got)
	}
}
