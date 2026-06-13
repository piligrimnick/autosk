package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/buildinfo"
	"autosk/internal/changelog"
	"autosk/internal/daemon/rpcclient"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/tui"
	"autosk/internal/userstate"
)

// newLazyCmd is the cobra entry point for `autosk lazy`.
//
// Behaviour:
//
//   - Renders entirely from autoskd over JSON-RPC (plan §7.5: the single
//     RPC-client Datasource). autoskd owns .autosk/db; the Go binary opens no
//     local doltlite store. autoskd is auto-spawned by the connector on first
//     request (and runs migrations/bootstrap via project.init on a fresh dir).
//   - Reads, writes, and the live job transcript tail all route to the daemon
//     over the UDS. Panels refresh on the daemon's task-changed/project-changed
//     push (no client-side poll); --refresh is the long safety re-sync floor.
func newLazyCmd() *cobra.Command {
	var (
		sock        string
		refresh     time.Duration
		noChangelog bool
	)
	cmd := &cobra.Command{
		Use:   "lazy",
		Short: "Interactive TUI for tasks, sessions, workflows, and agents",
		Long: "lazy is a lazygit-style terminal dashboard for the autosk world.\n" +
			"Tasks, Sessions, Workflows, and Agents in one process; selecting a\n" +
			"session renders its transcript (live + archive) in the Detail pane.\n" +
			"On the first run of a new release, lazy opens a modal showing the\n" +
			"embedded CHANGELOG.md; press ctrl+w to re-open it manually, or pass\n" +
			"--no-changelog to suppress the auto-popup.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLazyRPC(cmd.Context(), sock, refresh, noChangelog)
		},
	}
	cmd.Flags().StringVar(&sock, "sock", "", "daemon socket path (default $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().DurationVar(&refresh, "refresh", 2*time.Second,
		"safety re-sync interval (panels update on the daemon push; floored to 30s while the push is active)")
	cmd.Flags().BoolVar(&noChangelog, "no-changelog", false,
		"suppress the first-run-of-a-new-release changelog popup (read-only; does not modify ~/.autosk/state.json)")
	return cmd
}

// runLazyRPC runs the TUI against the autoskd-backed datasource (reads +
// writes + the live transcript tail + the task-changed/project-changed push).
// autoskd is auto-spawned by the connector on first request; the Go binary
// opens no local doltlite store.
func runLazyRPC(ctx context.Context, sock string, refresh time.Duration, noChangelog bool) error {
	cwd, err := getCwd()
	if err != nil {
		return err
	}
	cli, err := rpcclient.New(rpcclient.Options{Sock: sock, Cwd: cwd})
	if err != nil {
		return fmt.Errorf("autoskd client: %w", err)
	}
	opts := tui.Options{
		Datasource:  datasource.NewRPC(cli),
		ProjectRoot: cwd,
		Refresh:     refresh,
	}
	if !noChangelog {
		opts.ChangelogModal = buildChangelogModal(buildinfo.Version)
	}
	return tui.Run(ctx, opts)
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
