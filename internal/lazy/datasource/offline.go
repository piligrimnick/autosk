package datasource

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/comments"
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/transcript"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// Offline is a Datasource backed entirely by the project's .autosk/db
// + the on-disk session.jsonl transcripts. No daemon traffic.
//
// Writes go through the same store APIs the CLI uses (so commits land
// in doltlite, comments insert via comments.Store, etc.) which keeps
// the lazy TUI semantically identical to the CLI when the daemon is
// down. Verbs that fundamentally need a daemon (CancelJob, SendInput,
// AbortJob, StreamLive) return ErrDaemonRequired.
type Offline struct {
	s        *doltlite.Store
	cwd      string
	registry *pkgregistry.Registry
}

// NewOffline wires an Offline datasource on top of an already-open
// store. The caller retains ownership of s; closing the store while
// the datasource is in use is undefined behaviour.
//
// registry is optional; when nil agent metadata is filled in best
// effort from the DB row alone.
func NewOffline(s store.Store, cwd string, registry *pkgregistry.Registry) (*Offline, error) {
	dl, ok := s.(*doltlite.Store)
	if !ok {
		return nil, fmt.Errorf("offline datasource: store is not doltlite (%T)", s)
	}
	return &Offline{s: dl, cwd: cwd, registry: registry}, nil
}

// DB returns the underlying *sql.DB. Exposed for tests / palette ops
// that need a raw query; ordinary callers should not reach in.
func (o *Offline) DB() *sql.DB { return o.s.DB() }

// Tasks lists matching tasks with all derived fields resolved.
func (o *Offline) Tasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	statuses := f.Statuses
	if statuses == nil {
		statuses = store.OpenStatuses()
	}
	raw, err := o.s.ListTasks(ctx, store.ListFilter{Statuses: statuses, Priority: f.Priority, Limit: 0})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	out := make([]Task, 0, len(raw))
	for _, r := range raw {
		t, err := o.projectTask(ctx, r)
		if err != nil {
			return nil, err
		}
		if f.WorkflowID != "" && t.WorkflowID != f.WorkflowID {
			continue
		}
		if f.AgentName != "" {
			// Match author OR current step agent.
			if !strings.EqualFold(t.AuthorName, f.AgentName) && !strings.EqualFold(t.AgentName, f.AgentName) {
				continue
			}
		}
		if f.Search != "" {
			needle := strings.ToLower(f.Search)
			if !strings.Contains(strings.ToLower(t.ID), needle) &&
				!strings.Contains(strings.ToLower(t.Title), needle) {
				continue
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// GetTask returns one task with all derived fields resolved.
func (o *Offline) GetTask(ctx context.Context, id string) (Task, error) {
	raw, err := o.s.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return o.projectTask(ctx, raw)
}

func (o *Offline) projectTask(ctx context.Context, raw store.Task) (Task, error) {
	t := Task{
		ID:            raw.ID,
		Title:         raw.Title,
		Description:   raw.Description,
		Status:        raw.Status,
		Priority:      raw.Priority,
		AuthorID:      raw.AuthorID,
		WorkflowID:    raw.WorkflowID,
		CurrentStepID: raw.CurrentStepID,
		CreatedAt:     raw.CreatedAt,
		UpdatedAt:     raw.UpdatedAt,
	}
	if raw.AuthorID != "" {
		ag := agent.New(o.s.DB())
		a, err := ag.GetByID(ctx, raw.AuthorID)
		if err == nil {
			t.AuthorName = a.Name
		}
	}
	if raw.WorkflowID != "" {
		wf, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).GetByID(ctx, raw.WorkflowID)
		if err == nil {
			t.WorkflowName = wf.Name
		}
	}
	if raw.CurrentStepID != "" {
		st, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).FindStepByID(ctx, raw.CurrentStepID)
		if err == nil {
			t.StepName = st.Name
			t.AgentName = st.AgentName
		}
	}
	if blocked, err := o.s.IsBlocked(ctx, raw.ID); err == nil {
		t.Blocked = blocked
	}
	if in, out, err := o.s.Deps(ctx, raw.ID); err == nil {
		t.BlockedBy = in
		t.Blocks = out
	}
	cs := comments.New(o.s.DB())
	if list, err := cs.ListByTask(ctx, raw.ID); err == nil {
		t.CommentCount = len(list)
	}
	return t, nil
}

// Jobs reads daemon_runs and decorates each row with workflow / step
// / agent names for display.
func (o *Offline) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	statuses := make([]runstore.RunStatus, 0, len(f.Statuses))
	for _, s := range f.Statuses {
		statuses = append(statuses, runstore.RunStatus(s))
	}
	rs := runstore.New(o.s.DB())
	raw, err := rs.ListRuns(ctx, runstore.RunFilter{Statuses: statuses, TaskID: f.TaskID, Limit: f.Limit})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	out := make([]Job, 0, len(raw))
	for _, r := range raw {
		j := Job{JobResponse: api.FromRun(r)}
		if f.WorkflowID != "" {
			st, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).FindStepByID(ctx, r.StepID)
			if err != nil || st.WorkflowID != f.WorkflowID {
				continue
			}
			j.StepName = st.Name
			j.AgentName = st.AgentName
			j.WorkflowName = wfName(o.s.DB(), st.WorkflowID)
		} else if r.StepID != "" {
			st, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).FindStepByID(ctx, r.StepID)
			if err == nil {
				j.StepName = st.Name
				j.AgentName = st.AgentName
				j.WorkflowName = wfName(o.s.DB(), st.WorkflowID)
			}
		}
		out = append(out, j)
	}
	return out, nil
}

// GetJob returns one job (DB-backed; Streaming/AttachCount stay 0).
func (o *Offline) GetJob(ctx context.Context, id string) (Job, error) {
	rs := runstore.New(o.s.DB())
	r, err := rs.GetRun(ctx, id)
	if err != nil {
		return Job{}, err
	}
	j := Job{JobResponse: api.FromRun(r)}
	if r.StepID != "" {
		st, err := workflow.New(o.s.DB(), agent.New(o.s.DB())).FindStepByID(ctx, r.StepID)
		if err == nil {
			j.StepName = st.Name
			j.AgentName = st.AgentName
			j.WorkflowName = wfName(o.s.DB(), st.WorkflowID)
		}
	}
	return j, nil
}

func wfName(db *sql.DB, wfID string) string {
	if wfID == "" {
		return ""
	}
	var name string
	_ = db.QueryRowContext(context.Background(),
		`SELECT name FROM workflows WHERE id = ?`, wfID).Scan(&name)
	return name
}

// Workflows lists workflows + their steps + their per-step task counts.
func (o *Offline) Workflows(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	list, err := ws.List(ctx, includeSynthetic)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	out := make([]Workflow, 0, len(list))
	for _, w := range list {
		full, err := ws.GetByID(ctx, w.ID)
		if err != nil {
			continue
		}
		out = append(out, projectWorkflow(o.s.DB(), full))
	}
	return out, nil
}

func projectWorkflow(db *sql.DB, w workflow.Workflow) Workflow {
	out := Workflow{
		ID: w.ID, Name: w.Name, Description: w.Description, IsSynthetic: w.IsSynthetic,
	}
	firstStep := ""
	for _, s := range w.Steps {
		if s.ID == w.FirstStepID {
			firstStep = s.Name
			break
		}
	}
	out.FirstStep = firstStep
	for _, s := range w.Steps {
		ws := WorkflowStep{ID: s.ID, Name: s.Name, AgentName: s.AgentName}
		for _, tr := range s.Transitions {
			if tr.IsTaskStatus() {
				ws.NextStatus = append(ws.NextStatus, tr.TaskStatus)
			} else if tr.NextStepName != "" {
				ws.NextSteps = append(ws.NextSteps, tr.NextStepName)
			}
		}
		var n int
		_ = db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM tasks WHERE current_step_id = ?`, s.ID).Scan(&n)
		ws.TaskCount = n
		out.Steps = append(out.Steps, ws)
		out.TaskCount += n
	}
	return out
}

// Agents lists DB agents + registry / package metadata.
func (o *Offline) Agents(ctx context.Context) ([]Agent, error) {
	as := agent.New(o.s.DB())
	list, err := as.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	out := make([]Agent, 0, len(list))
	for _, a := range list {
		out = append(out, o.projectAgent(a))
	}
	// Tasks owned: author OR current_step agent.
	for i := range out {
		var n int
		_ = o.s.DB().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tasks t
			LEFT JOIN steps s ON s.id = t.current_step_id
			WHERE t.author_id = ? OR s.agent_id = ?
		`, out[i].ID, out[i].ID).Scan(&n)
		out[i].TasksOwned = n
	}
	return out, nil
}

func (o *Offline) projectAgent(a agent.Agent) Agent {
	out := Agent{
		ID: a.ID, Name: a.Name, IsHuman: a.IsHuman,
	}
	if a.IsHuman {
		out.Source = "builtin"
		return out
	}
	out.Source = "db_only"
	if o.registry == nil {
		return out
	}
	if !o.registry.Has(a.Name) {
		return out
	}
	cfg, err := o.registry.Resolve(a.Name)
	if err != nil {
		return out
	}
	out.Source = "installed"
	out.Version = cfg.Version
	out.Model = cfg.Model
	out.Thinking = cfg.Thinking
	out.ExtraArgs = cfg.ExtraArgs
	out.PiExt = cfg.PiExtensions
	out.PiSkills = cfg.PiSkills
	return out
}

// Comments returns the task's comment thread, oldest first.
func (o *Offline) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	cs := comments.New(o.s.DB())
	raw, err := cs.ListByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, Comment{
			ID: c.ID, TaskID: c.TaskID, AuthorID: c.AuthorID, AuthorName: c.AuthorName,
			Text: c.Text, CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

// Signals returns step_signals rows attached to taskID across all
// runs, newest first.
func (o *Offline) Signals(ctx context.Context, taskID string) ([]Signal, error) {
	rows, err := o.s.DB().QueryContext(ctx, `
		SELECT ss.id, ss.task_id, ss.run_id, ss.created_at,
		       dr.step_id, st.name,
		       COALESCE(t.next_step_id, ''), COALESCE(t.task_status, ''),
		       COALESCE(ns.name, ''),
		       st.agent_id, a.name
		  FROM step_signals ss
		  JOIN daemon_runs dr      ON dr.job_id = ss.run_id
		  JOIN steps st            ON st.id = dr.step_id
		  JOIN agents a            ON a.id = st.agent_id
		  LEFT JOIN step_transitions t  ON t.id = ss.transition_id
		  LEFT JOIN steps ns       ON ns.id = t.next_step_id
		 WHERE ss.task_id = ?
		 ORDER BY ss.created_at DESC, ss.id DESC
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list signals: %w", err)
	}
	defer rows.Close()
	var out []Signal
	for rows.Next() {
		var (
			s        Signal
			created  int64
			nextID   string
			status   string
			nextName string
		)
		if err := rows.Scan(&s.ID, &s.TaskID, &s.JobID, &created,
			&s.StepID, &s.StepName, &nextID, &status, &nextName,
			&s.AgentID, &s.AgentName); err != nil {
			return nil, err
		}
		s.CreatedAt = time.Unix(created, 0).UTC()
		switch {
		case status != "":
			s.Target = status
		case nextName != "":
			s.Target = nextName
		default:
			s.Target = "(unknown)"
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Messages reads a job's session.jsonl from disk and projects to
// MessageEvents. Offline always has access to the file; live tabs use
// this when SSE isn't available.
func (o *Offline) Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error) {
	rs := runstore.New(o.s.DB())
	r, err := rs.GetRun(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if r.SessionPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(r.SessionPath); err != nil {
		return nil, nil
	}
	events, err := transcript.Read(r.SessionPath)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	if !full && limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	out := make([]MessageEvent, 0, len(events))
	for _, e := range events {
		out = append(out, MessageEvent{
			Kind:    string(e.Kind),
			TS:      e.TS,
			Text:    e.Text,
			Name:    e.Name,
			Input:   asAny(e.Input),
			IsError: e.IsError,
			Raw:     asAny(e.Raw),
		})
	}
	return out, nil
}

func asAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// Healthz always reports daemon=down in offline mode.
func (o *Offline) Healthz(ctx context.Context) (Health, error) {
	return Health{Daemon: "down", UpdatedAt: time.Now().UTC()}, nil
}

// ---- writes -------------------------------------------------------------

// CreateTask inserts a task and returns its id.
func (o *Offline) CreateTask(ctx context.Context, title, description string, priority int) (string, error) {
	if title = strings.TrimSpace(title); title == "" {
		return "", fmt.Errorf("title is required")
	}
	if priority < store.MinPriority || priority > store.MaxPriority {
		priority = store.DefaultPriority
	}
	t, err := o.s.CreateTask(ctx, store.Task{Title: title, Description: description, Priority: priority, Status: store.StatusNew})
	if err != nil {
		return "", err
	}
	_ = o.s.DoltCommit(ctx, "lazy: create task "+t.ID)
	return t.ID, nil
}

// UpdateStatus rewrites tasks.status.
func (o *Offline) UpdateStatus(ctx context.Context, id string, status store.Status) error {
	if !status.Valid() {
		return fmt.Errorf("invalid status %q", status)
	}
	if _, err := o.s.UpdateTask(ctx, id, store.TaskPatch{Status: &status}); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: status "+id+"="+string(status))
	return nil
}

// UpdatePriority rewrites tasks.priority.
func (o *Offline) UpdatePriority(ctx context.Context, id string, p int) error {
	if p < store.MinPriority || p > store.MaxPriority {
		return fmt.Errorf("priority must be in [%d,%d]", store.MinPriority, store.MaxPriority)
	}
	if _, err := o.s.UpdateTask(ctx, id, store.TaskPatch{Priority: &p}); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, fmt.Sprintf("lazy: priority %s=%d", id, p))
	return nil
}

// Enroll attaches an existing task to a workflow's first step.
func (o *Offline) Enroll(ctx context.Context, id, wfName string) error {
	t, err := o.s.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != store.StatusNew {
		return fmt.Errorf("enroll: task is not 'new' (status=%s)", t.Status)
	}
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	wf, err := ws.GetByName(ctx, wfName)
	if err != nil {
		return fmt.Errorf("workflow %q: %w", wfName, err)
	}
	newStatus := store.StatusInWorkflow
	if _, err := o.s.UpdateTask(ctx, id, store.TaskPatch{
		Status:        &newStatus,
		WorkflowID:    &wf.ID,
		CurrentStepID: &wf.FirstStepID,
	}); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, fmt.Sprintf("lazy: enroll %s -> %s", id, wfName))
	return nil
}

// EnrollAgent attaches an existing task to the synthetic single:<agent>
// workflow.
func (o *Offline) EnrollAgent(ctx context.Context, id, agentName string) error {
	return o.Enroll(ctx, id, "single:"+agentName)
}

// Resume flips a task from human_feedback back to in_workflow,
// optionally relocating its current step.
func (o *Offline) Resume(ctx context.Context, id, toStep string) error {
	t, err := o.s.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != store.StatusHumanFeedback {
		return fmt.Errorf("resume: task is not 'human_feedback' (status=%s)", t.Status)
	}
	patch := store.TaskPatch{Status: ptrStatus(store.StatusInWorkflow)}
	if toStep != "" {
		ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
		st, err := ws.FindStepByName(ctx, t.WorkflowID, toStep)
		if err != nil {
			return fmt.Errorf("resume target step %q: %w", toStep, err)
		}
		patch.CurrentStepID = &st.ID
	}
	if _, err := o.s.UpdateTask(ctx, id, patch); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: resume "+id)
	return nil
}

func ptrStatus(s store.Status) *store.Status { return &s }

// Block adds a blocker edge id ← blocker.
func (o *Offline) Block(ctx context.Context, id, blocker string) error {
	if err := o.s.Block(ctx, id, blocker); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: block "+id+"<-"+blocker)
	return nil
}

// Unblock removes a blocker edge.
func (o *Offline) Unblock(ctx context.Context, id, blocker string) error {
	if err := o.s.Unblock(ctx, id, blocker); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: unblock "+id+"<-"+blocker)
	return nil
}

// AddComment inserts a comment authored by the current user.
func (o *Offline) AddComment(ctx context.Context, taskID, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("comment text is empty")
	}
	as := agent.New(o.s.DB())
	authorName := os.Getenv("AUTOSK_AGENT")
	if authorName == "" {
		authorName = agent.HumanAgentName
	}
	a, err := as.EnsureByName(ctx, authorName)
	if err != nil {
		return fmt.Errorf("ensure author %q: %w", authorName, err)
	}
	cs := comments.New(o.s.DB())
	if _, err := cs.Add(ctx, taskID, a.ID, text); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: comment "+taskID)
	return nil
}

// CreateWorkflow loads a JSON file from disk and persists it.
func (o *Offline) CreateWorkflow(ctx context.Context, path string) (string, error) {
	def, err := workflow.ParseFile(path)
	if err != nil {
		return "", fmt.Errorf("parse workflow %q: %w", path, err)
	}
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	wf, err := ws.Create(ctx, def, false)
	if err != nil {
		return "", err
	}
	_ = o.s.DoltCommit(ctx, "lazy: create workflow "+wf.Name)
	return wf.Name, nil
}

// DeleteWorkflow removes a workflow by name. Refuses on referenced wfs.
func (o *Offline) DeleteWorkflow(ctx context.Context, name string) error {
	ws := workflow.New(o.s.DB(), agent.New(o.s.DB()))
	if err := ws.Delete(ctx, name); err != nil {
		return err
	}
	_ = o.s.DoltCommit(ctx, "lazy: delete workflow "+name)
	return nil
}

// InstallAgent is the offline-mode shim that only adds the row to the
// agents table; running 'npm install' from inside the TUI is left to
// the live datasource which dispatches via the daemon. Offline returns
// ErrDaemonRequired so the popup can surface a clean error.
func (o *Offline) InstallAgent(ctx context.Context, name, version string) error {
	return ErrDaemonRequired
}

// UninstallAgent mirrors InstallAgent — needs the registry.
func (o *Offline) UninstallAgent(ctx context.Context, name string) error {
	return ErrDaemonRequired
}

// CancelJob requires a live daemon.
func (o *Offline) CancelJob(ctx context.Context, jobID string) error { return ErrDaemonRequired }

// SendInput requires a live daemon.
func (o *Offline) SendInput(ctx context.Context, jobID, message, behavior string) (string, error) {
	return "", ErrDaemonRequired
}

// AbortJob requires a live daemon.
func (o *Offline) AbortJob(ctx context.Context, jobID string) error { return ErrDaemonRequired }

// StreamLive requires a live daemon.
func (o *Offline) StreamLive(ctx context.Context, jobID string) (*LiveHandle, error) {
	return nil, ErrDaemonRequired
}
