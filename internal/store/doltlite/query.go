package doltlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/store"
)

// ListTasks returns tasks matching the filter. See ListFilter docs for
// nil/empty semantics.
func (s *Store) ListTasks(ctx context.Context, f store.ListFilter) ([]store.Task, error) {
	if s.db == nil {
		return nil, store.ErrNotOpen
	}

	var (
		where []string
		args  []any
	)

	statuses := defaultStatuses(f.Statuses)
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+strings.Join(placeholders, ",")+")")
	}

	if f.Priority != nil {
		where = append(where, "priority = ?")
		args = append(args, *f.Priority)
	}

	q := `SELECT id, title, description, status, priority, created_at, updated_at
		    FROM tasks`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY priority ASC, created_at ASC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	return s.scanTasks(ctx, q, args...)
}

// Ready returns the ready set. P3 version: ignores edges (status='new' only).
// P4 replaces this with the edge-aware version.
func (s *Store) Ready(ctx context.Context, limit int) ([]store.Task, error) {
	if s.db == nil {
		return nil, store.ErrNotOpen
	}
	q := `SELECT t.id, t.title, t.description, t.status, t.priority, t.created_at, t.updated_at
		    FROM tasks t
		   WHERE t.status = 'new'
		     AND NOT EXISTS (
		         SELECT 1 FROM task_deps d
		           JOIN tasks b ON b.id = d.blocker_id
		          WHERE d.blocked_id = t.id
		            AND b.status IN ('new','claimed'))
		ORDER BY t.priority ASC, t.created_at ASC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return s.scanTasks(ctx, q, args...)
}

// UpdateTask applies a partial update and returns the resulting row.
func (s *Store) UpdateTask(ctx context.Context, idStr string, p store.TaskPatch) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	if p.IsEmpty() {
		// Nothing to do; return current state for ergonomics.
		return s.GetTask(ctx, idStr)
	}
	if err := validatePatch(p); err != nil {
		return store.Task{}, err
	}

	var (
		sets []string
		args []any
	)
	if p.Title != nil {
		title := strings.TrimSpace(*p.Title)
		if title == "" {
			return store.Task{}, store.ErrEmptyTitle
		}
		sets = append(sets, "title = ?")
		args = append(args, title)
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *p.Description)
	}
	if p.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*p.Status))
	}
	if p.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *p.Priority)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Unix())
	args = append(args, idStr)

	q := "UPDATE tasks SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	var res sql.Result
	err := retryOnBusy(ctx, func() error {
		var e error
		res, e = s.db.ExecContext(ctx, q, args...)
		return e
	})
	if err != nil {
		return store.Task{}, fmt.Errorf("update task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either id doesn't exist, or update had no effect (unlikely given we
		// always bump updated_at). Distinguish via GetTask.
		if _, err := s.GetTask(ctx, idStr); err != nil {
			return store.Task{}, err
		}
		return store.Task{}, errors.New("update affected 0 rows")
	}
	return s.GetTask(ctx, idStr)
}

// Claim sets status='claimed' if currently in {new, claimed}. Idempotent.
//
// Returns ErrNotFound if no such task; ErrNotClaimable if status is done or
// cancelled. The returned Task always reflects post-state.
func (s *Store) Claim(ctx context.Context, idStr string) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	now := time.Now().UTC().Unix()
	var res sql.Result
	err := retryOnBusy(ctx, func() error {
		var e error
		res, e = s.db.ExecContext(ctx, `
			UPDATE tasks
			   SET status = 'claimed', updated_at = ?
			 WHERE id = ?
			   AND status IN ('new','claimed')
		`, now, idStr)
		return e
	})
	if err != nil {
		return store.Task{}, fmt.Errorf("claim: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Two cases: not found OR in terminal status. Resolve via GetTask.
		t, gerr := s.GetTask(ctx, idStr)
		if gerr != nil {
			return store.Task{}, gerr // ErrNotFound
		}
		switch t.Status {
		case store.StatusDone, store.StatusCancelled:
			return store.Task{}, store.ErrNotClaimable
		case store.StatusClaimed:
			// Race window: another writer transitioned to claimed before our
			// UPDATE; that's still the idempotent success case.
			return t, nil
		}
		// Anything else is unexpected.
		return store.Task{}, fmt.Errorf("unexpected status after no-op claim: %s", t.Status)
	}
	// We may have updated either a 'new' or already-'claimed' row to 'claimed'.
	return s.GetTask(ctx, idStr)
}

// ---- helpers --------------------------------------------------------------

func (s *Store) scanTasks(ctx context.Context, q string, args ...any) ([]store.Task, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()
	var out []store.Task
	for rows.Next() {
		var (
			t              store.Task
			statusStr      string
			createdU, updU int64
		)
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &statusStr, &t.Priority, &createdU, &updU); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t.Status = store.Status(statusStr)
		t.CreatedAt = time.Unix(createdU, 0).UTC()
		t.UpdatedAt = time.Unix(updU, 0).UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// defaultStatuses applies the "nil = backend default" rule.
// nil  → {new, claimed} (open work).
// []   → no filter (all statuses).
// list → as given.
func defaultStatuses(in []store.Status) []store.Status {
	if in == nil {
		return []store.Status{store.StatusNew, store.StatusClaimed}
	}
	return in
}

func validatePatch(p store.TaskPatch) error {
	if p.Status != nil && !p.Status.Valid() {
		return store.ErrInvalidStatus
	}
	if p.Priority != nil && (*p.Priority < store.MinPriority || *p.Priority > store.MaxPriority) {
		return store.ErrInvalidPriority
	}
	return nil
}

// _ silences unused-import warning if sql is unreferenced (it is referenced
// implicitly via *sql.DB usage in other files).
var _ = sql.ErrNoRows
