package datasource

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/client"
)

// Live overlays the daemon's HTTP/UDS API on top of an Offline base.
// All read verbs prefer the daemon's view (so Jobs carry the live
// Streaming / AttachCount values, and StreamLive opens an SSE
// subscription) but fall back to the embedded Offline for anything
// the daemon doesn't serve: DB-only entities (tasks, workflows,
// agents, comments, signals) and writes.
//
// Constructed by NewLive — never directly. The compose layer flips
// between Live and Offline based on a 2s health probe.
type Live struct {
	*Offline
	cli *client.Client
}

// NewLive wires a Live datasource on top of an Offline base.
func NewLive(base *Offline, cli *client.Client) *Live {
	return &Live{Offline: base, cli: cli}
}

// Jobs prefers the daemon's view so Streaming/AttachCount are live.
func (l *Live) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	resp, err := l.cli.ListJobs(ctx, f.TaskID, f.Statuses, f.Limit)
	if err != nil {
		// Daemon hiccup — fall back to the DB.
		return l.Offline.Jobs(ctx, f)
	}
	out := make([]Job, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		dj := Job{JobResponse: j}
		// Pull workflow/step labels via the DB (the API doesn't carry them).
		if j.StepID != "" {
			off := l.Offline
			if dj2, derr := off.GetJob(ctx, j.JobID); derr == nil {
				dj.StepName = dj2.StepName
				dj.AgentName = dj2.AgentName
				dj.WorkflowName = dj2.WorkflowName
			}
		}
		if f.WorkflowID != "" && dj.WorkflowName == "" {
			continue
		}
		out = append(out, dj)
	}
	return out, nil
}

// GetJob hits the daemon's GET /v1/jobs/{id} for the live shape.
func (l *Live) GetJob(ctx context.Context, id string) (Job, error) {
	resp, err := l.cli.GetJob(ctx, id)
	if err != nil {
		return l.Offline.GetJob(ctx, id)
	}
	j := Job{JobResponse: resp}
	if dj, derr := l.Offline.GetJob(ctx, id); derr == nil {
		j.StepName = dj.StepName
		j.AgentName = dj.AgentName
		j.WorkflowName = dj.WorkflowName
	}
	return j, nil
}

// Messages prefers the daemon's /messages?full=true so the transcript
// includes events that haven't been flushed to disk yet.
func (l *Live) Messages(ctx context.Context, jobID string, full bool, limit int) ([]MessageEvent, error) {
	resp, err := l.cli.GetMessages(ctx, jobID, full, limit)
	if err != nil {
		return l.Offline.Messages(ctx, jobID, full, limit)
	}
	out := make([]MessageEvent, 0, len(resp.Events))
	out = append(out, resp.Events...)
	return out, nil
}

// Healthz pings the daemon and translates the response.
func (l *Live) Healthz(ctx context.Context) (Health, error) {
	resp, err := l.cli.Healthz(ctx)
	if err != nil {
		return Health{Daemon: "down", UpdatedAt: time.Now().UTC()}, nil
	}
	state := "ok"
	if !resp.OK {
		state = "stale"
	}
	return Health{
		Daemon:    state,
		Workers:   resp.Workers,
		Queued:    resp.Queued,
		Running:   resp.Running,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

// CancelJob asks the daemon to cancel a run.
func (l *Live) CancelJob(ctx context.Context, jobID string) error {
	_, err := l.cli.CancelJob(ctx, jobID)
	return err
}

// SendInput dispatches operator text to the daemon's pi runner.
func (l *Live) SendInput(ctx context.Context, jobID, message, behavior string) (string, error) {
	resp, err := l.cli.SendInput(ctx, jobID, message, behavior)
	if err != nil {
		return "", err
	}
	return resp.Dispatched, nil
}

// AbortJob asks pi to stop its in-flight turn.
func (l *Live) AbortJob(ctx context.Context, jobID string) error {
	_, err := l.cli.Abort(ctx, jobID)
	return err
}

// StreamLive opens an SSE subscription with attach=true.
func (l *Live) StreamLive(ctx context.Context, jobID string) (*LiveHandle, error) {
	h, err := l.cli.Stream(ctx, jobID, client.StreamOptions{Attach: true, Full: true})
	if err != nil {
		return nil, err
	}
	ch := make(chan LiveEvent, 32)
	go func() {
		defer close(ch)
		for ev := range h.Events {
			out := LiveEvent{EventID: ev.EventID}
			switch ev.Type {
			case client.EventTypeMessage:
				out.Kind = "message"
				if ev.Message != nil {
					out.Message = *ev.Message
				}
			case client.EventTypeStatus:
				out.Kind = "status"
				if ev.Status != nil {
					out.Status = *ev.Status
				}
			case client.EventTypeDone:
				out.Kind = "done"
				if ev.Status != nil {
					out.Status = *ev.Status
				}
			case client.EventTypeError:
				out.Kind = "error"
				out.Err = ev.Err
			default:
				continue
			}
			select {
			case ch <- out:
			case <-ctx.Done():
				return
			}
		}
	}()
	return NewLiveHandle(ch, func() error { h.Close(); return nil }), nil
}

// InstallAgent / UninstallAgent: still no daemon endpoint for these,
// so they return ErrDaemonRequired even in Live mode. (The agent
// install verb shells out to `autosk agent install` outside the
// daemon; the lazy TUI documents this in the popup hint.)
func (l *Live) InstallAgent(ctx context.Context, name, version string) error {
	return errors.New("agent install: run `autosk agent install` outside lazy (TUI shell-out is out of scope)")
}

// UninstallAgent — see InstallAgent.
func (l *Live) UninstallAgent(ctx context.Context, name string) error {
	return errors.New("agent uninstall: run `autosk agent uninstall` outside lazy (TUI shell-out is out of scope)")
}

// jobMatchesFilter is the in-memory filter we apply after fetching
// because the daemon API doesn't support workflow scoping on the
// /jobs endpoint.
func jobMatchesFilter(j Job, f JobFilter) bool {
	if f.WorkflowID != "" && j.WorkflowName == "" {
		return false
	}
	return true
}

// statusesArg mirrors the client's status-set encoding for tests.
func statusesArg(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return fmt.Sprintf("status=%s", apiCSV(s))
}

func apiCSV(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// Compile-time interface check.
var _ Datasource = (*Live)(nil)
var _ Datasource = (*Offline)(nil)

// jobResponseFlat exists so test fixtures can build a wire-shape Job
// without importing api.
type jobResponseFlat = api.JobResponse
