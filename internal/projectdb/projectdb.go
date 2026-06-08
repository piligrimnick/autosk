// Package projectdb resolves where the autosk database lives for a given
// invocation.
//
// Resolution order:
//  1. --db flag (handled by caller; passed as `override` here).
//  2. AUTOSK_DB env var.
//  3. Walk up from cwd looking for .autosk/db.
//  4. AutoInit: create ./.autosk/db in cwd (only when the caller is OK with it).
//
// AUTOSK_NO_AUTOINIT=1 disables step 4.
package projectdb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// Dir is the per-project directory name.
	Dir = ".autosk"
	// FileName is the database filename inside Dir.
	FileName = "db"

	// EnvDB overrides discovery entirely.
	EnvDB = "AUTOSK_DB"
	// EnvNoAutoInit refuses auto-creation (for tests).
	EnvNoAutoInit = "AUTOSK_NO_AUTOINIT"
)

// ErrNotFound — no .autosk/db was found and AutoInit was not invoked.
var ErrNotFound = errors.New("no .autosk/db found in this directory or any parent")

// ErrAutoInitDisabled — AUTOSK_NO_AUTOINIT was set.
var ErrAutoInitDisabled = errors.New("auto-init disabled by AUTOSK_NO_AUTOINIT")

// Resolve picks a DB path for reads. It walks up from cwd. If override is
// non-empty it wins; AUTOSK_DB wins next; then walk-up.
//
// Resolve never creates anything on disk — see AutoInit for that.
func Resolve(cwd, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv(EnvDB); env != "" {
		return env, nil
	}
	if p, ok := walkUp(cwd); ok {
		return p, nil
	}
	return "", ErrNotFound
}

// ResolveOrInit is the write-side cousin of Resolve. If discovery fails it
// creates ./.autosk/db in cwd, unless AUTOSK_NO_AUTOINIT is set.
//
// Returns (path, created, error) where created indicates whether AutoInit ran.
func ResolveOrInit(cwd, override string) (path string, created bool, err error) {
	p, rerr := Resolve(cwd, override)
	if rerr == nil {
		return p, false, nil
	}
	if !errors.Is(rerr, ErrNotFound) {
		return "", false, rerr
	}
	if os.Getenv(EnvNoAutoInit) != "" {
		return "", false, ErrAutoInitDisabled
	}
	p, err = autoInit(cwd)
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

func walkUp(start string) (string, bool) {
	dir := start
	for {
		candidate := filepath.Join(dir, Dir, FileName)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func autoInit(cwd string) (string, error) {
	dir := filepath.Join(cwd, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return filepath.Join(dir, FileName), nil
}
