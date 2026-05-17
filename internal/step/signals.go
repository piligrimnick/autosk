// Package step writes step_signals rows — the structural transition
// signal that closes a workflow run (plan §5.4).
//
// One row per run; PK on run_id enforces "exactly one signal per run".
// A second `step next` call within the same run returns
// ErrAlreadyEmitted. The daemon (W6/W8) reads this table after the
// agent's end-of-turn and advances the task accordingly.
package step

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/daemon/runstore"
)

// Sentinel errors.
var (
	ErrNotOpen        = errors.New("step signals: not open")
	ErrNoActiveRun    = errors.New("no active run for this task")
	ErrUnknownTarget  = errors.New("target not in current step's transitions")
	ErrAlreadyEmitted = errors.New("step_next_already_emitted")
)

// Emitted is the materialised signal row, returned by Emit for echoing
// back to the agent / CLI.
type Emitted struct {
	RunID        string
	TaskID       string
	TransitionID int64
	NextStepName string // empty when TaskStatus is set
	TaskStatus   string // empty when NextStepName is set
	PromptRule   string
	CreatedAt    time.Time
}

// Store wraps step_signals.
type Store struct {
	db *sql.DB
}

// New constructs a Store.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Emit resolves the active run for taskID, validates target against the
// run's current step's outgoing transitions, and records the chosen
// transition in step_signals. target is either a sibling step name or
// one of {done, cancelled, human_feedback}.
//
// Errors:
//   - ErrNoActiveRun     — no daemon_runs row in status='running' for this task.
//   - ErrUnknownTarget   — target doesn't match any outgoing transition.
//   - ErrAlreadyEmitted  — `step next` was already called for this run.
func (s *Store) Emit(ctx context.Context, taskID, target string) (Emitted, error) {
	if s.db == nil {
		return Emitted{}, ErrNotOpen
	}
	target = strings.TrimSpace(target)
	if taskID == "" || target == "" {
		return Emitted{}, fmt.Errorf("emit: taskID and target are required")
	}

	// 1. Active run.
	var (
		runID  string
		stepID string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT job_id, step_id
		  FROM daemon_runs
		 WHERE task_id = ? AND status = ?
		 ORDER BY created_at DESC, job_id DESC
		 LIMIT 1
	`, taskID, string(runstore.StatusRunning)).Scan(&runID, &stepID)
	if errors.Is(err, sql.ErrNoRows) {
		return Emitted{}, fmt.Errorf("%w: %s", ErrNoActiveRun, taskID)
	}
	if err != nil {
		return Emitted{}, fmt.Errorf("find active run: %w", err)
	}

	// 2. Resolve target → transition_id by joining transitions to (optional)
	// next-step-name. We match either next_step.name = target OR
	// task_status = target. Exactly one match required.
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.next_step_id, t.task_status, t.prompt_rule,
		       COALESCE((SELECT name FROM steps WHERE id = t.next_step_id), '')
		  FROM step_transitions t
		 WHERE t.step_id = ?
	`, stepID)
	if err != nil {
		return Emitted{}, fmt.Errorf("list transitions: %w", err)
	}
	defer rows.Close()
	var (
		all   []cand
		picks []cand
	)
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.nextStepID, &c.taskStatus, &c.promptRule, &c.nextStepName); err != nil {
			return Emitted{}, fmt.Errorf("scan transition: %w", err)
		}
		all = append(all, c)
		if (c.taskStatus.Valid && c.taskStatus.String == target) || (c.nextStepName != "" && c.nextStepName == target) {
			picks = append(picks, c)
		}
	}
	if err := rows.Err(); err != nil {
		return Emitted{}, err
	}
	if len(picks) == 0 {
		return Emitted{}, fmt.Errorf("%w: %q (valid: %s)", ErrUnknownTarget, target, describeTargets(all))
	}
	if len(picks) > 1 {
		return Emitted{}, fmt.Errorf("ambiguous target %q (matched %d transitions)", target, len(picks))
	}
	pick := picks[0]

	// 3. Insert signal. PK(run_id) enforces "exactly one per run".
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO step_signals(run_id, task_id, transition_id, created_at)
		VALUES (?, ?, ?, ?)
	`, runID, taskID, pick.id, now); err != nil {
		if isUniqueErr(err, "step_signals.run_id") || strings.Contains(err.Error(), "UNIQUE constraint failed: step_signals.run_id") {
			return Emitted{}, fmt.Errorf("%w (run=%s)", ErrAlreadyEmitted, runID)
		}
		// SQLite reports the PK violation slightly differently for INTEGER PK
		// columns vs unique indexes; cover both.
		if strings.Contains(err.Error(), "PRIMARY KEY") {
			return Emitted{}, fmt.Errorf("%w (run=%s)", ErrAlreadyEmitted, runID)
		}
		return Emitted{}, fmt.Errorf("insert step_signal: %w", err)
	}
	return Emitted{
		RunID:        runID,
		TaskID:       taskID,
		TransitionID: pick.id,
		NextStepName: pick.nextStepName,
		TaskStatus:   pick.taskStatus.String,
		PromptRule:   pick.promptRule,
		CreatedAt:    time.Unix(now, 0).UTC(),
	}, nil
}

// GetForRun returns the signal recorded for the given run, or ErrNoActiveRun
// when none exists. Used by the executor (W6) after end-of-turn.
func (s *Store) GetForRun(ctx context.Context, runID string) (Emitted, error) {
	if s.db == nil {
		return Emitted{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT ss.run_id, ss.task_id, ss.transition_id, ss.created_at,
		       t.task_status,
		       COALESCE((SELECT name FROM steps WHERE id = t.next_step_id), '') AS next_name,
		       t.prompt_rule
		  FROM step_signals ss
		  JOIN step_transitions t ON t.id = ss.transition_id
		 WHERE ss.run_id = ?
	`, runID)
	var (
		e          Emitted
		taskStatus sql.NullString
		createdU   int64
	)
	if err := row.Scan(&e.RunID, &e.TaskID, &e.TransitionID, &createdU, &taskStatus, &e.NextStepName, &e.PromptRule); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Emitted{}, ErrNoActiveRun
		}
		return Emitted{}, fmt.Errorf("get signal: %w", err)
	}
	e.TaskStatus = taskStatus.String
	e.CreatedAt = time.Unix(createdU, 0).UTC()
	return e, nil
}

// cand is one candidate transition while resolving `--to`.
type cand struct {
	id           int64
	nextStepID   sql.NullString
	taskStatus   sql.NullString
	promptRule   string
	nextStepName string
}

// describeTargets returns a human-friendly comma-separated list of valid
// targets for the current step.
func describeTargets(all []cand) string {
	parts := make([]string, 0, len(all))
	for _, c := range all {
		if c.taskStatus.Valid && c.taskStatus.String != "" {
			parts = append(parts, c.taskStatus.String)
		} else if c.nextStepName != "" {
			parts = append(parts, c.nextStepName)
		}
	}
	return strings.Join(parts, ", ")
}

func isUniqueErr(err error, indexKey string) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed: "+indexKey)
}
