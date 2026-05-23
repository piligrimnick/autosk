package doltlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"autosk/internal/id"
	"autosk/internal/sqlretry"
	"autosk/internal/store"
)

// taskIDShape pins the v0.2-post-007 task-id format: `ask-` followed
// by exactly 6 lowercase hex chars. It is strictly narrower than
// `id.Valid` (which still accepts the 4-hex agent-id shape and any
// even-width hex suffix) because the create path mints task ids and
// must reject anything that doesn't match the canonical task layout.
var taskIDShape = regexp.MustCompile(`^ask-[0-9a-f]{6}$`)

// assertTaskIDShape rejects caller-supplied ids that don't match the
// `ask-XXXXXX` task-id shape. The generator path (`id.NewUnique`)
// already produces the right shape, so this is purely defensive against
// callers that pre-populate `Task.ID` (the rollback path in
// `autosk create`, some tests, future RPC clients).
func assertTaskIDShape(idStr string) error {
	if !taskIDShape.MatchString(idStr) {
		return fmt.Errorf("%w: task id %q does not match canonical shape `ask-` + 6 lowercase hex chars",
			store.ErrInvalidShape, idStr)
	}
	return nil
}

// CreateTask inserts a new task. If t.ID is empty, a fresh unique id is
// generated. created_at / updated_at are stamped here.
func (s *Store) CreateTask(ctx context.Context, t store.Task) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	if err := validateForCreate(&t); err != nil {
		return store.Task{}, err
	}
	if t.ID == "" {
		newID, err := id.NewUnique(id.DefaultPrefix, func(candidate string) (bool, error) {
			return s.taskExists(ctx, candidate)
		})
		if err != nil {
			return store.Task{}, fmt.Errorf("generate id: %w", err)
		}
		t.ID = newID
	} else if err := assertTaskIDShape(t.ID); err != nil {
		return store.Task{}, err
	}
	now := time.Unix(time.Now().Unix(), 0).UTC()
	t.CreatedAt = now
	t.UpdatedAt = now

	metaArg, err := marshalMetadata(t.Metadata)
	if err != nil {
		return store.Task{}, err
	}
	err = sqlretry.OnBusy(ctx, func() error {
		_, e := s.db.ExecContext(ctx, `
			INSERT INTO tasks(id, title, description, status, priority,
			                  author_id, workflow_id, current_step_id,
			                  metadata,
			                  created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, t.ID, t.Title, t.Description, string(t.Status), t.Priority,
			nullText(t.AuthorID), nullText(t.WorkflowID), nullText(t.CurrentStepID),
			metaArg,
			now.Unix(), now.Unix())
		return e
	})
	if err != nil {
		return store.Task{}, fmt.Errorf("insert task: %w", err)
	}
	return t, nil
}

// marshalMetadata converts an in-memory metadata map into the SQL
// argument used for tasks.metadata. nil / empty maps collapse to SQL
// NULL so the column round-trips cleanly through CreateTask + UpdateTask.
func marshalMetadata(m map[string]any) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	return string(b), nil
}

// unmarshalMetadata is the inverse of marshalMetadata. NULL / empty
// strings return nil so callers see the zero value uniformly.
func unmarshalMetadata(raw sql.NullString) (map[string]any, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return m, nil
}

// nullText converts a possibly-empty Go string into a SQL NULL or text
// value for use as a parameter. Mirrors the convention in daemon/runstore.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DeleteTask removes a task row by id. Returns ErrNotFound when no row
// matched. Used by the worktree-isolation rollback path in `autosk
// create` — see store.Store.DeleteTask for the broader contract.
//
// FK CASCADE handles task_deps, comments, daemon_runs and
// (transitively through daemon_runs) step_signals. Callers do NOT
// need to reap those rows first.
func (s *Store) DeleteTask(ctx context.Context, idStr string) error {
	if s.db == nil {
		return store.ErrNotOpen
	}
	var (
		res store.Result
		err error
	)
	err = sqlretry.OnBusy(ctx, func() error {
		res, err = s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, idStr)
		return err
	})
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetTask returns the task with the given id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, idStr string) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, title, description, status, priority,
		       author_id, workflow_id, current_step_id,
		       metadata,
		       created_at, updated_at
		  FROM tasks WHERE id = ?
	`, idStr)
	t, err := scanOneTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Task{}, store.ErrNotFound
	}
	if err != nil {
		return store.Task{}, fmt.Errorf("select task: %w", err)
	}
	return t, nil
}

// scanOneTask scans a single tasks-row from sql.Row or sql.Rows.
func scanOneTask(sc interface{ Scan(...any) error }) (store.Task, error) {
	var (
		t              store.Task
		statusStr      string
		authorID       sql.NullString
		workflowID     sql.NullString
		currentStepID  sql.NullString
		metadataRaw    sql.NullString
		createdU, updU int64
	)
	if err := sc.Scan(
		&t.ID, &t.Title, &t.Description, &statusStr, &t.Priority,
		&authorID, &workflowID, &currentStepID,
		&metadataRaw,
		&createdU, &updU,
	); err != nil {
		return store.Task{}, err
	}
	t.Status = store.Status(statusStr)
	t.AuthorID = authorID.String
	t.WorkflowID = workflowID.String
	t.CurrentStepID = currentStepID.String
	md, err := unmarshalMetadata(metadataRaw)
	if err != nil {
		return store.Task{}, err
	}
	t.Metadata = md
	t.CreatedAt = time.Unix(createdU, 0).UTC()
	t.UpdatedAt = time.Unix(updU, 0).UTC()
	return t, nil
}

func (s *Store) taskExists(ctx context.Context, idStr string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, idStr).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// validateForCreate normalizes and validates a Task before insert.
// Mutates t to fill in defaults. Enforces the v0.2 invariant that ties
// status to current_step_id (also enforced by SQL CHECK, but failing
// fast in Go gives a clearer error).
func validateForCreate(t *store.Task) error {
	t.Title = strings.TrimSpace(t.Title)
	if t.Title == "" {
		return store.ErrEmptyTitle
	}
	if t.Status == "" {
		t.Status = store.StatusNew
	}
	if !t.Status.Valid() {
		return store.ErrInvalidStatus
	}
	if t.Priority < store.MinPriority || t.Priority > store.MaxPriority {
		return store.ErrInvalidPriority
	}
	switch t.Status {
	case store.StatusWork:
		if t.CurrentStepID == "" || t.WorkflowID == "" {
			return fmt.Errorf("%w: status=work requires workflow_id and current_step_id",
				store.ErrInvalidStatus)
		}
	case store.StatusNew, store.StatusDone, store.StatusCancel:
		if t.CurrentStepID != "" {
			return fmt.Errorf("%w: status=%s must have current_step_id cleared",
				store.ErrInvalidStatus, t.Status)
		}
	}
	return nil
}
