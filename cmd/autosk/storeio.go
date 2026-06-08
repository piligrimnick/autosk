package main

import (
	"os"
	"path/filepath"

	"autosk/internal/projectdb"
)

// projectRootFromCwd returns the absolute project root for the
// current working directory: the directory that contains .autosk/.
// Honours the same --db override / AUTOSK_DB precedence as the daemon's
// project resolution.
//
// Used by worktree-isolation callers that need the project root for
// deterministic worktree-path derivation client-side (the worktree path
// is derived, never stored). Returns an error when no .autosk/db can be
// located.
//
// This is the lone survivor of the pre-daemon openStore helper: under the
// single-writer model the Go binary no longer opens .autosk/db (autoskd is
// the sole reader+writer), so the store-opening + dolt-commit helpers are
// gone and this file is CGO-free.
func projectRootFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dbPath, err := projectdb.Resolve(cwd, flagDB)
	if err != nil {
		return "", err
	}
	absDB, err := filepath.Abs(dbPath)
	if err != nil {
		return "", err
	}
	// dbPath is .../<root>/.autosk/db; root = parent of .autosk.
	return filepath.Dir(filepath.Dir(absDB)), nil
}
