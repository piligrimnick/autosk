package datasource

import (
	"context"
	"errors"
	"os"
	"time"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/store"
)

// lazySource tags every write so the daemon reproduces lazy's dolt_commit
// dialect + behaviour (plan §7.5).
const lazySource = "lazy"

// RPC is a read-only Datasource backed by the autoskd JSON-RPC client
// (plan §9 Phase 1: "point a read-only lazy at autoskd"). Reads route to the
// daemon over the UDS (auto-spawning it on first use); writes return
// ErrReadOnlyRPC because the write surface lands in Phase 3.
//
// This is the seam that lets `autosk lazy` render entirely from autoskd before
// the Go front-ends lose their direct doltlite access. Once the write RPCs
// exist, RPC absorbs them and the Offline/Live/Compose split is deleted.
type RPC struct {
	cli *rpcclient.Client
}

// ErrReadOnlyRPC is returned by every write verb while autoskd only serves the
// Phase 1 read surface.
var ErrReadOnlyRPC = errors.New("read-only RPC mode (Phase 1): writes land in a later phase")

// NewRPC wires a read-only datasource over an autoskd client.
func NewRPC(cli *rpcclient.Client) *RPC { return &RPC{cli: cli} }

// Compile-time check that RPC satisfies the full TUI contract.
var _ Datasource = (*RPC)(nil)

// ---- reads ----------------------------------------------------------------

func (r *RPC) Tasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	raw, err := r.cli.Tasks(ctx, mapTaskFilter(f))
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(raw))
	for _, t := range raw {
		out = append(out, mapTask(t))
	}
	return out, nil
}

func (r *RPC) GetTask(ctx context.Context, id string) (Task, error) {
	t, err := r.cli.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return mapTask(t), nil
}

func (r *RPC) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	raw, err := r.cli.Jobs(ctx, rpcclient.JobListFilter{
		TaskID:     f.TaskID,
		WorkflowID: f.WorkflowID,
		Statuses:   f.Statuses,
		Limit:      f.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(raw))
	for _, j := range raw {
		out = append(out, mapJob(j))
	}
	return out, nil
}

func (r *RPC) GetJob(ctx context.Context, id string) (Job, error) {
	j, err := r.cli.GetJob(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return mapJob(j), nil
}

func (r *RPC) Workflows(ctx context.Context, includeSynthetic bool) ([]Workflow, error) {
	raw, err := r.cli.Workflows(ctx, includeSynthetic)
	if err != nil {
		return nil, err
	}
	out := make([]Workflow, 0, len(raw))
	for _, w := range raw {
		out = append(out, mapWorkflow(w))
	}
	return out, nil
}

func (r *RPC) Agents(ctx context.Context) ([]Agent, error) {
	raw, err := r.cli.Agents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(raw))
	for _, a := range raw {
		out = append(out, Agent{
			ID: a.ID, Name: a.Name, IsHuman: a.IsHuman, Source: a.Source,
			Version: a.Version, Model: a.Model, Thinking: a.Thinking,
			ExtraArgs: a.ExtraArgs, PiSkills: a.PiSkills, PiExt: a.PiExt,
			TasksOwned: a.TasksOwned,
		})
	}
	return out, nil
}

func (r *RPC) Comments(ctx context.Context, taskID string) ([]Comment, error) {
	raw, err := r.cli.Comments(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, Comment{
			ID: c.ID, TaskID: c.TaskID, AuthorID: c.AuthorID,
			AuthorName: c.AuthorName, Text: c.Text, CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

func (r *RPC) Signals(ctx context.Context, jobID string) ([]Signal, error) {
	raw, err := r.cli.SignalsForJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return mapSignals(raw), nil
}

func (r *RPC) SignalsForTask(ctx context.Context, taskID string) ([]Signal, error) {
	raw, err := r.cli.SignalsForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return mapSignals(raw), nil
}

func (r *RPC) Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error) {
	// MessageEvent is an alias for api.MessageEvent — the client returns it
	// directly, no mapping needed.
	return r.cli.Messages(ctx, jobID, full, limit)
}

func (r *RPC) Healthz(ctx context.Context) (Health, error) {
	h, err := r.cli.Healthz(ctx)
	if err != nil {
		return Health{Daemon: "down", UpdatedAt: time.Now().UTC()}, nil //nolint:nilerr // health probe never errors to the TUI
	}
	daemon := "down"
	if h.OK {
		daemon = "ok"
	}
	return Health{
		Daemon:    daemon,
		Workers:   h.Workers,
		Queued:    h.Queued,
		Running:   h.Running,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

// Reconnect is a no-op: the RPC datasource holds no doltlite connection to
// refresh (the daemon owns the file). Cross-process GC freshness is the
// daemon's concern.
func (r *RPC) Reconnect(ctx context.Context) error { return nil }

// StreamLive requires the Phase 2 live job.subscribe tail.
func (r *RPC) StreamLive(ctx context.Context, jobID string) (*LiveHandle, error) {
	return nil, ErrDaemonRequired
}

// ---- writes (Phase 3) -----------------------------------------------------

func (r *RPC) CreateTask(ctx context.Context, title, description string, priority int) (string, error) {
	p := priority
	t, err := r.cli.CreateTask(ctx, rpcclient.TaskCreateParams{
		Source: lazySource, Title: title, Description: description, Priority: &p,
	})
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

func (r *RPC) UpdateStatus(ctx context.Context, id string, status store.Status) error {
	_, err := r.cli.SetStatus(ctx, id, string(status))
	return err
}

func (r *RPC) UpdatePriority(ctx context.Context, id string, p int) error {
	_, err := r.cli.SetPriority(ctx, id, p)
	return err
}

func (r *RPC) UpdateTitleDescription(ctx context.Context, id, title, description string) error {
	_, err := r.cli.SetTitleDescription(ctx, id, title, description)
	return err
}

func (r *RPC) Enroll(ctx context.Context, id, workflow, stepName string) error {
	_, err := r.cli.Enroll(ctx, lazySource, id, workflow, "", stepName, "")
	return err
}

func (r *RPC) Resume(ctx context.Context, id, toStep string) error {
	_, err := r.cli.Resume(ctx, lazySource, id, toStep)
	return err
}

func (r *RPC) Block(ctx context.Context, id, blocker string) error {
	return r.cli.Block(ctx, lazySource, id, []string{blocker})
}

func (r *RPC) Unblock(ctx context.Context, id, blocker string) error {
	return r.cli.Unblock(ctx, lazySource, id, []string{blocker})
}

func (r *RPC) AddComment(ctx context.Context, taskID, text string) error {
	_, err := r.cli.AddComment(ctx, lazySource, taskID, os.Getenv("AUTOSK_AGENT"), text)
	return err
}

func (r *RPC) SetMetadata(ctx context.Context, id string, m map[string]any) error {
	var v any = m
	if m == nil {
		v = map[string]any{}
	}
	_, err := r.cli.MetadataSet(ctx, lazySource, id, "", v, true)
	return err
}

func (r *RPC) CreateWorkflow(ctx context.Context, jsonOrPath string) (string, error) {
	// Lazy passes a file path (mirrors Offline.CreateWorkflow → ParseFile).
	// noInstall=true: Go-lazy's Offline.CreateWorkflow calls ws.Create(.., false)
	// directly and never auto-installs missing scoped agents, so a workflow
	// referencing an uninstalled agent must fail validation (no surprise npm
	// side effects from the TUI). The CLI path keeps its own --no-install gate.
	return r.cli.WorkflowCreate(ctx, lazySource, jsonOrPath, "", true)
}

func (r *RPC) DeleteWorkflow(ctx context.Context, name string) error {
	return r.cli.WorkflowDelete(ctx, lazySource, name)
}

func (r *RPC) UpdateWorkflowIsolation(ctx context.Context, name, mode string, force bool) (UpdateIsolationReport, error) {
	// Thread lazySource so the daemon commits the `lazy: workflow update …`
	// dialect; surface the partial report even on error (Go-lazy returns
	// `(out, err)` so the TUI can render the rollback/leftover diagnostics).
	rep, err := r.cli.WorkflowUpdateIsolation(ctx, lazySource, name, mode, force, false)
	return mapIsolationReport(rep), err
}

func (r *RPC) InstallAgent(ctx context.Context, name, version string) error {
	_, err := r.cli.AgentInstall(ctx, name, version)
	return err
}

func (r *RPC) UninstallAgent(ctx context.Context, name string) error {
	return r.cli.AgentUninstall(ctx, name, false)
}

func (r *RPC) CancelJob(ctx context.Context, jobID string) error {
	_, err := r.cli.CancelJob(ctx, jobID)
	return err
}

func (r *RPC) SendInput(ctx context.Context, jobID, message, behavior string) (string, error) {
	return r.cli.SendInput(ctx, jobID, message, behavior)
}

func (r *RPC) AbortJob(ctx context.Context, jobID string) error {
	return r.cli.AbortJob(ctx, jobID)
}

func mapIsolationReport(rep rpcclient.UpdateIsolationReport) UpdateIsolationReport {
	out := UpdateIsolationReport{
		Workflow:         rep.Workflow,
		From:             rep.From,
		To:               rep.To,
		Noop:             rep.Noop,
		NonTerminalTasks: rep.NonTerminalTasks,
		FailedTask:       rep.FailedTask,
	}
	for _, e := range rep.EnsuredTasks {
		out.EnsuredTasks = append(out.EnsuredTasks, EnsureRecord{TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing})
	}
	for _, l := range rep.LeftoverWorktrees {
		out.LeftoverWorktrees = append(out.LeftoverWorktrees, LeftoverWorktree{TaskID: l.TaskID, Path: l.Path})
	}
	for _, e := range rep.RolledBackEnsures {
		out.RolledBackEnsures = append(out.RolledBackEnsures, EnsureRecord{TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing})
	}
	return out
}

// ---- mapping helpers ------------------------------------------------------

func mapTaskFilter(f TaskFilter) rpcclient.TaskListFilter {
	var statuses []string
	if f.Statuses != nil {
		statuses = make([]string, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			statuses = append(statuses, string(s))
		}
	}
	return rpcclient.TaskListFilter{
		Statuses:      statuses,
		Priority:      f.Priority,
		WorkflowID:    f.WorkflowID,
		AgentName:     f.AgentName,
		AuthorName:    f.AuthorName,
		StepAgentName: f.StepAgentName,
		Search:        f.Search,
	}
}

func mapTask(t rpcclient.Task) Task {
	return Task{
		ID: t.ID, Title: t.Title, Description: t.Description,
		Status: store.Status(t.Status), Priority: t.Priority,
		AuthorID: t.AuthorID, AuthorName: t.AuthorName,
		WorkflowID: t.WorkflowID, WorkflowName: t.WorkflowName,
		CurrentStepID: t.CurrentStepID, StepName: t.StepName, AgentName: t.AgentName,
		Blocked:      t.Blocked,
		BlockedBy:    mapRefs(t.BlockedBy),
		Blocks:       mapRefs(t.Blocks),
		CommentCount: t.CommentCount, Metadata: t.Metadata,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func mapRefs(in []rpcclient.TaskRef) []TaskRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]TaskRef, 0, len(in))
	for _, r := range in {
		out = append(out, TaskRef{ID: r.ID, Status: store.Status(r.Status)})
	}
	return out
}

func mapJob(j rpcclient.Job) Job {
	return Job{
		JobResponse:  j.JobResponse,
		WorkflowName: j.WorkflowName,
		StepName:     j.StepName,
		AgentName:    j.AgentName,
	}
}

func mapWorkflow(w rpcclient.Workflow) Workflow {
	out := Workflow{
		ID: w.ID, Name: w.Name, Description: w.Description,
		IsSynthetic: w.IsSynthetic, FirstStep: w.FirstStep,
		TaskCount: w.TaskCount, Isolation: w.Isolation,
		NonTerminalTaskCount: w.NonTerminalTaskCount,
	}
	for _, s := range w.Steps {
		out.Steps = append(out.Steps, WorkflowStep{
			ID: s.ID, Name: s.Name, AgentName: s.AgentName,
			NextSteps: s.NextSteps, NextStatus: s.NextStatus, TaskCount: s.TaskCount,
		})
	}
	for _, nt := range w.NonTerminalTasks {
		out.NonTerminalTasks = append(out.NonTerminalTasks, NonTerminalTaskRef{
			ID: nt.ID, Status: store.Status(nt.Status), StepName: nt.StepName,
		})
	}
	return out
}

func mapSignals(in []rpcclient.Signal) []Signal {
	out := make([]Signal, 0, len(in))
	for _, s := range in {
		out = append(out, Signal{
			TransitionID: s.TransitionID, TaskID: s.TaskID, JobID: s.JobID,
			StepID: s.StepID, StepName: s.StepName,
			WorkflowID: s.WorkflowID, WorkflowName: s.WorkflowName,
			Target: s.Target, AgentID: s.AgentID, AgentName: s.AgentName,
			CreatedAt: s.CreatedAt,
		})
	}
	return out
}
