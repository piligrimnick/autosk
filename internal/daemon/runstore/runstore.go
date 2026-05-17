// Package runstore is the storage abstraction the autosk daemon uses to
// persist daemon_runs rows.
//
// A Run mirrors the daemon_runs table (see migration 002 + plan §4.1).
// All methods are safe to call after the underlying database has had the
// daemon-runs migration applied. The package does not own the *sql.DB —
// callers (the daemon main) pass the same handle the task Store uses, so
// that writes serialize through the doltlite single-writer connection.
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

// ClosureKind is how the agent closed the autosk task. Set only on success.
type ClosureKind string

const (
	ClosureDone       ClosureKind = "done"
	ClosureCancelled  ClosureKind = "cancelled"
	ClosureDecomposed ClosureKind = "decomposed"
)

// Valid reports whether c is one of the three allowed values (empty is
// allowed in practice, meaning "not set" — callers should special-case).
func (c ClosureKind) Valid() bool {
	switch c {
	case ClosureDone, ClosureCancelled, ClosureDecomposed:
		return true
	}
	return false
}

// ThinkingLevel mirrors pi's --thinking enum. Empty string = pi default.
type ThinkingLevel string

const (
	ThinkingDefault ThinkingLevel = ""
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// Valid reports whether t is one of the allowed enum values.
func (t ThinkingLevel) Valid() bool {
	switch t {
	case ThinkingDefault, ThinkingOff, ThinkingMinimal, ThinkingLow,
		ThinkingMedium, ThinkingHigh, ThinkingXHigh:
		return true
	}
	return false
}

// IDPrefix is the prefix for generated job ids.
const IDPrefix = "job"

// IDBytes is the random byte count for generated job ids
// (3 bytes = 6 hex chars; see plan §4.2).
const IDBytes = 3

// Run is the in-memory representation of a daemon_runs row.
type Run struct {
	JobID         string
	TaskID        string // empty for ad-hoc prompt runs
	Prompt        string
	Model         string
	Thinking      ThinkingLevel
	Cwd           string
	Status        RunStatus
	ExitCode      *int
	PID           *int
	PISessionID   string
	SessionPath   string
	Error         string
	AutoClaim     bool
	MaxCorrections  int
	CorrectionsUsed int
	ClosureKind   ClosureKind // empty until terminal success
	PreBlockedBy  []string
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// Duration is finished_at - started_at when both are set; otherwise 0.
func (r Run) Duration() time.Duration {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return 0
	}
	return r.FinishedAt.Sub(*r.StartedAt)
}

// NewRun is the input shape for CreateRun. Fields that are auto-assigned
// (job_id, status, created_at, defaults) are filled by the store.
type NewRun struct {
	TaskID         string
	Prompt         string
	Model          string
	Thinking       ThinkingLevel
	Cwd            string
	AutoClaim      bool
	MaxCorrections int
	PreBlockedBy   []string // optional snapshot at create time; can be set later
}

// RunFilter narrows ListRuns.
//
// Semantics:
//   - Statuses == nil → no status filter (all).
//   - Statuses == []  → no status filter (all).
//   - TaskID   == ""  → no task filter.
//   - Limit    == 0   → no limit.
type RunFilter struct {
	Statuses []RunStatus
	TaskID   string
	Limit    int
}

// Sentinel errors.
var (
	ErrNotFound       = errors.New("run not found")
	ErrInvalidStatus  = errors.New("invalid run status")
	ErrInvalidNewRun  = errors.New("invalid new run")
	ErrNotOpen        = errors.New("runstore is not open")
	ErrIDExhausted    = errors.New("job id space exhausted")
)

// Store backs daemon_runs.
//
// All writes go through the same *sql.DB the task store uses, so they
// serialize on the doltlite single-writer connection.
type Store struct {
	db *sql.DB
}

// New returns a runstore backed by db. The caller is responsible for
// applying migrations before issuing any Store calls.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// CreateRun inserts a new daemon_runs row with status='queued' and
// returns the materialised Run. JobID and CreatedAt are auto-assigned.
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
	if max < 0 {
		max = 0
	}

	autoClaim := 0
	if nr.AutoClaim {
		autoClaim = 1
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO daemon_runs(
			job_id, task_id, prompt, model, thinking, cwd, status,
			auto_claim, max_corrections, corrections_used,
			pre_blocked_by, created_at
		) VALUES (?,?,?,?,?,?, 'queued',
		          ?, ?, 0,
		          ?, ?)
	`,
		jobID,
		nullableText(nr.TaskID),
		nr.Prompt,
		nr.Model,
		string(nr.Thinking),
		nr.Cwd,
		autoClaim,
		max,
		joinIDs(nr.PreBlockedBy),
		now.Unix(),
	)
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

// MarkRunning atomically transitions queued → running, stamping started_at
// and pid. Returns the post-state. ErrNotFound if no such row.
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
		// Either not found or not in queued; surface via GetRun.
		return s.GetRun(ctx, jobID)
	}
	return s.GetRun(ctx, jobID)
}

// MarkDone transitions running → done with the given exit code and closure.
// closure may be empty for ad-hoc runs (no autosk task).
func (s *Store) MarkDone(ctx context.Context, jobID string, exitCode int, closure ClosureKind) (Run, error) {
	return s.markTerminal(ctx, jobID, StatusDone, &exitCode, "", closure)
}

// MarkFailed transitions to failed with an optional exit code and a
// non-empty failure reason (used as daemon_runs.error).
func (s *Store) MarkFailed(ctx context.Context, jobID string, exitCode *int, errMsg string) (Run, error) {
	return s.markTerminal(ctx, jobID, StatusFailed, exitCode, errMsg, "")
}

// MarkCancelled transitions to cancelled with an optional exit code.
func (s *Store) MarkCancelled(ctx context.Context, jobID string, exitCode *int) (Run, error) {
	return s.markTerminal(ctx, jobID, StatusCancelled, exitCode, "", "")
}

func (s *Store) markTerminal(
	ctx context.Context, jobID string, target RunStatus,
	exitCode *int, errMsg string, closure ClosureKind,
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
		   SET status      = ?,
		       exit_code   = ?,
		       error       = CASE WHEN ?='' THEN error ELSE ? END,
		       closure_kind= CASE WHEN ?='' THEN closure_kind ELSE ? END,
		       finished_at = ?,
		       pid         = NULL
		 WHERE job_id = ?
	`, string(target), nullableInt(exitCode),
		errMsg, errMsg,
		string(closure), string(closure),
		now, jobID)
	if err != nil {
		return Run{}, fmt.Errorf("mark %s: %w", target, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Run{}, ErrNotFound
	}
	return s.GetRun(ctx, jobID)
}

// SetPID updates the pid column without changing status. No-op when the row
// is missing (returns ErrNotFound).
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

// SetPISession records the pi session id and the absolute path to its
// session.jsonl file. Both can be empty if pi didn't return one.
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

// SetPreBlockedBy stores the snapshot of incoming blocker ids taken at the
// start of the run. Replaces any existing snapshot.
func (s *Store) SetPreBlockedBy(ctx context.Context, jobID string, ids []string) error {
	if s.db == nil {
		return ErrNotOpen
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE daemon_runs SET pre_blocked_by = ? WHERE job_id = ?`,
		joinIDs(ids), jobID)
	if err != nil {
		return fmt.Errorf("set pre_blocked_by: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// IncCorrections atomically bumps corrections_used by 1 and returns the new value.
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
// error='daemon_restart'. Used at daemon startup; see plan §4.3.
// Returns the number of rows rewritten.
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
	SELECT job_id, task_id, prompt, model, thinking, cwd, status,
	       exit_code, pid, pi_session_id, session_path, error,
	       auto_claim, max_corrections, corrections_used,
	       closure_kind, pre_blocked_by,
	       created_at, started_at, finished_at
	  FROM daemon_runs`

// scanner abstracts *sql.Row and *sql.Rows for shared scan logic.
type scanner interface {
	Scan(dest ...any) error
}

func scanRunRow(sc scanner) (Run, error) {
	var (
		r              Run
		taskID         sql.NullString
		exitCode       sql.NullInt64
		pid            sql.NullInt64
		piSessionID    sql.NullString
		sessionPath    sql.NullString
		errStr         sql.NullString
		autoClaim      int
		closureKind    sql.NullString
		preBlockedBy   string
		createdUnix    int64
		startedUnix    sql.NullInt64
		finishedUnix   sql.NullInt64
		status, think  string
	)
	if err := sc.Scan(
		&r.JobID, &taskID, &r.Prompt, &r.Model, &think, &r.Cwd, &status,
		&exitCode, &pid, &piSessionID, &sessionPath, &errStr,
		&autoClaim, &r.MaxCorrections, &r.CorrectionsUsed,
		&closureKind, &preBlockedBy,
		&createdUnix, &startedUnix, &finishedUnix,
	); err != nil {
		return Run{}, err
	}
	r.TaskID = taskID.String
	r.Thinking = ThinkingLevel(think)
	r.Status = RunStatus(status)
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
	r.AutoClaim = autoClaim != 0
	if closureKind.Valid {
		r.ClosureKind = ClosureKind(closureKind.String)
	}
	r.PreBlockedBy = splitIDs(preBlockedBy)
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
	if nr.Prompt == "" {
		return fmt.Errorf("%w: prompt required", ErrInvalidNewRun)
	}
	if nr.Cwd == "" {
		return fmt.Errorf("%w: cwd required", ErrInvalidNewRun)
	}
	if !nr.Thinking.Valid() {
		return fmt.Errorf("%w: invalid thinking %q", ErrInvalidNewRun, nr.Thinking)
	}
	return nil
}

// nullableText returns sql.NullString from a Go string ("" → NULL).
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt converts *int → any (NULL when nil).
func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return strings.Join(ids, ",")
}

func splitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
