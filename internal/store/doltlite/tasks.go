package doltlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/id"
	"autosk/internal/store"
)

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
	}
	// Persist at second precision; return the same so callers see what the DB sees.
	now := time.Unix(time.Now().Unix(), 0).UTC()
	t.CreatedAt = now
	t.UpdatedAt = now

	err := retryOnBusy(ctx, func() error {
		_, e := s.db.ExecContext(ctx, `
			INSERT INTO tasks(id, title, description, status, priority, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, t.ID, t.Title, t.Description, string(t.Status), t.Priority, now.Unix(), now.Unix())
		return e
	})
	if err != nil {
		return store.Task{}, fmt.Errorf("insert task: %w", err)
	}
	return t, nil
}

// GetTask returns the task with the given id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, idStr string) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, title, description, status, priority, created_at, updated_at
		  FROM tasks WHERE id = ?
	`, idStr)
	var (
		t                store.Task
		statusStr        string
		createdU, updU   int64
	)
	if err := row.Scan(&t.ID, &t.Title, &t.Description, &statusStr, &t.Priority, &createdU, &updU); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Task{}, store.ErrNotFound
		}
		return store.Task{}, fmt.Errorf("select task: %w", err)
	}
	t.Status = store.Status(statusStr)
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
// Mutates t to fill in defaults.
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
	if t.Priority == 0 && false {
		// Reserved: do not silently default 0 since 0 is a valid (highest) priority.
	}
	// Priority defaults to MaxPriority/2 (==2) when zero-value AND title omitted? No —
	// the caller controls priority. The CLI layer applies DefaultPriority if no flag is given.
	if t.Priority < store.MinPriority || t.Priority > store.MaxPriority {
		return store.ErrInvalidPriority
	}
	return nil
}
