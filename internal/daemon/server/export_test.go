package server

import (
	"autosk/internal/daemon/api"
	"autosk/internal/daemon/pi"
)

// BuildInputCommand is the exported test alias for buildInputCommand,
// so attach_test.go can verify the prompt|steer|follow_up dispatch
// table without depending on the rest of the handler plumbing.
func BuildInputCommand(req api.InputRequest, streaming bool) (pi.Command, string) {
	return buildInputCommand(req, streaming)
}

// ReplayStartIndex is the exported test alias for replayStartIndex,
// so sse_test.go can table-test the Last-Event-ID / full / limit
// branching without standing up a real session.jsonl on disk.
func ReplayStartIndex(skip int, full bool, limit, total int) int {
	return replayStartIndex(skip, full, limit, total)
}

// IsStateMismatchError is the exported test alias for
// isStateMismatchError, so attach_test.go can pin the recognised pi
// error-string tokens.
func IsStateMismatchError(s string) bool { return isStateMismatchError(s) }
