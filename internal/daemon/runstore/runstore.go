// Package runstore holds the job (daemon_runs) status enum shared by the
// Go CLI + lazy TUI.
//
// The `daemon_runs` table and its writes are owned by the Rust daemon
// (autoskd / autosk-core); the Go front ends only ever consume job
// status values that arrive over JSON-RPC.
package runstore

// RunStatus is the lifecycle state of a job. Mirrors the SQL CHECK enum.
type RunStatus string

const (
	StatusQueued  RunStatus = "queued"
	StatusRunning RunStatus = "running"
	StatusDone    RunStatus = "done"
	StatusFailed  RunStatus = "failed"
	StatusCancel  RunStatus = "cancel"
)

// IsTerminal reports whether s is a sticky terminal status.
func (s RunStatus) IsTerminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCancel:
		return true
	}
	return false
}
