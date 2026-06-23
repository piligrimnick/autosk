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
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version + the daemon version when reachable",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := versionInfo{
				Version: buildinfo.Version,
				Commit:  buildinfo.Commit,
				Backend: buildinfo.Backend,
				Go:      runtime.Version(),
				OS:      runtime.GOOS,
				Arch:    runtime.GOARCH,
			}
			// Best-effort: report the daemon's version if one is already
			// running. `autosk version` never auto-spawns (zero side effects).
			if dv, ok := tryDaemonVersion(cmd.Context()); ok {
				info.DaemonVersion = dv.Version
				info.DaemonCommit = dv.Commit
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(info)
			}
			fmt.Printf("autosk %s (%s)\n", info.Version, info.Commit)
			fmt.Printf("  backend:        %s\n", info.Backend)
			if info.DaemonVersion != "" {
				fmt.Printf("  daemon:         %s (%s)\n", info.DaemonVersion, info.DaemonCommit)
			} else {
				fmt.Printf("  daemon:         -   (not running)\n")
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
	DaemonVersion string `json:"daemon_version,omitempty"`
	DaemonCommit  string `json:"daemon_commit,omitempty"`
	Go            string `json:"go"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
}

// tryDaemonVersion probes an already-running daemon for its version. It never
// auto-spawns (NoAutoSpawn) so `autosk version` has zero side effects.
func tryDaemonVersion(ctx context.Context) (rpcclient.Version, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	cl, err := rpcclient.New(rpcclient.Options{NoAutoSpawn: true})
	if err != nil {
		return rpcclient.Version{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	v, err := cl.Version(cctx)
	if err != nil {
		return rpcclient.Version{}, false
	}
	return v, true
}
