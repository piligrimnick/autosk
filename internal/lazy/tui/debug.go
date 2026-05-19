package tui

// debugLog, when non-nil, is invoked by the TUI from places where a
// breakpoint-via-printf is useful (layout, refresh, popup). Tests set
// this to a t.Logf binding; production leaves it nil.
var debugLog func(format string, args ...any)

// SetDebugLogger installs a debug logger. Exported for tests.
func SetDebugLogger(f func(format string, args ...any)) { debugLog = f }
