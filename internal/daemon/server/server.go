// Package server hosts the daemon's HTTP API.
//
// Routes (see plan §5.1):
//
//	POST   /v1/jobs
//	GET    /v1/jobs
//	GET    /v1/jobs/{job_id}
//	DELETE /v1/jobs/{job_id}
//	GET    /v1/jobs/{job_id}/messages
//	GET    /v1/healthz
//	GET    /v1/version
//
// SSE (`/v1/jobs/{id}/stream`) is added in D7.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"autosk/internal/buildinfo"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/transcript"
	"autosk/internal/store"
)

// Deps is the set of collaborators a Server needs. They are wired up by
// cmd/autosk/daemon.go.
type Deps struct {
	Runs       *runstore.Store
	Tasks      store.Store
	Sched      *scheduler.Scheduler
	Workers    int
	Token      string // empty disables auth
	DefaultCwd string
	DBPath     string // absolute path to .autosk/db (for diagnostics)
}

// Server wraps an *http.ServeMux with our routes + auth middleware.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// New constructs a Server.
func New(d Deps) *Server {
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the http.Handler with auth middleware applied.
func (s *Server) Handler() http.Handler {
	return s.authMiddleware(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/jobs", s.handleSubmit)
	s.mux.HandleFunc("GET /v1/jobs", s.handleList)
	s.mux.HandleFunc("GET /v1/jobs/{job_id}", s.handleGet)
	s.mux.HandleFunc("DELETE /v1/jobs/{job_id}", s.handleCancel)
	s.mux.HandleFunc("GET /v1/jobs/{job_id}/messages", s.handleMessages)
	s.mux.HandleFunc("GET /v1/jobs/{job_id}/stream", s.handleStream)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
}

// ---- middleware ----------------------------------------------------------

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Token == "" || r.URL.Path == "/v1/healthz" || r.URL.Path == "/v1/version" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.deps.Token
		if got != want {
			writeError(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- handlers -----------------------------------------------------------

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req api.SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode_body: "+err.Error(), nil)
		return
	}
	req, err := req.Validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// Resolve cwd.
	cwd := req.Cwd
	if cwd == "" {
		cwd = s.deps.DefaultCwd
	}
	if cwd == "" {
		writeError(w, http.StatusBadRequest, "cwd is empty and no default configured", nil)
		return
	}
	if !filepath.IsAbs(cwd) {
		writeError(w, http.StatusBadRequest, "cwd must be absolute", map[string]any{"cwd": cwd})
		return
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "cwd does not exist or is not a directory", map[string]any{"cwd": cwd})
		return
	}

	// Verify the autosk task exists. Always do this first so the error
	// includes the daemon's db_path — the #1 confusion is the daemon
	// reading a different .autosk/db than the client.
	if req.TaskID != "" {
		if _, err := s.deps.Tasks.GetTask(r.Context(), req.TaskID); err != nil {
			writeError(w, http.StatusBadRequest, "task not found", map[string]any{
				"task_id":         req.TaskID,
				"daemon_db_path":  s.deps.DBPath,
				"hint":            "the daemon resolves .autosk/db relative to its own --cwd; make sure it matches the directory where you created the task",
			})
			return
		}
	}

	// Resolve auto_claim default: true when task_id present, false for ad-hoc.
	autoClaim := req.TaskID != ""
	if req.AutoClaim != nil {
		autoClaim = *req.AutoClaim
	}
	maxCorr := 3
	if req.MaxCorrections != nil {
		maxCorr = *req.MaxCorrections
	}

	// Materialise the prompt: if task_id is set and prompt is empty, render
	// from the task's title + description. Existence was already verified above.
	prompt := req.Prompt
	if prompt == "" && req.TaskID != "" {
		tk, err := s.deps.Tasks.GetTask(r.Context(), req.TaskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get_task: "+err.Error(), nil)
			return
		}
		prompt = renderTaskPrompt(tk)
	}

	run, err := s.deps.Runs.CreateRun(r.Context(), runstore.NewRun{
		TaskID:         req.TaskID,
		Prompt:         prompt,
		Model:          req.Model,
		Thinking:       runstore.ThinkingLevel(req.Thinking),
		Cwd:            cwd,
		AutoClaim:      autoClaim,
		MaxCorrections: maxCorr,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create_run: "+err.Error(), nil)
		return
	}
	if err := s.deps.Sched.Enqueue(run.JobID); err != nil {
		if errors.Is(err, scheduler.ErrQueueFull) {
			writeError(w, http.StatusServiceUnavailable, "queue_full", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "enqueue: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusCreated, api.FromRun(run))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := runstore.RunFilter{
		TaskID: q.Get("task_id"),
	}
	if ss := q.Get("status"); ss != "" && ss != "all" {
		for _, part := range strings.Split(ss, ",") {
			st := runstore.RunStatus(strings.TrimSpace(part))
			if !st.Valid() {
				writeError(w, http.StatusBadRequest, "invalid status: "+part, nil)
				return
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}
	if lim := q.Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit", nil)
			return
		}
		filter.Limit = n
	}
	runs, err := s.deps.Runs.ListRuns(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_runs: "+err.Error(), nil)
		return
	}
	out := api.ListResponse{Jobs: make([]api.JobResponse, 0, len(runs))}
	for _, r := range runs {
		out.Jobs = append(out.Jobs, api.FromRun(r))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	run, err := s.deps.Runs.GetRun(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, runstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found", map[string]any{"job_id": jobID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, api.FromRun(run))
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	run, err := s.deps.Runs.GetRun(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, runstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if run.Status.IsTerminal() {
		// Idempotent: already done; return current state.
		writeJSON(w, http.StatusOK, api.FromRun(run))
		return
	}
	// Try the scheduler first (active jobs).
	if err := s.deps.Sched.Cancel(jobID); err != nil && !errors.Is(err, scheduler.ErrJobNotActive) {
		writeError(w, http.StatusInternalServerError, "cancel: "+err.Error(), nil)
		return
	}
	if errors.Is(err, scheduler.ErrJobNotActive) || err == nil {
		// Queued (never executed) or not yet picked up: persist cancellation
		// directly so the worker won't pick up a cancelled run.
		if run.Status == runstore.StatusQueued {
			_, _ = s.deps.Runs.MarkCancelled(r.Context(), jobID, nil)
		}
	}
	// Re-read for the response.
	run, _ = s.deps.Runs.GetRun(r.Context(), jobID)
	writeJSON(w, http.StatusAccepted, api.FromRun(run))
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	run, err := s.deps.Runs.GetRun(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, runstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if run.SessionPath == "" {
		writeError(w, http.StatusGone, "session_missing: session_path is empty", nil)
		return
	}

	q := r.URL.Query()
	full := q.Get("full") == "true"
	limit := 20
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit", nil)
			return
		}
		if n > 500 {
			n = 500
		}
		limit = n
	}

	var (
		events    []transcript.Event
		truncated bool
	)
	if full {
		events, err = transcript.Read(run.SessionPath)
	} else {
		all, rerr := transcript.Read(run.SessionPath)
		err = rerr
		if rerr == nil {
			if limit < len(all) {
				truncated = true
				events = all[len(all)-limit:]
			} else {
				events = all
			}
		}
	}
	if err != nil {
		if errors.Is(err, transcript.ErrMissing) {
			writeError(w, http.StatusGone, "session_missing", map[string]any{"path": run.SessionPath})
			return
		}
		writeError(w, http.StatusInternalServerError, "read_transcript: "+err.Error(), nil)
		return
	}

	resp := api.MessagesResponse{
		JobID:     jobID,
		Events:    make([]api.MessageEvent, 0, len(events)),
		Truncated: truncated,
	}
	for _, e := range events {
		resp.Events = append(resp.Events, api.MessageEvent{
			Kind:    string(e.Kind),
			TS:      e.TS,
			Text:    e.Text,
			Name:    e.Name,
			Input:   rawOrNil(e.Input),
			IsError: e.IsError,
			Raw:     rawOrNil(e.Raw),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Count queued + running rows for an at-a-glance health view.
	runs, _ := s.deps.Runs.ListRuns(r.Context(), runstore.RunFilter{
		Statuses: []runstore.RunStatus{runstore.StatusQueued, runstore.StatusRunning},
	})
	var queued, running int
	for _, r := range runs {
		switch r.Status {
		case runstore.StatusQueued:
			queued++
		case runstore.StatusRunning:
			running++
		}
	}
	writeJSON(w, http.StatusOK, api.HealthResponse{
		OK:         true,
		Workers:    s.deps.Workers,
		Queued:     queued,
		Running:    running,
		DBPath:     s.deps.DBPath,
		DefaultCwd: s.deps.DefaultCwd,
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.VersionResponse{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit,
	})
}

// ---- helpers -----------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string, details map[string]any) {
	writeJSON(w, status, api.ErrorResponse{Error: msg, Details: details})
}

func rawOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// renderTaskPrompt builds the initial prompt from a task's title and
// description. We deliberately surface the autosk task id so the agent
// can call `autosk done <id>` correctly.
func renderTaskPrompt(t store.Task) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are working on autosk task `%s`.\n\n", t.ID)
	if t.Title != "" {
		fmt.Fprintf(&sb, "Title: %s\n\n", t.Title)
	}
	if t.Description != "" {
		sb.WriteString("Description:\n")
		sb.WriteString(t.Description)
		sb.WriteString("\n\n")
	}
	sb.WriteString("When the work is complete (or you have decided to cancel or decompose it), close the task via the `autosk` extension.\n")
	return sb.String()
}

// nowUnix returns the current time. Exposed for tests if needed.
func nowUnix() int64 { return time.Now().Unix() }
