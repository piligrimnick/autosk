package executor

import "os"

// ensureDir is a tiny wrapper kept separate so tests can stub it later
// without touching executor.go's main flow.
func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }
