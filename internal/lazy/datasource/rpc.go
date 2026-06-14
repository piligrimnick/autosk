package datasource

import (
	"context"
	"errors"
	"os"
	"time"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
)

// RPC is the Datasource backed by the autoskd JSON-RPC client (proto-v2: the
// single RPC-client Datasource impl). Reads AND writes route to the daemon over
// the UDS (auto-spawning it on first use); every request carries the {cwd}
// project selector only (v2 dropped the cli|lazy source discriminator and the
// dolt commits it drove). Live transcript tailing lands via session.subscribe.
type RPC struct {
	cli *rpcclient.Client
}

// NewRPC wires a datasource over an autoskd client.
func NewRPC(cli *rpcclient.Client) *RPC { return &RPC{cli: cli} }

// Compile-time check that RPC satisfies the full TUI contract, including the
// optional Watcher push capability.
var (
	_ Datasource = (*RPC)(nil)
	_ Watcher    = (*RPC)(nil)
)

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

func (r *RPC) Sessions(ctx context.Context, taskID string) ([]Session, error) {
	raw, err := r.cli.Sessions(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(raw))
	for _, s := range raw {
		out = append(out, mapSession(s))
	}
	return out, nil
}

func (r *RPC) GetSession(ctx context.Context, id string) (Session, error) {
	s, err := r.cli.GetSession(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return mapSession(s), nil
}

func (r *RPC) Workflows(ctx context.Context) ([]Workflow, error) {
	raw, err := r.cli.Workflows(ctx)
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
		out = append(out, Agent{Name: a.Name})
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
			ID: c.ID, Author: c.Author, Text: c.Text,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	return out, nil
}

// NOTE: Signals and Messages removed in v2

func (r *RPC) Healthz(ctx context.Context) (Health, error) {
	h, err := r.cli.Healthz(ctx)
	if err != nil {
		return Health{Daemon: "down", UpdatedAt: time.Now().UTC()}, nil //nolint:nilerr // health probe never errors to the TUI
	}
	// Reachable but !OK maps to "stale" (parity with the old Live.Healthz: the
	// daemon answered but reports an unhealthy/degraded state). Only an
	// unreachable daemon (err above) is "down". The status-bar renderer and the
	// Health doc both advertise the three-state {ok|down|stale} contract.
	daemon := "stale"
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

// Reconnect is a no-op: the RPC datasource holds no store connection to
// refresh (the daemon owns the project store). Store freshness is the
// daemon's concern.
func (r *RPC) Reconnect(ctx context.Context) error { return nil }

// SessionTranscript fetches a session's full archived transcript (every line)
// and adapts each pi-format line to a LiveEvent, oldest first. Used by the
// Detail pane's archive load for both running (initial snapshot) and terminal
// (no SSE) sessions.
func (r *RPC) SessionTranscript(ctx context.Context, sessionID string) ([]LiveEvent, error) {
	res, err := r.cli.Transcript(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]LiveEvent, 0, len(res.Entries))
	for i, line := range res.Entries {
		out = append(out, LiveEvent{Kind: "message", Line: line, LineNum: i + 1})
	}
	return out, nil
}

// StreamSession opens a session.subscribe tail over the daemon's persistent
// connection and adapts the session-event frames to the TUI's LiveEvent shape.
// The caller must Close the returned handle.
func (r *RPC) StreamSession(ctx context.Context, sessionID string) (*LiveHandle, error) {
	stream, err := r.cli.SessionSubscribe(ctx, sessionID, 0) // 0 = from start
	if err != nil {
		return nil, err
	}
	ch := make(chan LiveEvent, 32)
	go func() {
		defer close(ch)
		for ev := range stream.Events() {
			out := LiveEvent{LineNum: ev.Line}
			terminal := false
			switch ev.Kind {
			case "message":
				out.Kind = "message"
				if ev.Event != nil {
					out.Line = *ev.Event
				}
			case "status":
				out.Kind = "status"
				if ev.Session != nil {
					out.Session = *ev.Session
				}
			case "done":
				out.Kind = "done"
				if ev.Session != nil {
					out.Session = *ev.Session
				}
				terminal = true
			case "error":
				out.Kind = "error"
				out.Err = errors.New(ev.Error)
			default:
				continue
			}
			select {
			case ch <- out:
			case <-ctx.Done():
				return
			}
			if terminal {
				_ = stream.Close()
				return
			}
		}
	}()
	return NewLiveHandle(ch, func() error { return stream.Close() }), nil
}

// Watch opens a task-changed/project-changed subscription over the daemon's
// task.subscribe stream and adapts the notification frames to the TUI's
// ChangeEvent shape. The caller must Close the returned handle. This is the
// server push that replaces lazy's former client-side 2s poll (plan §5): the
// daemon emits `task-changed` eagerly after every write AND from a per-project
// change poller (so its own executor-driven advances notify too), plus
// `project-changed` on workflow/agent edits.
func (r *RPC) Watch(ctx context.Context) (*WatchHandle, error) {
	stream, err := r.cli.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan ChangeEvent, 16)
	go func() {
		defer close(ch)
		for n := range stream.Events() {
			kind := "task"
			switch n.Method {
			case "project-changed":
				kind = "project"
			case "session-changed":
				kind = "session"
			}
			select {
			case ch <- ChangeEvent{Kind: kind}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return NewWatchHandle(ch, func() error { return stream.Close() }), nil
}

// ---- writes (Phase 3) -----------------------------------------------------

func (r *RPC) CreateTask(ctx context.Context, title, description string) (string, error) {
	var blockedBy []string // empty slice for v2
	t, err := r.cli.CreateTask(ctx, title, description, blockedBy)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

func (r *RPC) TaskDone(ctx context.Context, id string) error {
	_, err := r.cli.TaskDone(ctx, id)
	return err
}

func (r *RPC) TaskCancel(ctx context.Context, id string) error {
	_, err := r.cli.TaskCancel(ctx, id)
	return err
}

func (r *RPC) TaskReopen(ctx context.Context, id string) error {
	_, err := r.cli.TaskReopen(ctx, id)
	return err
}

func (r *RPC) UpdateTask(ctx context.Context, id string, title, description *string) error {
	_, err := r.cli.UpdateTask(ctx, id, title, description)
	return err
}

func (r *RPC) EnrollWorkflow(ctx context.Context, id, workflow string) error {
	_, err := r.cli.EnrollWorkflow(ctx, id, workflow)
	return err
}

func (r *RPC) EnrollAgent(ctx context.Context, id, agent string) error {
	_, err := r.cli.EnrollAgent(ctx, id, agent)
	return err
}

func (r *RPC) Resume(ctx context.Context, id, toStep string) error {
	var target *rpcclient.StepTarget
	if toStep != "" {
		target = &rpcclient.StepTarget{Step: toStep}
	}
	_, err := r.cli.Resume(ctx, id, target)
	return err
}

func (r *RPC) Block(ctx context.Context, id, blocker string) error {
	_, err := r.cli.Block(ctx, id, blocker)
	return err
}

func (r *RPC) Unblock(ctx context.Context, id, blocker string) error {
	_, err := r.cli.Unblock(ctx, id, blocker)
	return err
}

func (r *RPC) AddComment(ctx context.Context, taskID, text string) error {
	_, err := r.cli.AddComment(ctx, taskID, os.Getenv("AUTOSK_AGENT"), text)
	return err
}

// NOTE: SetMetadata removed in v2

// NOTE: workflow/agent writes removed in v2 - workflows are read-only

func (r *RPC) AbortSession(ctx context.Context, id string) error {
	return r.cli.SessionAbort(ctx, id)
}

func (r *RPC) SessionInput(ctx context.Context, id, message, kind string) error {
	return r.cli.SessionInput(ctx, id, message, kind)
}

// NOTE: mapIsolationReport removed - v2 workflows are read-only

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
		Statuses: statuses,
		Workflow: f.WorkflowName,
		Step:     f.StepName,
		Blocked:  f.Blocked,
	}
}

func mapTask(t rpcclient.Task) Task {
	return Task{
		ID: t.ID, Title: t.Title, Description: t.Description,
		Status:       store.Status(t.Status),
		WorkflowName: t.Workflow, // v2 field name
		StepName:     t.Step,     // v2 field name
		Blocked:      t.Blocked,
		BlockedBy:    mapRefs(t.BlockedBy),
		Blocks:       mapRefs(t.Blocks),
		CommentCount: t.CommentCount,
		CreatedAt:    t.CreatedAt,
		UpdatedAt:    t.UpdatedAt,
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

func mapSession(s rpcclient.Session) Session {
	return Session{
		ID:        s.ID,
		TaskID:    s.TaskID,
		Workflow:  s.Workflow,
		Step:      s.Step,
		Agent:     s.Agent,
		Status:    runstore.RunStatus(s.Status),
		Error:     s.Error,
		StartedAt: s.StartedAt,
		EndedAt:   s.EndedAt,
	}
}

func mapWorkflow(w rpcclient.WorkflowInfo) Workflow {
	out := Workflow{
		Name:        w.Name,
		Description: w.Description,
		FirstStep:   w.FirstStep,
		Isolation:   w.Isolation,
	}
	for _, s := range w.Steps {
		targets := make([]string, 0, len(s.Targets))
		for _, t := range s.Targets {
			if t.Step != "" {
				targets = append(targets, t.Step)
			} else if t.Status != "" {
				targets = append(targets, t.Status)
			}
		}
		out.Steps = append(out.Steps, WorkflowStep{
			Name:      s.Name,
			AgentName: s.Agent,
			Human:     s.Human,
			Targets:   targets,
		})
	}
	return out
}

// NOTE: mapSignals removed - v2 has no signals
