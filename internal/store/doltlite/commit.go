package doltlite

import (
	"context"
	"fmt"

	"autosk/internal/sqlretry"
)

// DoltCommit creates a dolt commit with the given message, recording every
// modified row in the prolly tree. Best-effort: any error is returned but
// callers may want to ignore it (e.g., if the change set is empty).
//
// We use `SELECT dolt_commit(...)` which is doltlite's function form.
// `-A` stages all changes, `--allow-empty` would let us commit nothing —
// we omit it so empty commits silently fail upstream and we ignore them.
func (s *Store) DoltCommit(ctx context.Context, msg string) error {
	if s.db == nil {
		return nil
	}
	return sqlretry.OnBusy(ctx, func() error {
		row := s.db.QueryRowContext(ctx, `SELECT dolt_commit('-A', '-m', ?)`, msg)
		var hash string
		if err := row.Scan(&hash); err != nil {
			return fmt.Errorf("dolt_commit: %w", err)
		}
		return nil
	})
}
