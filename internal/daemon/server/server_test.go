package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/server"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// stack is one wired-up daemon for tests.
type stack struct {
	t        *testing.T
	cwd      string
	tasks    *doltlite.Store
	runs     *runstore.Store
	sched    *scheduler.Scheduler
	ts       *httptest.Server
	executed chan string
}

func newStack(t *testing.T, exec scheduler.Executor, token string) *stack {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cwd := filepath.Join(dir, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	taskStore := doltlite.New()
	if err := taskStore.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := taskStore.Migrate(ctx); err != nil {
		_ = taskStore.Close()
		t.Fatalf("Migrate: %v", err)
	}
	runs := runstore.New(taskStore.DB())

	executed := make(chan string, 4)
	if exec == nil {
		// Default test executor: marks running then done synchronously.
		exec = scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
			_, _ = runs.MarkRunning(ctx, jobID, 999)
			_ = runs.SetPISession(ctx, jobID, "sess-test", filepath.Join(cwd, "fake.jsonl"))
			_, _ = runs.MarkDone(ctx, jobID, 0, runstore.ClosureDone)
			executed <- jobID
			return nil
		})
	}
	sched := scheduler.New(runs, exec, scheduler.Config{Workers: 2})
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	srv := server.New(server.Deps{
		Runs: runs, Tasks: taskStore, Sched: sched, Workers: 2,
		Token: token, DefaultCwd: cwd,
	})
	ts := httptest.NewServer(srv.Handler())
	st := &stack{t: t, cwd: cwd, tasks: taskStore, runs: runs, sched: sched, ts: ts, executed: executed}
	t.Cleanup(func() {
		ts.Close()
		gctx, gc := context.WithTimeout(context.Background(), 2*time.Second)
		defer gc()
		_ = sched.Stop(gctx)
		_ = taskStore.Close()
	})
	return st
}

func (s *stack) do(method, path string, body any, headers ...string) (*http.Response, []byte) {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, s.ts.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, b
}

// ---- tests ----------------------------------------------------------------

func TestSubmit_AdHoc_Succeeds(t *testing.T) {
	s := newStack(t, nil, "")
	resp, body := s.do("POST", "/v1/jobs", api.SubmitRequest{Prompt: "say hi"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, body=%s", resp.StatusCode, body)
	}
	var job api.JobResponse
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(job.JobID, "job-") {
		t.Errorf("bad job_id: %q", job.JobID)
	}
	if job.Cwd != s.cwd {
		t.Errorf("cwd: %q want %q", job.Cwd, s.cwd)
	}

	// Wait for the default executor to run it.
	select {
	case <-s.executed:
	case <-time.After(time.Second):
		t.Fatal("executor never ran")
	}
	// Re-read; should be done.
	resp2, body2 := s.do("GET", "/v1/jobs/"+job.JobID, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status %d body=%s", resp2.StatusCode, body2)
	}
	var fin api.JobResponse
	_ = json.Unmarshal(body2, &fin)
	if fin.Status != string(runstore.StatusDone) {
		t.Fatalf("status: %q", fin.Status)
	}
	if fin.ClosureKind != string(runstore.ClosureDone) {
		t.Fatalf("closure_kind: %q", fin.ClosureKind)
	}
}

func TestSubmit_TaskBound_RendersPromptFromTask(t *testing.T) {
	s := newStack(t, nil, "")
	tk, err := s.tasks.CreateTask(context.Background(), store.Task{
		Title:       "Fix the bug",
		Description: "It crashes on save.",
		Status:      store.StatusNew, Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := s.do("POST", "/v1/jobs", api.SubmitRequest{TaskID: tk.ID})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	var job api.JobResponse
	_ = json.Unmarshal(body, &job)
	if !strings.Contains(job.PromptPreview, tk.ID) {
		t.Errorf("prompt preview missing task id: %q", job.PromptPreview)
	}
	if !strings.Contains(job.PromptPreview, "Fix the bug") {
		t.Errorf("prompt missing title: %q", job.PromptPreview)
	}
	if !job.AutoClaim {
		t.Errorf("auto_claim should default to true for task runs")
	}
}

func TestSubmit_RejectsBadThinking(t *testing.T) {
	s := newStack(t, nil, "")
	resp, _ := s.do("POST", "/v1/jobs", api.SubmitRequest{Prompt: "p", Thinking: "bogus"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSubmit_RejectsDangerousExtraArgs(t *testing.T) {
	s := newStack(t, nil, "")
	resp, _ := s.do("POST", "/v1/jobs", api.SubmitRequest{Prompt: "p", ExtraArgs: []string{"--api-key=secret"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSubmit_RejectsNonAbsoluteCwd(t *testing.T) {
	s := newStack(t, nil, "")
	resp, _ := s.do("POST", "/v1/jobs", api.SubmitRequest{Prompt: "p", Cwd: "relative/path"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestList_FilterByStatus(t *testing.T) {
	s := newStack(t, nil, "")
	r1, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})
	r2, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})
	_, _ = s.runs.MarkRunning(context.Background(), r2.JobID, 1)
	_, _ = s.runs.MarkDone(context.Background(), r2.JobID, 0, runstore.ClosureDone)
	// r1 still queued.
	_ = r1

	resp, body := s.do("GET", "/v1/jobs?status=queued", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	var got api.ListResponse
	_ = json.Unmarshal(body, &got)
	if len(got.Jobs) == 0 {
		t.Fatalf("got 0, want >=1 queued; body=%s", body)
	}
	for _, j := range got.Jobs {
		if j.Status != string(runstore.StatusQueued) {
			t.Fatalf("non-queued in queued result: %+v", j)
		}
	}
}

func TestCancel_QueuedRow(t *testing.T) {
	// Use an executor that blocks so cancellation has a window to fire.
	hold := make(chan struct{})
	defer close(hold)
	holding := scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
		<-hold
		return nil
	})
	s := newStack(t, holding, "")
	r, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})

	resp, body := s.do("DELETE", "/v1/jobs/"+r.JobID, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	got, _ := s.runs.GetRun(context.Background(), r.JobID)
	if got.Status != runstore.StatusCancelled {
		t.Fatalf("expected cancelled, got %+v", got)
	}
}

func TestCancel_TerminalIsIdempotent(t *testing.T) {
	s := newStack(t, nil, "")
	r, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})
	_, _ = s.runs.MarkRunning(context.Background(), r.JobID, 1)
	_, _ = s.runs.MarkDone(context.Background(), r.JobID, 0, runstore.ClosureDone)
	resp, _ := s.do("DELETE", "/v1/jobs/"+r.JobID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := newStack(t, nil, "")
	resp, _ := s.do("GET", "/v1/jobs/job-deadbe", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestMessages_MissingSession_410(t *testing.T) {
	s := newStack(t, nil, "")
	r, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})
	resp, _ := s.do("GET", "/v1/jobs/"+r.JobID+"/messages", nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status %d, want 410", resp.StatusCode)
	}
}

func TestMessages_ServesFromSessionPath(t *testing.T) {
	s := newStack(t, nil, "")
	r, _ := s.runs.CreateRun(context.Background(), runstore.NewRun{Prompt: "p", Cwd: s.cwd})
	// Write a minimal session.jsonl + record the path.
	jsonl := filepath.Join(s.cwd, "session.jsonl")
	if err := os.WriteFile(jsonl, []byte(
		`{"type":"session","id":"s1","timestamp":"2026-05-17T10:00:00Z","cwd":"/x"}`+"\n"+
			`{"type":"message","id":"e1","parentId":null,"timestamp":"2026-05-17T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.runs.SetPISession(context.Background(), r.JobID, "s1", jsonl); err != nil {
		t.Fatal(err)
	}
	resp, body := s.do("GET", "/v1/jobs/"+r.JobID+"/messages", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	var out api.MessagesResponse
	_ = json.Unmarshal(body, &out)
	if len(out.Events) != 2 {
		t.Fatalf("events: %+v", out.Events)
	}
	if out.Events[0].Kind != "session" || out.Events[1].Kind != "assistant_text" || out.Events[1].Text != "hi" {
		t.Fatalf("projection wrong: %+v", out.Events)
	}
}

func TestHealthAndVersion_AlwaysReachable(t *testing.T) {
	s := newStack(t, nil, "totally-secret")
	// Health/version skip auth even with a token set.
	resp, body := s.do("GET", "/v1/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d body=%s", resp.StatusCode, body)
	}
	var hr api.HealthResponse
	_ = json.Unmarshal(body, &hr)
	if !hr.OK || hr.Workers != 2 {
		t.Fatalf("health: %+v", hr)
	}
	resp, _ = s.do("GET", "/v1/version", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal("version not OK")
	}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	s := newStack(t, nil, "totally-secret")
	resp, _ := s.do("GET", "/v1/jobs", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAuth_AcceptsCorrectToken(t *testing.T) {
	s := newStack(t, nil, "totally-secret")
	resp, _ := s.do("GET", "/v1/jobs", nil, "Authorization", "Bearer totally-secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
