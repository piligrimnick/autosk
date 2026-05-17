// Package runstore is the storage abstraction the autosk daemon uses to
// persist daemon_runs rows.
//
// v0.2 shape (docs/plans/20260517-Workflows-Plan.md §4.1): one row per
// STEP execution. step_id is always set (single-agent runs go through the
// synthetic single:<agent> workflow). Most of the v0.1 columns (prompt,
// model, thinking, cwd, auto_claim, pre_blocked_by, closure_kind,
// agent_id) are gone. The agent and the prompt are rendered fresh per
// spawn from (task, current_step, comments) by the executor in W6.
//
// The package does not own the *sql.DB — callers (the daemon main) pass
// the same handle the task Store uses, so writes serialize through the
// doltlite single-writer connection.
package runstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/id"
)

// RunStatus is the lifecycle state of a job. Mirrors the SQL CHECK enum.
type RunStatus string

const (
	StatusQueued    RunStatus = "queued"
	StatusRunning   RunStatus = "running"
	StatusDone      RunStatus = "done"
	StatusFailed    RunStatus = "failed"
	StatusCancelled RunStatus = "cancelled"
)

// Valid reports whether s is one of the five allowed values.
func (s RunStatus) Valid() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusDone, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// IsTerminal reports whether s is a sticky terminal status.
func (s RunStatus) IsTerminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// IDPrefix is the prefix for generated job ids.
const IDPrefix = "job"

// IDBytes is the random byte count for generated job ids
// (3 bytes = 6 hex chars).
const IDBytes = 3

// Run is the in-memory representation of a daemon_runs row.
type Run struct {
	JobID           string
	TaskID          string // non-empty for every workflow run
	StepID          string // non-empty: every run targets a step (real or synthetic)
	Status          RunStatus
	TransitionID    *int64 // populated on successful workflow run
	ExitCode        *int
	PID             *int
	PISessionID     string
	SessionPath     string
	Error           string
	MaxCorrections  int
	CorrectionsUsed int
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// Duration is finished_at - started_at when both are set; otherwise 0.
func (r Run) Duration() time.Duration {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return 0
	}
	return r.FinishedAt.Sub(*r.StartedAt)
}

// NewRun is the input shape for CreateRun. Fields that are auto-assigned
// (job_id, status='queued', created_at) are filled by the store.
type NewRun struct {
	TaskID         string // required
	StepID         string // required
	MaxCorrections int    // 0 → default 3
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	Statuses []RunStatus
	TaskID   string
	Limit    int
}

// Sentinel errors.
var (
	ErrNotFound      = errors.New("run not found")
	ErrInvalidStatus = errors.New("invalid run status")
	ErrInvalidNewRun = errors.New("invalid new run")
	ErrNotOpen       = errors.New("runstore is not open")
	ErrIDExhausted   = errors.New("job id space exhausted")
)

// Store backs daemon_runs.
type Store struct {
	db *sql.DB
}

// New returns a runstore backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// CreateRun inserts a new daemon_runs row with status='queued' and returns
// the materialised Run. JobID and CreatedAt are auto-assigned.
func (s *Store) CreateRun(ctx context.Context, nr NewRun) (Run, error) {
	if s.db == nil {
		return Run{}, ErrNotOpen
	}
	if err := validateNewRun(nr); err != nil {
		return Run{}, err
	}
	jobID, err := id.NewUniqueN(IDPrefix, IDBytes, func(candidate string) (bool, error) {
		return s.runExists(ctx, candidate)
	})
	if err != nil {
		if errors.Is(err, id.ErrExhausted) {
			return Run{}, ErrIDExhausted
		}
		return Run{}, fmt.Errorf("generate run id: %w", err)
	}
	now := time.Unix(time.Now().Unix(), 0).UTC()
	max := nr.MaxCorrections
	if max <= 0 {
		max = 3
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO daemon_runs(
			job_id, task_id, step_id, status, max_corrections, corrections_used, created_at
		) VALUES (?, ?, ?, 'queued', ?, 0, ?)
	`, jobID, nr.TaskID, nr.StepID, max, now.Unix())
	if err != nil {
		return Run{}, fmt.Errorf("insert daemon_run: %w", err)
	}
	return s.GetRun(ctx, jobID)
}

// GetRun returns the row for jobID, or ErrNotFound.
func (s *Store) GetRun(ctx context.Context, jobID string) (Run, error) {
	if s.db == nil {
		return Run{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, selectRunSQL+` WHERE job_id = ?`, jobID)
	r, err := scanRunRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// ListRuns returns matching rows, ordered by created_at DESC.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	if s.db == nil {
		return nil, ErrNotOpen
	}
	var (
		where []string
		args  []any
	)
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+strings.Join(ph, ",")+")")
	}
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	q := selectRunSQL
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, job_id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query daemon_runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRunning atomically transitions queued → running.
func (s *Store) MarkRunning(ctx context.Context, jobID string, pid int) (Run, error) {
	if s.db == nil {
		return Run{}, ErrNotOpen
	}
	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE daemon_runs
		   SET status = 'running', pid = ?, started_at = ?
		 WHERE job_id = ? AND status = 'queued'
	`, pid, now, jobID)
	if err != nil {
		return Run{}, fmt.Errorf("mark running: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.GetRun(ctx, jobID)
	}
	return s.GetRun(ctx, jobID)
}

// MarkDone transitions running → done with the given exit code and the
// transition id picked by the agent. transitionID may be nil (e.g. the
// engine accepted a closure for reasons unrelated to a transition row).
func (s *Store) MarkDone(ctx context.Context, jobID string, exitCode int, transitionID *int64) (Run, error) {
	return s.markTerminal(ctx, jobID, StatusDone, &exitCode, "", transitionID)
}

// MarkFailed transitions to failed.
func (s *Store) MarkFailed(ctx context.Context, jobID string, exitCode *int, errMsg string) (Run, error) {
	return s.markTerminal(ctx, jobID, StatusFailed, exitCode, errMsg, nil)
}

// MarkCancelled transitions to cancelled.
func (s *Store) MarkCancelled(ctx context.Context, jobID string, exitCode *int) (Run, error) {
	return s.markTerminal(ctx, jobID, StatusCancelled, exitCode, "", nil)
}

func (s *Store) markTerminal(
	ctx context.Context, jobID string, target RunStatus,
	exitCode *int, errMsg string, transitionID *int64,
) (Run, error) {
	if s.db == nil {
		return Run{}, ErrNotOpen
	}
	if !target.IsTerminal() {
		return Run{}, ErrInvalidStatus
	}
	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE daemon_runs
		   SET status        = ?,
		       exit_code     = ?,
		       error         = CASE WHEN ?='' THEN error ELSE ? END,
		       transition_id = COALESCE(?, transition_id),
		       finished_at   = ?,
		       pid           = NULL
		 WHERE job_id = ?
	`, string(target), nullableInt(exitCode),
		errMsg, errMsg,
		nullableInt64(transitionID),
		now, jobID)
	if err != nil {
		return Run{}, fmt.Errorf("mark %s: %w", target, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Run{}, ErrNotFound
	}
	return s.GetRun(ctx, jobID)
}

// SetPID updates the pid column.
func (s *Store) SetPID(ctx context.Context, jobID string, pid int) error {
	if s.db == nil {
		return ErrNotOpen
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE daemon_runs SET pid = ? WHERE job_id = ?`, pid, jobID)
	if err != nil {
		return fmt.Errorf("set pid: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPISession records the pi session id and session.jsonl path.
func (s *Store) SetPISession(ctx context.Context, jobID, sessionID, sessionPath string) error {
	if s.db == nil {
		return ErrNotOpen
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE daemon_runs SET pi_session_id = ?, session_path = ? WHERE job_id = ?`,
		nullableText(sessionID), nullableText(sessionPath), jobID)
	if err != nil {
		return fmt.Errorf("set pi session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// IncCorrections atomically bumps corrections_used by 1.
func (s *Store) IncCorrections(ctx context.Context, jobID string) (int, error) {
	if s.db == nil {
		return 0, ErrNotOpen
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE daemon_runs SET corrections_used = corrections_used + 1 WHERE job_id = ?`, jobID)
	if err != nil {
		return 0, fmt.Errorf("inc corrections: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	var used int
	if err := s.db.QueryRowContext(ctx,
		`SELECT corrections_used FROM daemon_runs WHERE job_id = ?`, jobID).Scan(&used); err != nil {
		return 0, fmt.Errorf("read corrections: %w", err)
	}
	return used, nil
}

// SweepRunningOnStartup rewrites every running row to failed with
// error='daemon_restart'.
func (s *Store) SweepRunningOnStartup(ctx context.Context) (int, error) {
	if s.db == nil {
		return 0, ErrNotOpen
	}
	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE daemon_runs
		   SET status = 'failed',
		       error  = COALESCE(NULLIF(error,''), 'daemon_restart'),
		       finished_at = COALESCE(finished_at, ?),
		       pid    = NULL
		 WHERE status = 'running'
	`, now)
	if err != nil {
		return 0, fmt.Errorf("sweep running: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---- helpers --------------------------------------------------------------

const selectRunSQL = `
	SELECT job_id, task_id, step_id, status, transition_id,
	       exit_code, pid, pi_session_id, session_path, error,
	       max_corrections, corrections_used,
	       created_at, started_at, finished_at
	  FROM daemon_runs`

type scanner interface {
	Scan(dest ...any) error
}

func scanRunRow(sc scanner) (Run, error) {
	var (
		r            Run
		transitionID sql.NullInt64
		exitCode     sql.NullInt64
		pid          sql.NullInt64
		piSessionID  sql.NullString
		sessionPath  sql.NullString
		errStr       sql.NullString
		createdUnix  int64
		startedUnix  sql.NullInt64
		finishedUnix sql.NullInt64
		status       string
	)
	if err := sc.Scan(
		&r.JobID, &r.TaskID, &r.StepID, &status, &transitionID,
		&exitCode, &pid, &piSessionID, &sessionPath, &errStr,
		&r.MaxCorrections, &r.CorrectionsUsed,
		&createdUnix, &startedUnix, &finishedUnix,
	); err != nil {
		return Run{}, err
	}
	r.Status = RunStatus(status)
	if transitionID.Valid {
		v := transitionID.Int64
		r.TransitionID = &v
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		r.ExitCode = &v
	}
	if pid.Valid {
		v := int(pid.Int64)
		r.PID = &v
	}
	r.PISessionID = piSessionID.String
	r.SessionPath = sessionPath.String
	r.Error = errStr.String
	r.CreatedAt = time.Unix(createdUnix, 0).UTC()
	if startedUnix.Valid {
		t := time.Unix(startedUnix.Int64, 0).UTC()
		r.StartedAt = &t
	}
	if finishedUnix.Valid {
		t := time.Unix(finishedUnix.Int64, 0).UTC()
		r.FinishedAt = &t
	}
	return r, nil
}

func (s *Store) runExists(ctx context.Context, jobID string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM daemon_runs WHERE job_id = ?`, jobID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func validateNewRun(nr NewRun) error {
	if nr.TaskID == "" {
		return fmt.Errorf("%w: task_id required", ErrInvalidNewRun)
	}
	if nr.StepID == "" {
		return fmt.Errorf("%w: step_id required", ErrInvalidNewRun)
	}
	return nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
