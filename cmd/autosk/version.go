package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"autosk/internal/buildinfo"
	"autosk/internal/projectdb"
	"autosk/internal/store/doltlite"
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

// tryReadSchemaVersion attempts to resolve and open the project DB read-only-ish.
// Returns (schemaVersion, dbPath, ok). ok=false on any failure (including
// "no DB found" — not an error for `version`).
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
		if errors.Is(err, projectdb.ErrNotFound) {
			return 0, "", false
		}
		return 0, "", false
	}
	s := doltlite.New()
	if err := s.Open(ctx, path); err != nil {
		return 0, path, false
	}
	defer s.Close()
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return 0, path, false
	}
	return v, path, true
}
