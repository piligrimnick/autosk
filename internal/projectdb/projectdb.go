// Package projectdb resolves the project root for a given invocation under
// proto-v2.
//
// v2 has no database: a "project" is a directory containing an .autosk/
// directory (holding tasks/, sessions/, extensions/). The daemon owns project
// resolution server-side (walk-up from {cwd} to the nearest .autosk/); this
// package mirrors that walk-up so the CLI can decide, client-side, whether a
// project already exists before it auto-inits one.
package projectdb

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	// Dir is the per-project marker directory name.
	Dir = ".autosk"

	// EnvNoAutoInit refuses auto-creation (for tests / automation).
	EnvNoAutoInit = "AUTOSK_NO_AUTOINIT"
)

// ErrNotFound — no .autosk/ project was found in cwd or any parent.
var ErrNotFound = errors.New("no .autosk/ project found in this directory or any parent")

// ErrAutoInitDisabled — AUTOSK_NO_AUTOINIT was set.
var ErrAutoInitDisabled = errors.New("auto-init disabled by AUTOSK_NO_AUTOINIT")

// ResolveRoot walks up from cwd looking for an .autosk/ directory and returns
// the project root that holds it. It never creates anything on disk — the
// daemon's project.init lays down the skeleton. Mirrors the daemon's
// resolveProjectRoot walk-up.
func ResolveRoot(cwd string) (string, error) {
	dir := cwd
	for {
		marker := filepath.Join(dir, Dir)
		if fi, err := os.Stat(marker); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}
