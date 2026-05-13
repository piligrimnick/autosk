package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"autosk/internal/projectdb"
	"autosk/internal/store/doltlite"
)

// newInitCmd: explicit `autosk init`. Idempotent — runs migrations on an
// existing DB.
func newInitCmd() *cobra.Command {
	var prefix string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize an autosk database in the current directory",
		Long: `Create ./.autosk/db and apply schema migrations.

Idempotent: running on an existing project re-applies any pending migrations.

Write commands auto-init the database on first use, so calling init is
optional unless you want to set a custom ID prefix (not yet implemented).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if prefix != "" {
				// Reserved for when we persist config.
				return errors.New("--prefix is reserved for a future release (current default: 'as')")
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			dbDir := filepath.Join(cwd, projectdb.Dir)
			if err := os.MkdirAll(dbDir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dbDir, err)
			}
			dbPath := filepath.Join(dbDir, projectdb.FileName)

			s := doltlite.New()
			if err := s.Open(cmd.Context(), dbPath); err != nil {
				return err
			}
			defer s.Close()
			if err := s.Migrate(cmd.Context()); err != nil {
				return err
			}
			v, _ := s.SchemaVersion(cmd.Context())
			if !flagQuiet {
				fmt.Printf("initialized %s (schema_version=%d)\n", dbPath, v)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "ID prefix (reserved; currently always 'as')")
	return cmd
}

// newMigrateCmd: explicit migrate (also runs implicitly on every Open).
func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply any pending schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			// Migrate already ran inside openStore — report current version.
			v, err := s.SchemaVersion(cmd.Context())
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("schema_version=%d\n", v)
			}
			return nil
		},
	}
}

// newHistoryCmd: stub. Reserves the verb until doltlite log integration lands.
func newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <id>",
		Short: "Show change history for a task (stub — coming in v0.2)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "history: delegates to doltlite log; planned for v0.2")
			return errSilentExit1
		},
	}
}
