// autosk — minimal task tracker for AI coding agents.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Globals set by persistent flags.
var (
	flagJSON  bool
	flagDB    string // overrides AUTOSK_DB / project discovery
	flagQuiet bool
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "autosk",
		Short:         "Minimal task tracker for AI coding agents",
		Long:          "autosk tracks tasks with a tiny, agent-friendly CLI backed by doltlite.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "emit machine-readable JSON")
	root.PersistentFlags().StringVar(&flagDB, "db", "", "path to .autosk/db (overrides discovery and AUTOSK_DB)")
	root.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "suppress non-essential output")

	root.AddCommand(
		newVersionCmd(),
		newCreateCmd(),
		newShowCmd(),
		newListCmd(),
		newReadyCmd(),
		newNextCmd(),
		newClaimCmd(),
		newDoneCmd(),
		newCancelCmd(),
		newReopenCmd(),
		newUpdateCmd(),
		newBlockCmd(),
		newUnblockCmd(),
		newDepCmd(),
		newInitCmd(),
		newMigrateCmd(),
		newSQLCmd(),
		newHistoryCmd(),
	)
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if errors.Is(err, errSilentExit1) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "autosk: "+err.Error())
		os.Exit(1)
	}
}
