package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/buildinfo"
	"autosk/internal/changelog"
	"autosk/internal/daemon/client"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/tui"
	"autosk/internal/store/doltlite"
	"autosk/internal/userstate"
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
		sock        string
		refresh     time.Duration
		noChangelog bool
	)
	cmd := &cobra.Command{
		Use:   "lazy",
		Short: "Interactive TUI for tasks, jobs, workflows, and agents",
		Long: "lazy is a lazygit-style terminal dashboard for the autosk world.\n" +
			"Tasks, Jobs, Workflows, and Agents in one process; selecting a job\n" +
			"renders its transcript (live + archive) directly in the Detail pane.\n" +
			"On the first run of a new release, lazy opens a modal showing the\n" +
			"embedded CHANGELOG.md; press ctrl+w to re-open it manually, or pass\n" +
			"--no-changelog to suppress the auto-popup.",
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

			opts := tui.Options{
				Datasource:  comp,
				ProjectRoot: cwd,
				Refresh:     refresh,
			}
			if !noChangelog {
				opts.ChangelogModal = buildChangelogModal(buildinfo.Version)
			}
			return tui.Run(ctx, opts)
		},
	}
	cmd.Flags().StringVar(&sock, "sock", "", "daemon socket path (default $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().DurationVar(&refresh, "refresh", 2*time.Second, "panel refresh interval")
	cmd.Flags().BoolVar(&noChangelog, "no-changelog", false,
		"suppress the first-run-of-a-new-release changelog popup (read-only; does not modify ~/.autosk/state.json)")
	return cmd
}

// buildChangelogModal decides whether to push the auto-popup on lazy
// start and returns the populated ChangelogModalOptions (or nil to
// skip the popup). Policy lives here so the TUI package stays
// build-info-agnostic and the tests can exercise the decision
// independently of a real tui.Run.
//
// Rules (locked in the task plan):
//
//   - Dev / non-release builds skip the popup entirely AND leave
//     state.json untouched. buildinfo.Version="dev" or any
//     `git describe` output that doesn't normalise to a clean
//     vX.Y.Z falls into this branch.
//   - First-run path (state.json missing or last_seen_changelog
//     empty): show the FULL embedded changelog, then stamp
//     last_seen_changelog to the latest version on dismiss.
//   - Steady-state path: show only the entries strictly newer than
//     last_seen_changelog, stamp on dismiss.
//   - No unseen entries: return nil (no popup).
//   - Embedded changelog is empty (no parseable entries): return
//     nil. The binary still works — the popup is a UX nicety.
func buildChangelogModal(version string) *tui.ChangelogModalOptions {
	_, isRelease := changelog.NormalizeBuild(version)
	if !isRelease {
		return nil
	}
	entries := changelog.Embedded()
	if len(entries) == 0 {
		return nil
	}
	state, err := userstate.Load()
	if err != nil {
		// Malformed state.json: skip the popup AND don't touch the
		// file. The lazy startup hook would otherwise silently
		// overwrite the operator's (corrupt) file, hiding the bug.
		return nil
	}
	unseen := changelog.Unseen(entries, state.LastSeenChangelog)
	if len(unseen) == 0 {
		return nil
	}
	latest := changelog.Latest(entries)
	title := "What's new in autosk " + latest
	return &tui.ChangelogModalOptions{
		Title: title,
		Body:  changelog.RenderMarkdown(unseen),
		OnDismiss: func() error {
			state.LastSeenChangelog = latest
			return userstate.Save(state)
		},
	}
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
