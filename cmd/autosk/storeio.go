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
// writeOK controls whether AutoInit is allowed (write commands pass
// true). When AutoInit fires, openStore also runs the same workflow
// bootstrap that `autosk init` does so a fresh project gets the
// canonical `feature-dev-generic` workflow seeded regardless of which
// verb triggered the auto-init (write verb, `autosk lazy`, ...).
//
// Bootstrap can be suppressed by setting AUTOSK_AUTOINIT_SKIP_BOOTSTRAP
// (see autoinit.go). Bootstrap failures are non-fatal: a warning is
// printed to stderr and the migrated DB is still returned.
//
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
		dbPath, created, err = resolveOrInitInteractive(cwd, flagDB)
	} else {
		dbPath, err = projectdb.Resolve(cwd, flagDB)
	}
	if err != nil {
		switch {
		case errors.Is(err, projectdb.ErrNotFound):
			return nil, nil, fmt.Errorf("no .autosk/db found in this directory or any parent (run `autosk init`, or run a write command to auto-init)")
		case errors.Is(err, ErrAutoInitDeclined):
			return nil, nil, fmt.Errorf("no .autosk/db: declined to create one (run `autosk init` explicitly, or point at an existing project with --db <path> / AUTOSK_DB)")
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

	if created {
		if !flagQuiet {
			fmt.Fprintf(os.Stderr, "autosk: created %s\n", dbPath)
		}
		// NOTE: the legacy in-process auto-init only migrates now.
		// Workflow bootstrap (feature-dev-generic + @autogent/generic)
		// is owned by the daemon's project.init, reached through the
		// write-verb preamble (ensureProject -> ProjectInit). This
		// openStore path survives only for the not-yet-rewired
		// agent/workflow verbs and is slated for removal in 3b-C.
		if os.Getenv(EnvAutoInitSkipBootstrap) == "" {
			if reg, perr := openPackagesRegistry(); perr == nil {
				if err := reg.EnsurePrefix(); err != nil && !flagQuiet {
					fmt.Fprintf(os.Stderr,
						"warning: could not create packages prefix at %s: %v\n",
						reg.Prefix(), err)
				}
			}
		}
	}
	return s, func() { _ = s.Close() }, nil
}
