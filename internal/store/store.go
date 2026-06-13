// Package store defines the shared task view types the autosk front ends render
// against. v2 has no database in the Go tree — the daemon (autoskd) owns all
// storage; the CLI + lazy TUI consume these types over JSON-RPC and never open
// a store. This package is now just the status enum + the rendered task shape.
package store

import "time"

// Status is the lifecycle state of a task. The five-status enum is unchanged
// from v1; "blocked" is NOT a stored status (it is a derived flag).
type Status string

const (
	StatusNew    Status = "new"
	StatusWork   Status = "work"
	StatusHuman  Status = "human"
	StatusDone   Status = "done"
	StatusCancel Status = "cancel"
)

// Valid reports whether s is one of the five allowed values.
func (s Status) Valid() bool {
	switch s {
	case StatusNew, StatusWork, StatusHuman, StatusDone, StatusCancel:
		return true
	}
	return false
}

// AllStatuses returns the enum in canonical order.
func AllStatuses() []Status {
	return []Status{StatusNew, StatusWork, StatusHuman, StatusDone, StatusCancel}
}

// OpenStatuses returns the statuses that count as "open work" — the default
// filter for `autosk list` and the set that keeps a task blocking its
// dependents.
func OpenStatuses() []Status {
	return []Status{StatusNew, StatusWork, StatusHuman}
}

// Task is the rendered task view (v2 shape). v2 drops priority, author, and
// metadata; `workflow`/`step` carry the names (empty when the task is not
// enrolled). Derived fields (blocked / blocked_by / blocks / comment_count) are
// layered on by the caller via render Options.
type Task struct {
	ID          string
	Title       string
	Description string
	Status      Status
	Workflow    string // workflow name ("" when not enrolled)
	Step        string // current step name ("" when not enrolled)
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
