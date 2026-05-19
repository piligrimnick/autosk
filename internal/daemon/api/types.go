// Package api defines the daemon's HTTP wire types.
//
// Routes live in package server; this file is the source of truth for
// request/response JSON shapes. Both the daemon HTTP server and the
// CLI client (cmd/autosk/daemon.go subcommands) import these types.
//
// The daemon listens on a per-host unix-domain socket and serves any
// number of projects. Per-request project context is carried in
// X-Autosk-Cwd and (optionally) X-Autosk-DB headers. There is no
// server-side default cwd anymore.
package api

import (
	"time"

	"autosk/internal/daemon/runstore"
)

// JobResponse is the canonical Job shape returned by the API.
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
//
// When the request did not opt into the cross-project aggregate
// (?all=true), Queued/Running are the scoped counts for the project
// identified by X-Autosk-Cwd and ProjectRoot/DBPath are set. With
// ?all=true the per-project counts are reported as a list under
// Projects and the scoped fields are zero.
type HealthResponse struct {
	OK          bool            `json:"ok"`
	Workers     int             `json:"workers"`
	Queued      int             `json:"queued"`
	Running     int             `json:"running"`
	DBPath      string          `json:"db_path,omitempty"`
	ProjectRoot string          `json:"project_root,omitempty"`
	Projects    []HealthProject `json:"projects,omitempty"`
}

// HealthProject is one row of the aggregated health view (?all=true).
type HealthProject struct {
	Root     string    `json:"root"`
	DBPath   string    `json:"db_path"`
	Queued   int       `json:"queued"`
	Running  int       `json:"running"`
	OpenedAt time.Time `json:"opened_at"`
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
