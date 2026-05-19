// Package server hosts the daemon's HTTP API.
//
// The daemon listens on a single per-host unix-domain socket and serves
// requests scoped to any number of projects. The project is selected by
// the X-Autosk-Cwd / X-Autosk-DB request headers and resolved through
// internal/daemon/projectmgr.
//
// Routes:
//
//	GET    /v1/jobs
//	GET    /v1/jobs/{job_id}
//	DELETE /v1/jobs/{job_id}
//	GET    /v1/jobs/{job_id}/messages
//	GET    /v1/jobs/{job_id}/stream
//	GET    /v1/healthz                  (per-project; ?all=true → aggregate)
//	GET    /v1/version
//
// POST /v1/jobs no longer exists — work is enrolled into a workflow
// via the autosk CLI and picked up by the per-project poller.
//
// Per docs/plans/20260518-Daemon-UDS-Plan.md §4.4–4.6.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"autosk/internal/buildinfo"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/pirunners"
	"autosk/internal/daemon/projectmgr"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/transcript"
)

// Deps is the set of collaborators a Server needs. The multi-project
// daemon wires Projects + Sched at startup; per-request handlers fish
// their per-project stores out of the resolved *projectmgr.Project.
//
// Runners and Attachments are the daemon-wide in-memory hubs that the
// attach surface needs: Runners maps jobID→live pi runner handles (so
// /input and /abort can talk to the right pi process), Attachments
// counts attached SSE clients per job. Both default to nil — when
// either is nil, the attach routes return 503 ("attach disabled").
type Deps struct {
	Projects    *projectmgr.Manager
	Sched       *scheduler.Scheduler
	Workers     int
	Runners     *pirunners.Registry
	Attachments *pirunners.Attachments
}

// Server wraps an *http.ServeMux with our routes + project middleware.
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

// Handler returns the http.Handler with project middleware applied.
func (s *Server) Handler() http.Handler {
	return s.projectMiddleware(s.mux)
}

// AttachEnabled reports whether both attach hubs (Runners + Attachments)
// were wired into Deps. When this returns false, /input + /abort fail
// with 503 and Streaming/AttachCount on JobResponse are always zero.
//
// Exposed for production-wiring tests; ordinary callers shouldn't need it.
func (s *Server) AttachEnabled() bool {
	return s.deps.Runners != nil && s.deps.Attachments != nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/jobs", s.handleList)
	s.mux.HandleFunc("GET /v1/jobs/{job_id}", s.handleGet)
	s.mux.HandleFunc("DELETE /v1/jobs/{job_id}", s.handleCancel)
	s.mux.HandleFunc("GET /v1/jobs/{job_id}/messages", s.handleMessages)
	s.mux.HandleFunc("GET /v1/jobs/{job_id}/stream", s.handleStream)
	s.mux.HandleFunc("POST /v1/jobs/{job_id}/input", s.handleInput)
	s.mux.HandleFunc("POST /v1/jobs/{job_id}/abort", s.handleAbort)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
}

// ---- handlers -----------------------------------------------------------

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
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
	runs, err := proj.Runs.ListRuns(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_runs: "+err.Error(), nil)
		return
	}
	out := api.ListResponse{Jobs: make([]api.JobResponse, 0, len(runs))}
	for _, r := range runs {
		out.Jobs = append(out.Jobs, s.decorateRun(r))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	jobID := r.PathValue("job_id")
	run, err := proj.Runs.GetRun(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, runstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found", map[string]any{"job_id": jobID})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, s.decorateRun(run))
}

// decorateRun populates AttachCount and Streaming on a JobResponse
// from the live pirunners hubs. Safe to call with nil Runners or
// nil Attachments — both fields collapse to their zero values, which
// matches "no live runner / no attached clients".
func (s *Server) decorateRun(r runstore.Run) api.JobResponse {
	out := api.FromRun(r)
	if s.deps.Attachments != nil {
		out.AttachCount = s.deps.Attachments.Count(r.JobID)
	}
	if s.deps.Runners != nil {
		if h, err := s.deps.Runners.Get(r.JobID); err == nil && h != nil {
			out.Streaming = h.IsStreaming()
		}
	}
	return out
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	jobID := r.PathValue("job_id")
	run, err := proj.Runs.GetRun(r.Context(), jobID)
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
		writeJSON(w, http.StatusOK, s.decorateRun(run))
		return
	}
	job := scheduler.Job{Project: proj.Root, ID: jobID}
	cerr := s.deps.Sched.Cancel(job)
	if cerr != nil && !errors.Is(cerr, scheduler.ErrJobNotActive) {
		writeError(w, http.StatusInternalServerError, "cancel: "+cerr.Error(), nil)
		return
	}
	if errors.Is(cerr, scheduler.ErrJobNotActive) || cerr == nil {
		// Queued (never executed) or not yet picked up: persist cancellation
		// directly so the worker won't pick up a cancelled run.
		if run.Status == runstore.StatusQueued {
			_, _ = proj.Runs.MarkCancelled(r.Context(), jobID, nil)
		}
	}
	// Re-read for the response.
	run, _ = proj.Runs.GetRun(r.Context(), jobID)
	writeJSON(w, http.StatusAccepted, s.decorateRun(run))
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	jobID := r.PathValue("job_id")
	run, err := proj.Runs.GetRun(r.Context(), jobID)
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
	aggregate := strings.EqualFold(r.URL.Query().Get("all"), "true")
	if aggregate {
		s.writeAggregateHealth(w, r)
		return
	}
	proj := projectFromCtx(r.Context())
	if proj == nil {
		writeError(w, http.StatusInternalServerError, "missing project context", nil)
		return
	}
	queued, running := s.projectCounts(r, proj)
	writeJSON(w, http.StatusOK, api.HealthResponse{
		OK:          true,
		Workers:     s.deps.Workers,
		Queued:      queued,
		Running:     running,
		DBPath:      proj.DBPath,
		ProjectRoot: proj.Root,
	})
}

// writeAggregateHealth returns a snapshot of every loaded project.
func (s *Server) writeAggregateHealth(w http.ResponseWriter, r *http.Request) {
	loaded := s.deps.Projects.Loaded()
	out := api.HealthResponse{
		OK:       true,
		Workers:  s.deps.Workers,
		Projects: make([]api.HealthProject, 0, len(loaded)),
	}
	for _, p := range loaded {
		q, n := s.projectCounts(r, p)
		out.Projects = append(out.Projects, api.HealthProject{
			Root:     p.Root,
			DBPath:   p.DBPath,
			Queued:   q,
			Running:  n,
			OpenedAt: p.OpenedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// projectCounts returns (queued, running) for one project.
func (s *Server) projectCounts(r *http.Request, p *projectmgr.Project) (int, int) {
	rows, err := p.Runs.ListRuns(r.Context(), runstore.RunFilter{
		Statuses: []runstore.RunStatus{runstore.StatusQueued, runstore.StatusRunning},
	})
	if err != nil {
		return 0, 0
	}
	var q, n int
	for _, rr := range rows {
		switch rr.Status {
		case runstore.StatusQueued:
			q++
		case runstore.StatusRunning:
			n++
		}
	}
	return q, n
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
