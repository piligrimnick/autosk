// Package api defines the daemon's HTTP wire types and validation rules.
//
// Routes live in package server; this file is the source of truth for
// request/response JSON shapes. Both the daemon HTTP server and the
// CLI client (cmd/autosk/daemon.go subcommands) import these types.
//
// v0.2 (workflows): SubmitRequest carries only task_id; everything else
// (agent, model, thinking, prompt, cwd) is derived from the workflow,
// step, agent-config file, and daemon config. The pre-W1 fields are
// preserved here as deprecated optionals for one release so old clients
// don't break instantly; they are ignored by the server.
package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/daemon/runstore"
)

// SubmitRequest is POST /v1/jobs.
//
// In v0.2 only TaskID and MaxCorrections are honored. Other fields are
// accepted (for backwards compatibility of the JSON shape only) but the
// server logs and ignores them. The CLI clients no longer set them.
type SubmitRequest struct {
	TaskID         string `json:"task_id"`
	MaxCorrections *int   `json:"max_corrections,omitempty"`

	// Deprecated; accepted but ignored. Removed in a future release.
	Prompt    string   `json:"prompt,omitempty"`
	Model     string   `json:"model,omitempty"`
	Thinking  string   `json:"thinking,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	AutoClaim *bool    `json:"auto_claim,omitempty"`
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// JobResponse is the canonical Job shape returned by the API.
//
// Most v0.1 fields are gone. The agent that ran is now derivable from
// (StepID → agents); for convenience the projection at runtime fills
// AgentName when the join is cheap, but the wire shape does not require it.
type JobResponse struct {
	JobID           string     `json:"job_id"`
	TaskID          string     `json:"task_id"`
	StepID          string     `json:"step_id"`
	Status          string     `json:"status"`
	TransitionID    *int64     `json:"transition_id,omitempty"`
	PISessionID     string     `json:"pi_session_id,omitempty"`
	SessionPath     string     `json:"session_path,omitempty"`
	PID             *int       `json:"pid,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Error           string     `json:"error,omitempty"`
	CorrectionsUsed int        `json:"corrections_used"`
	MaxCorrections  int        `json:"max_corrections"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationMS      int64      `json:"duration_ms"`
}

// MessagesResponse is GET /v1/jobs/{id}/messages.
type MessagesResponse struct {
	JobID     string         `json:"job_id"`
	Events    []MessageEvent `json:"events"`
	Truncated bool           `json:"truncated"`
}

// MessageEvent is the transcript event shape served by the messages API.
type MessageEvent struct {
	Kind    string      `json:"kind"`
	TS      time.Time   `json:"ts,omitempty"`
	Text    string      `json:"text,omitempty"`
	Name    string      `json:"name,omitempty"`
	Input   interface{} `json:"input,omitempty"`
	IsError bool        `json:"is_error,omitempty"`
	Raw     interface{} `json:"raw,omitempty"`
}

// HealthResponse is GET /v1/healthz.
type HealthResponse struct {
	OK         bool   `json:"ok"`
	Workers    int    `json:"workers"`
	Queued     int    `json:"queued"`
	Running    int    `json:"running"`
	DBPath     string `json:"db_path,omitempty"`
	DefaultCwd string `json:"default_cwd,omitempty"`
}

// VersionResponse is GET /v1/version.
type VersionResponse struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// ErrorResponse is the standard error envelope.
type ErrorResponse struct {
	Error   string         `json:"error"`
	Details map[string]any `json:"details,omitempty"`
}

// ListResponse is GET /v1/jobs.
type ListResponse struct {
	Jobs []JobResponse `json:"jobs"`
}

// ---- validation ----------------------------------------------------------

// ErrInvalidRequest is wrapped by validation failures.
var ErrInvalidRequest = errors.New("invalid request")

// Validate checks SubmitRequest and normalises blank fields.
func (r SubmitRequest) Validate() (SubmitRequest, error) {
	out := r
	out.TaskID = strings.TrimSpace(out.TaskID)
	if out.TaskID == "" {
		return out, fmt.Errorf("%w: task_id is required", ErrInvalidRequest)
	}
	if out.MaxCorrections != nil && *out.MaxCorrections < 0 {
		return out, fmt.Errorf("%w: max_corrections must be >= 0", ErrInvalidRequest)
	}
	return out, nil
}

// FromRun projects a runstore.Run to its API shape.
func FromRun(r runstore.Run) JobResponse {
	return JobResponse{
		JobID:           r.JobID,
		TaskID:          r.TaskID,
		StepID:          r.StepID,
		Status:          string(r.Status),
		TransitionID:    r.TransitionID,
		PISessionID:     r.PISessionID,
		SessionPath:     r.SessionPath,
		PID:             r.PID,
		ExitCode:        r.ExitCode,
		Error:           r.Error,
		CorrectionsUsed: r.CorrectionsUsed,
		MaxCorrections:  r.MaxCorrections,
		CreatedAt:       r.CreatedAt,
		StartedAt:       r.StartedAt,
		FinishedAt:      r.FinishedAt,
		DurationMS:      r.Duration().Milliseconds(),
	}
}
