package doltlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/sqlretry"
	"autosk/internal/store"
)

// UpdateMetadata applies fn to the task's current metadata in a single
// transaction and persists the result. Returns the new map along with a
// `changed` flag that is true iff the marshalled metadata actually
// differs from what was on disk before fn ran. When changed is false,
// the SQL UPDATE is skipped so updated_at and the dolt commit log stay
// quiet; callers (e.g. `metadata unset` of a missing key) use the flag
// to decide whether to record an audit commit.
//
// Concurrency: doltlite is configured with SetMaxOpenConns(1) (a
// single-connection write lane). The SQL transaction itself is
// `BEGIN DEFERRED` (the database/sql default), but two callers can
// never actually race because they serialise behind the same Go-side
// connection. The contract handed to fn is therefore: you observe a
// consistent snapshot, and your post-fn write is the next on-disk
// state.
//
// fn ALWAYS receives a non-nil map. Empty maps after fn collapse to
// SQL NULL on the metadata column (see marshalMetadata). The returned
// map matches what was persisted.
func (s *Store) UpdateMetadata(ctx context.Context, taskID string, fn func(m map[string]any) error) (map[string]any, bool, error) {
	if s.db == nil {
		return nil, false, store.ErrNotOpen
	}
	if fn == nil {
		return nil, false, errors.New("UpdateMetadata: fn is nil")
	}

	var (
		result  map[string]any
		changed bool
	)
	err := sqlretry.OnBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		var raw sql.NullString
		err = tx.QueryRowContext(ctx,
			`SELECT metadata FROM tasks WHERE id = ?`, taskID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("select metadata: %w", err)
		}

		current, err := unmarshalMetadata(raw)
		if err != nil {
			return err
		}
		if current == nil {
			current = map[string]any{}
		}
		// Snapshot the pre-fn state via its canonical marshalled form
		// so we can diff against the post-fn marshal below. json.Marshal
		// sorts map keys alphabetically, so equal maps produce equal
		// bytes.
		preBytes, err := canonicalMetadataBytes(current)
		if err != nil {
			return err
		}
		if err := fn(current); err != nil {
			return err
		}
		postBytes, err := canonicalMetadataBytes(current)
		if err != nil {
			return err
		}
		if bytes.Equal(preBytes, postBytes) {
			// No-op: skip the UPDATE and the updated_at bump entirely.
			// The tx rolls back via the defer; nothing was written.
			result = mapOrNil(current)
			changed = false
			return nil
		}

		arg, err := marshalMetadata(current)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Unix()
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET metadata = ?, updated_at = ? WHERE id = ?`,
			arg, now, taskID)
		if err != nil {
			return fmt.Errorf("update metadata: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return store.ErrNotFound
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		result = mapOrNil(current)
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, changed, nil
}

// UpdateMetadataAndPatch is the engine-side atomic helper: it reads
// tasks.metadata, hands fn a copy to mutate, then writes the new
// metadata AND the supplied TaskPatch in the same transaction. Used by
// workflow.EnterStep so the step_visits counter bump and the task
// pointer update (workflow_id / current_step_id / status) land or fail
// together.
//
// p.Metadata must be nil — pass metadata mutations through fn instead.
// Returns the resulting Task (re-read after commit).
//
// Concurrency: same single-connection write lane as UpdateMetadata.
func (s *Store) UpdateMetadataAndPatch(ctx context.Context, taskID string, fn func(m map[string]any) error, p store.TaskPatch) (store.Task, error) {
	if s.db == nil {
		return store.Task{}, store.ErrNotOpen
	}
	if fn == nil {
		return store.Task{}, errors.New("UpdateMetadataAndPatch: fn is nil")
	}
	if p.Metadata != nil {
		return store.Task{}, errors.New("UpdateMetadataAndPatch: patch.Metadata must be nil; route metadata through fn")
	}
	if err := validatePatch(p); err != nil {
		return store.Task{}, err
	}

	err := sqlretry.OnBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// 1. Read current metadata under the tx.
		var raw sql.NullString
		err = tx.QueryRowContext(ctx,
			`SELECT metadata FROM tasks WHERE id = ?`, taskID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("select metadata: %w", err)
		}
		current, err := unmarshalMetadata(raw)
		if err != nil {
			return err
		}
		if current == nil {
			current = map[string]any{}
		}

		// 2. Mutate.
		if err := fn(current); err != nil {
			return err
		}

		// 3. Build SET clause from patch + the new metadata.
		sets, args, perr := patchSetsAndArgs(p)
		if perr != nil {
			return perr
		}
		metaArg, merr := marshalMetadata(current)
		if merr != nil {
			return merr
		}
		sets = append(sets, "metadata = ?")
		args = append(args, metaArg)
		sets = append(sets, "updated_at = ?")
		args = append(args, time.Now().UTC().Unix())
		args = append(args, taskID)

		q := "UPDATE tasks SET " + strings.Join(sets, ", ") + " WHERE id = ?"
		res, eerr := tx.ExecContext(ctx, q, args...)
		if eerr != nil {
			return fmt.Errorf("update task: %w", eerr)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return store.ErrNotFound
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("commit: %w", cerr)
		}
		return nil
	})
	if err != nil {
		return store.Task{}, err
	}
	// Reload the row after commit so callers see the persisted shape.
	return s.GetTask(ctx, taskID)
}

// mapOrNil returns nil for empty maps so callers see the same "{} ==
// nil" surface the on-disk NULL column produces.
func mapOrNil(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// canonicalMetadataBytes marshals m with sorted keys (json.Marshal's
// default for map[string]any). Returns nil for empty maps so the empty
// state has a unique pre-image.
func canonicalMetadataBytes(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := marshalMetadata(m)
	if err != nil {
		return nil, err
	}
	if s, ok := b.(string); ok {
		return []byte(s), nil
	}
	return nil, nil
}
