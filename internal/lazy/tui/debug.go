package tui

// debugLog, when non-nil, is invoked by the TUI from places where a
// breakpoint-via-printf is useful (refresh errors, popup transitions,
// job-detail live pump). Tests install t.Logf via SetDebugLogger;
// production leaves it nil and dlog is a no-op.
var debugLog func(format string, args ...any)

// SetDebugLogger installs a debug logger. Exported for tests.
func SetDebugLogger(f func(format string, args ...any)) { debugLog = f }

// dlog is the in-package wrapper: cheap when no logger is installed.
func dlog(format string, args ...any) {
	if debugLog != nil {
		debugLog(format, args...)
	}
}
