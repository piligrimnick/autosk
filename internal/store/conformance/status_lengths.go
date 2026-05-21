package conformance

import (
	"testing"

	"autosk/internal/daemon/runstore"
	"autosk/internal/store"
)

// MaxStatusLen is the budget the lazy TUI's status column reserves
// and the cap docs/plans/20260521-Short-Statuses.md commits to.
//
// Every value across `tasks.status`, `step_transitions.task_status`
// and `daemon_runs.status` must fit in this many cells so the column
// renders without ellipsis truncation and CLI listings stay tidy.
const MaxStatusLen = 7

// AssertStatusLengths is a static guard: each value in the task and
// run-status enums must be at most MaxStatusLen characters long.
// Invoked from RunConformance so every backend test surfaces a
// regression immediately. A future addition of a longer status value
// (e.g. "deferred", 8 chars) trips this without needing a new SQL
// migration test -- the Go-level guard fires first.
//
// Both loops drive off the package-level AllStatuses helpers so a new
// status value (e.g. a future runstore.StatusPaused) is length-checked
// automatically; nothing here knows the enum cardinality.
func AssertStatusLengths(t *testing.T) {
	t.Helper()
	for _, s := range store.AllStatuses() {
		if l := len(string(s)); l > MaxStatusLen {
			t.Errorf("store.Status %q is %d chars; cap is %d", s, l, MaxStatusLen)
		}
	}
	for _, s := range runstore.AllStatuses() {
		if l := len(string(s)); l > MaxStatusLen {
			t.Errorf("runstore.RunStatus %q is %d chars; cap is %d", s, l, MaxStatusLen)
		}
	}
}
