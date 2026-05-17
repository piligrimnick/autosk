// Package api defines the daemon's HTTP wire types and validation rules.
//
// Routes live in package server; this file is the source of truth for
// request/response JSON shapes. Both the daemon HTTP server and the
// CLI client (cmd/autosk/daemon.go subcommands) import these types.
package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/daemon/runstore"
)

// SubmitRequest is POST /v1/jobs.
type SubmitRequest struct {
	TaskID         string   `json:"task_id,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	Model          string   `json:"model,omitempty"`
	Thinking       string   `json:"thinking,omitempty"`
	Cwd            string   `json:"cwd,omitempty"`
	AutoClaim      *bool    `json:"auto_claim,omitempty"`
	MaxCorrections *int     `json:"max_corrections,omitempty"`
	ExtraArgs      []string `json:"extra_args,omitempty"`
}

// JobResponse is the canonical Job shape returned by the API. Mirrors
// plan §5.3.
type JobResponse struct {
	JobID           string     `json:"job_id"`
	TaskID          string     `json:"task_id,omitempty"`
	Status          string     `json:"status"`
	Model           string     `json:"model,omitempty"`
	Thinking        string     `json:"thinking,omitempty"`
	Cwd             string     `json:"cwd"`
	PromptPreview   string     `json:"prompt_preview"`
	PISessionID     string     `json:"pi_session_id,omitempty"`
	SessionPath     string     `json:"session_path,omitempty"`
	PID             *int       `json:"pid,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Error           string     `json:"error,omitempty"`
	ClosureKind     string     `json:"closure_kind,omitempty"`
	AutoClaim       bool       `json:"auto_claim"`
	CorrectionsUsed int        `json:"corrections_used"`
	MaxCorrections  int        `json:"max_corrections"`
	PreBlockedBy    []string   `json:"pre_blocked_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationMS      int64      `json:"duration_ms"`
}

// MessagesResponse is GET /v1/jobs/{id}/messages.
type MessagesResponse struct {
	JobID     string          `json:"job_id"`
	Events    []MessageEvent  `json:"events"`
	Truncated bool            `json:"truncated"`
}

// MessageEvent is the transcript event shape served by the messages API.
// Mirrors internal/daemon/transcript.Event but with explicit JSON tags so
// the wire form is stable.
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
	DBPath     string `json:"db_path,omitempty"`     // absolute path to .autosk/db the daemon uses
	DefaultCwd string `json:"default_cwd,omitempty"` // cwd applied when a submit omits it
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

// dangerousExtraArgs are CLI flags the daemon manages itself; the client
// must not override them via extra_args (plan §5.2).
var dangerousExtraArgs = map[string]struct{}{
	"--api-key":         {},
	"--system-prompt":   {},
	"--mode":            {},
	"--session-dir":     {},
	"--session":         {},
	"--no-session":      {},
	"--print":           {},
	"-p":                {},
	"--model":           {},
	"--thinking":        {},
}

// Validate checks SubmitRequest and normalises blank fields. Returns the
// canonical form on success.
func (r SubmitRequest) Validate() (SubmitRequest, error) {
	out := r
	out.TaskID = strings.TrimSpace(out.TaskID)
	out.Prompt = strings.TrimSpace(out.Prompt)
	out.Cwd = strings.TrimSpace(out.Cwd)
	out.Model = strings.TrimSpace(out.Model)
	out.Thinking = strings.TrimSpace(out.Thinking)

	if out.TaskID == "" && out.Prompt == "" {
		return out, fmt.Errorf("%w: task_id or prompt is required", ErrInvalidRequest)
	}
	if !runstore.ThinkingLevel(out.Thinking).Valid() {
		return out, fmt.Errorf("%w: thinking %q is not in {off,minimal,low,medium,high,xhigh}", ErrInvalidRequest, out.Thinking)
	}
	for _, a := range out.ExtraArgs {
		key := a
		if i := strings.IndexByte(a, '='); i > 0 {
			key = a[:i]
		}
		if _, bad := dangerousExtraArgs[key]; bad {
			return out, fmt.Errorf("%w: extra_args may not contain %q (daemon-managed)", ErrInvalidRequest, key)
		}
	}
	if out.MaxCorrections != nil && *out.MaxCorrections < 0 {
		return out, fmt.Errorf("%w: max_corrections must be >= 0", ErrInvalidRequest)
	}
	return out, nil
}

// FromRun projects a runstore.Run to its API shape.
func FromRun(r runstore.Run) JobResponse {
	resp := JobResponse{
		JobID:           r.JobID,
		TaskID:          r.TaskID,
		Status:          string(r.Status),
		Model:           r.Model,
		Thinking:        string(r.Thinking),
		Cwd:             r.Cwd,
		PromptPreview:   preview(r.Prompt, 200),
		PISessionID:     r.PISessionID,
		SessionPath:     r.SessionPath,
		PID:             r.PID,
		ExitCode:        r.ExitCode,
		Error:           r.Error,
		ClosureKind:     string(r.ClosureKind),
		AutoClaim:       r.AutoClaim,
		CorrectionsUsed: r.CorrectionsUsed,
		MaxCorrections:  r.MaxCorrections,
		PreBlockedBy:    r.PreBlockedBy,
		CreatedAt:       r.CreatedAt,
		StartedAt:       r.StartedAt,
		FinishedAt:      r.FinishedAt,
		DurationMS:      r.Duration().Milliseconds(),
	}
	return resp
}

func preview(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\u2026"
}
