// Package comments owns the `comments` table.
//
// Comments are immutable, timestamped notes attached to a task. They are
// the cross-agent / human↔agent channel for a workflow: the step
// executor surfaces all previous comments at the top of every step's
// prompt (plan §5.7).
//
// v1: insert + list, no edit/delete. If we need those later, we'll add
// them — but the prompt-rendering protocol benefits from a strict
// "append-only ledger" feel.
package comments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors.
var (
	ErrNotOpen    = errors.New("comments store: not open")
	ErrEmptyText  = errors.New("comment text is empty")
	ErrInvalidFK  = errors.New("invalid task_id or author_id")
)

// Comment is one `comments` row plus the joined-in author name (cheap
// extra column from the JOIN; nice for rendering without a second query).
type Comment struct {
	ID         int64
	TaskID     string
	AuthorID   string
	AuthorName string
	Text       string
	CreatedAt  time.Time
}

// Store backs the `comments` table on the shared *sql.DB.
type Store struct {
	db *sql.DB
}

// New constructs a Store.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Add inserts a new comment. Returns the materialised Comment (with the
// new id and timestamp). Empty text is rejected.
func (s *Store) Add(ctx context.Context, taskID, authorID, text string) (Comment, error) {
	if s.db == nil {
		return Comment{}, ErrNotOpen
	}
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return Comment{}, ErrEmptyText
	}
	if taskID == "" || authorID == "" {
		return Comment{}, fmt.Errorf("%w: task_id=%q author_id=%q", ErrInvalidFK, taskID, authorID)
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO comments(task_id, author_id, text, created_at) VALUES (?, ?, ?, ?)`,
		taskID, authorID, text, now)
	if err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}
	cid, err := res.LastInsertId()
	if err != nil {
		return Comment{}, fmt.Errorf("last id: %w", err)
	}
	return s.GetByID(ctx, cid)
}

// GetByID returns the comment with the given id.
func (s *Store) GetByID(ctx context.Context, id int64) (Comment, error) {
	if s.db == nil {
		return Comment{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, selectCommentSQL+` WHERE c.id = ?`, id)
	return scanComment(row)
}

// ListByTask returns all comments for a task, oldest first.
func (s *Store) ListByTask(ctx context.Context, taskID string) ([]Comment, error) {
	if s.db == nil {
		return nil, ErrNotOpen
	}
	rows, err := s.db.QueryContext(ctx,
		selectCommentSQL+` WHERE c.task_id = ? ORDER BY c.created_at ASC, c.id ASC`,
		taskID)
	if err != nil {
		return nil, fmt.Errorf("query comments: %w", err)
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RenderForPrompt formats a task's comments as
// `[<author>@<RFC3339>]: <text>` lines, oldest first. Returned slice may
// be empty. Used by the step executor (W6) to embed the comment ledger
// in each spawn's prompt (plan §5.7).
func (s *Store) RenderForPrompt(ctx context.Context, taskID string) ([]string, error) {
	cs, err := s.ListByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, fmt.Sprintf("[%s@%s]: %s",
			c.AuthorName, c.CreatedAt.Format(time.RFC3339), c.Text))
	}
	return out, nil
}

// ---- internals ------------------------------------------------------------

// selectCommentSQL joins the agent name in so RenderForPrompt doesn't
// need a second query per comment.
const selectCommentSQL = `
	SELECT c.id, c.task_id, c.author_id, a.name, c.text, c.created_at
	  FROM comments c
	  JOIN agents a ON c.author_id = a.id`

type scanner interface {
	Scan(dest ...any) error
}

func scanComment(sc scanner) (Comment, error) {
	var (
		c          Comment
		createdU   int64
	)
	if err := sc.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.AuthorName, &c.Text, &createdU); err != nil {
		return Comment{}, err
	}
	c.CreatedAt = time.Unix(createdU, 0).UTC()
	return c, nil
}
