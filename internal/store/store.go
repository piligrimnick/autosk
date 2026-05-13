// Package store defines the storage abstraction autosk's commands sit on top
// of. Two implementations live alongside: doltlite (default, MVP) and
// doltserver (build-tagged stub).
package store

import (
	"context"
	"time"
)

// Status is the lifecycle state of a task.
//
// Transitions are unconstrained in v0.1 (any → any via UpdateTask), with the
// exception of Claim which enforces "new|claimed → claimed" and Reopen
// (in the CLI layer) which enforces "done|cancelled → new".
//
// "blocked" is NOT a stored status. A task is blocked iff it has at least
// one open blocker edge whose blocker is in StatusNew or StatusClaimed; this
// is computed by Ready and by GetTask consumers.
type Status string

const (
	StatusNew       Status = "new"
	StatusClaimed   Status = "claimed"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
)

// Valid reports whether s is one of the four allowed values.
func (s Status) Valid() bool {
	switch s {
	case StatusNew, StatusClaimed, StatusDone, StatusCancelled:
		return true
	}
	return false
}

// AllStatuses returns the enum in canonical order.
func AllStatuses() []Status {
	return []Status{StatusNew, StatusClaimed, StatusDone, StatusCancelled}
}

// MinPriority and MaxPriority bound the priority range (0 = highest).
const (
	MinPriority     = 0
	MaxPriority     = 3
	DefaultPriority = 2
)

// Task is the core domain object.
type Task struct {
	ID          string
	Title       string
	Description string
	Status      Status
	Priority    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TaskPatch is a partial update. Nil fields are left unchanged.
type TaskPatch struct {
	Title       *string
	Description *string
	Status      *Status
	Priority    *int
}

// IsEmpty reports whether the patch would change nothing.
func (p TaskPatch) IsEmpty() bool {
	return p.Title == nil && p.Description == nil && p.Status == nil && p.Priority == nil
}

// ListFilter narrows ListTasks results.
//
// Semantics:
//   - Statuses == nil  → backend default ({new, claimed} — open work).
//   - Statuses == []   → no filter (all statuses).
//   - Priority == nil  → no priority filter.
//   - Limit  ==  0     → backend default (typically unlimited or a sane cap).
type ListFilter struct {
	Statuses []Status
	Priority *int
	Limit    int
}

// Store is the storage abstraction. Every backend implements this interface.
//
// All methods are safe to call after a successful Open; behavior before Open
// or after Close is implementation-defined (usually returns an error).
type Store interface {
	// Lifecycle.
	Open(ctx context.Context, dbPath string) error
	Close() error
	Migrate(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)

	// Task CRUD.
	CreateTask(ctx context.Context, t Task) (Task, error)
	GetTask(ctx context.Context, id string) (Task, error)
	UpdateTask(ctx context.Context, id string, p TaskPatch) (Task, error)
	ListTasks(ctx context.Context, f ListFilter) ([]Task, error)

	// Claim is an atomic conditional update: new|claimed → claimed.
	// Returns ErrNotClaimable if the task is in done or cancelled.
	Claim(ctx context.Context, id string) (Task, error)

	// Edges. Variadic; runs in a single transaction; rejects self-block and
	// cycles. Block is idempotent (re-adding an existing edge is a no-op).
	Block(ctx context.Context, id string, blockers ...string) error
	Unblock(ctx context.Context, id string, blockers ...string) error
	UnblockAll(ctx context.Context, id string) (removed int, err error)

	// Deps returns the ids of tasks that block id (incoming) and the ids
	// of tasks that id blocks (outgoing).
	Deps(ctx context.Context, id string) (incoming, outgoing []string, err error)

	// IsBlocked is the derived `blocked` flag: true iff id has at least one
	// incoming blocker edge whose blocker's status is in {new, claimed}.
	IsBlocked(ctx context.Context, id string) (bool, error)

	// Ready returns tasks where status='new' AND no open blocker (open =
	// blocker in {new, claimed}). Sorted priority ASC, created_at ASC.
	Ready(ctx context.Context, limit int) ([]Task, error)

	// Raw passthrough for `autosk sql`. Implementations may refuse writes
	// when readOnly is true; "writes" is interpreted dialect-specifically.
	QueryRaw(ctx context.Context, q string, args ...any) (Rows, error)
	ExecRaw(ctx context.Context, q string, args ...any) (Result, error)
}

// Rows is a minimal subset of database/sql.Rows so the interface stays portable.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Err() error
	Close() error
}

// Result mirrors database/sql.Result.
type Result interface {
	LastInsertId() (int64, error)
	RowsAffected() (int64, error)
}
