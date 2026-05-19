// `autosk attach <job-id>` opens an in-process Bubble Tea TUI that
// mirrors a running daemon agent session: the full transcript is
// rendered live from the daemon's SSE stream, and operator input is
// dispatched back as prompt|steer|follow_up (the daemon picks the
// shape automatically based on pi's current state).
//
// History: this command used to exec a local `pi` with a custom
// `pi-autosk-attach` TS extension that talked back to the daemon. The
// extension surface ended up being heavier than the value it carried,
// so the rendering path is now a Go TUI inside this same binary.
//
// See docs/attach.md for the operator manual and
// docs/plans/20260519-Attach-Plan-v2.md for the locked design
// decisions; the abandoned v1 plan is preserved at
// docs/plans/20260519-Attach-Plan.md for the paper trail. The
// retired TypeScript extension still lives at extension-attach/
// pending cleanup task as-a3c2.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"autosk/internal/attach/tui"
	"autosk/internal/daemon/client"
)

// newAttachCmd builds the `autosk attach` subcommand.
func newAttachCmd() *cobra.Command {
	var (
		sockPath string
		cwdPath  string
	)
	cmd := &cobra.Command{
		Use:   "attach <job-id>",
		Short: "Open a Bubble Tea TUI mirroring a running daemon agent session (read/write)",
		Long: "Attach opens a Bubble Tea TUI that mirrors a running autosk daemon\n" +
			"agent session: the full transcript streams in via SSE and you can\n" +
			"type prompts that are dispatched as prompt|steer|follow_up. The\n" +
			"local terminal owns nothing — the daemon's pi process is the\n" +
			"single writer to the session JSONL. Closing the TUI detaches.\n\n" +
			"Key bindings: Enter newline · Ctrl-D send · Ctrl-F follow_up ·\n" +
			"Ctrl-A abort · Ctrl-C/Ctrl-Q quit. See docs/attach.md for the\n" +
			"operator manual and docs/plans/20260519-Attach-Plan-v2.md for\n" +
			"the locked design decisions.",

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]

			// Resolve cwd so X-Autosk-Cwd is always set, even when the
			// daemon was started in a different project root.
			if cwdPath == "" {
				wd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("getwd: %w", err)
				}
				cwdPath = wd
			}
			absCwd, err := filepath.Abs(cwdPath)
			if err != nil {
				return fmt.Errorf("resolve --cwd: %w", err)
			}

			// Sock resolution is shared with the rest of the CLI so
			// AUTOSK_SOCK / ~/.autosk/daemon.sock fallbacks behave
			// identically here.
			resolvedSock := sockPath
			if resolvedSock == "" {
				resolvedSock = defaultSockPath()
			}

			c, err := client.New(client.Options{
				Sock:       resolvedSock,
				Cwd:        absCwd,
				DBOverride: resolveDBOverride(),
			})
			if err != nil {
				return fmt.Errorf("init daemon client: %w", err)
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			if err := tui.Run(ctx, c, jobID); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sockPath, "sock", "", "unix socket path (default: $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().StringVar(&cwdPath, "cwd", "", "project root for X-Autosk-Cwd (default: current dir)")
	return cmd
}


