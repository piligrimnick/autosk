package datasource

// debugLog mirrors tui.debugLog: tests can install a printf-style
// callback to inspect daemon-fallback decisions without producing
// noise in production. Lives in the datasource package so the live
// path doesn't import the tui package.
var debugLog func(format string, args ...any)

// SetDebugLogger installs the package-level logger. Test-only;
// production leaves it nil and DebugLog returns nil.
func SetDebugLogger(f func(format string, args ...any)) { debugLog = f }

// DebugLog returns the installed logger or nil. Callers check
// against nil to avoid the (cheap) call when nothing is listening.
func DebugLog() func(format string, args ...any) { return debugLog }
