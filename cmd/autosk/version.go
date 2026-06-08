package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/buildinfo"
	"autosk/internal/daemon/rpcclient"
	"autosk/internal/projectdb"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, backend, and schema info",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := versionInfo{
				Version: buildinfo.Version,
				Commit:  buildinfo.Commit,
				Backend: buildinfo.Backend,
				Go:      runtime.Version(),
				OS:      runtime.GOOS,
				Arch:    runtime.GOARCH,
			}
			// Best-effort: report schema version of the project DB if we can find one.
			// Failure here is not an error — `autosk version` should always work.
			if v, dbPath, ok := tryReadSchemaVersion(cmd.Context()); ok {
				info.SchemaVersion = v
				info.DBPath = dbPath
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(info)
			}
			fmt.Printf("autosk %s (%s)\n", info.Version, info.Commit)
			fmt.Printf("  backend:        %s\n", info.Backend)
			if info.DBPath != "" {
				fmt.Printf("  schema version: %d  (%s)\n", info.SchemaVersion, info.DBPath)
			} else {
				fmt.Printf("  schema version: -   (no .autosk/db in scope)\n")
			}
			fmt.Printf("  go:             %s %s/%s\n", info.Go, info.OS, info.Arch)
			return nil
		},
	}
}

type versionInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Backend       string `json:"backend"`
	SchemaVersion int    `json:"schema_version"`
	DBPath        string `json:"db_path,omitempty"`
	Go            string `json:"go"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
}

// tryReadSchemaVersion best-effort resolves the project DB and reports its
// schema version. Returns (schemaVersion, dbPath, ok); ok=false on any failure
// (including "no DB in scope" — not an error for `version`).
//
// Under the single-writer model the Go binary cannot open the DB itself (it is
// CGO-free), so the schema version is read over RPC from an *already-running*
// autoskd. `autosk version` never auto-spawns a daemon — it must work with zero
// side effects — so when no daemon is up the schema line falls back to the
// "no .autosk/db in scope" rendering even though a project exists.
func tryReadSchemaVersion(ctx context.Context) (int, string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 0, "", false
	}
	path, err := projectdb.Resolve(cwd, flagDB)
	if err != nil {
		return 0, "", false
	}
	cl, err := rpcclient.New(rpcclient.Options{DBOverride: dbOverride(), NoAutoSpawn: true})
	if err != nil {
		return 0, path, false
	}
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	// COALESCE(..., 0) preserves the old migrations.CurrentVersion rendering
	// for an empty schema_migrations table (0, ok=true) instead of MAX()'s NULL
	// → nil → ok=false → "-". A real initialized DB always has ≥1 row.
	rows, err := cl.SQLQuery(cctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations")
	if err != nil {
		return 0, path, false
	}
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 1 {
		return 0, path, false
	}
	switch v := rows.Rows[0][0].(type) {
	case float64:
		return int(v), path, true
	case int64:
		return int(v), path, true
	default:
		return 0, path, false
	}
}
