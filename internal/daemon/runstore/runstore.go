// Package runstore holds the session status enum shared by the Go CLI + lazy
// TUI.
//
// Sessions (one agent onRun for one task step) are owned by the daemon
// (autoskd); the Go front ends only ever consume session status values that
// arrive over JSON-RPC. v2 renamed the v1 daemon_runs "cancel" terminal to
// "aborted".
package runstore

// RunStatus is the lifecycle state of a session (queued|running|done|failed|
// aborted). Mirrors api.SessionStatus.
type RunStatus string

const (
	StatusQueued  RunStatus = "queued"
	StatusRunning RunStatus = "running"
	StatusDone    RunStatus = "done"
	StatusFailed  RunStatus = "failed"
	StatusAborted RunStatus = "aborted"
)

// IsTerminal reports whether s is a sticky terminal status.
func (s RunStatus) IsTerminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusAborted:
		return true
	}
	return false
}
