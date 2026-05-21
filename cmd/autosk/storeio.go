package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"autosk/internal/projectdb"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// commitWrite records a doltlite commit after a successful mutation. Best-effort.
// Has no effect on non-doltlite backends.
func commitWrite(ctx context.Context, s store.Store, msg string) {
	if dl, ok := s.(*doltlite.Store); ok {
		_ = dl.DoltCommit(ctx, msg)
	}
}

// projectRootFromCwd returns the absolute project root for the
// current working directory: the directory that contains .autosk/.
// Honours the same --db override / AUTOSK_DB precedence as openStore.
//
// Used by worktree-isolation callers that need the project root for
// deterministic worktree-path derivation. Returns an error when no
// .autosk/db can be located.
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

// openStore resolves the DB path and opens it, running migrations.
//
// writeOK controls whether AutoInit is allowed (write commands pass true).
// Returns the store and a close func that the caller must defer.
func openStore(ctx context.Context, writeOK bool) (store.Store, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}

	var (
		dbPath  string
		created bool
	)
	if writeOK {
		dbPath, created, err = projectdb.ResolveOrInit(cwd, flagDB)
	} else {
		dbPath, err = projectdb.Resolve(cwd, flagDB)
	}
	if err != nil {
		if errors.Is(err, projectdb.ErrNotFound) {
			return nil, nil, fmt.Errorf("no .autosk/db found in this directory or any parent (run `autosk init`, or run a write command to auto-init)")
		}
		return nil, nil, err
	}

	s := doltlite.New()
	if err := s.Open(ctx, dbPath); err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("migrate %s: %w", dbPath, err)
	}
	if created && !flagQuiet {
		fmt.Fprintf(os.Stderr, "autosk: created %s\n", dbPath)
	}
	return s, func() { _ = s.Close() }, nil
}
