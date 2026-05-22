package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/client"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/tui"
	"autosk/internal/store/doltlite"
)

// newLazyCmd is the cobra entry point for `autosk lazy`.
//
// Behaviour:
//
//   - Opens the project DB read-write (lazy auto-inits when missing,
//     same as the other write-capable verbs).
//   - Constructs a datasource.Compose that probes the daemon every
//     --refresh interval; when the daemon is reachable, Jobs come
//     from the live HTTP API (with Streaming/AttachCount), otherwise
//     they come from .autosk/db (and the live SSE subscription is
//     disabled).
func newLazyCmd() *cobra.Command {
	var (
		sock    string
		refresh time.Duration
	)
	cmd := &cobra.Command{
		Use:   "lazy",
		Short: "Interactive TUI for tasks, jobs, workflows, and agents",
		Long: "lazy is a lazygit-style terminal dashboard for the autosk world.\n" +
			"Tasks, Jobs, Workflows, and Agents in one process; selecting a job\n" +
			"renders its transcript (live + archive) directly in the Detail pane.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, closeFn, err := openStore(ctx, true)
			if err != nil {
				return err
			}
			defer closeFn()

			// Tie the doltlite connection-rotation cadence to the
			// dashboard refresh interval. Lazy is the canonical
			// long-lived reader; without rotation it would silently
			// serve a stale snapshot after a cross-process dolt_gc()
			// atomic-rewrote .autosk/db out from under our fd. See
			// docs/lazy.md "cross-process freshness" and
			// doltlite.DefaultConnLifetime.
			if dl, ok := store.(*doltlite.Store); ok {
				dl.SetConnMaxLifetime(refresh)
			}

			reg, _ := pkgregistry.Default()
			cwd, err := getCwd()
			if err != nil {
				return err
			}
			off, err := datasource.NewOffline(store, cwd, reg)
			if err != nil {
				return fmt.Errorf("datasource: %w", err)
			}
			cli, err := client.New(client.Options{Sock: sock, Cwd: cwd})
			if err != nil {
				return fmt.Errorf("daemon client: %w", err)
			}
			comp := datasource.NewCompose(off, cli, refresh)
			defer comp.Close()

			return tui.Run(ctx, tui.Options{
				Datasource:  comp,
				ProjectRoot: cwd,
				Refresh:     refresh,
			})
		},
	}
	cmd.Flags().StringVar(&sock, "sock", "", "daemon socket path (default $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().DurationVar(&refresh, "refresh", 2*time.Second, "panel refresh interval")
	return cmd
}

// getCwd is split out so tests can swap it. Default uses os.Getwd.
func getCwd() (string, error) {
	return contextGetwd(context.Background())
}

// contextGetwd is a tiny wrapper that future versions can teach to
// honour --cwd or AUTOSK_CWD. v1 just delegates to os.Getwd.
func contextGetwd(_ context.Context) (string, error) {
	return getwdImpl()
}
