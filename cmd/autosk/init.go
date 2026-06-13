package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newInitCmd: explicit `autosk init`. A client of the daemon's project.init,
// which lays down the .autosk/{tasks,sessions,extensions} skeleton and
// registers the project in ~/.autosk/projects.json. Idempotent. v2 has no
// database and no per-project workflow seeding — workflows/agents ship as
// bundled extensions available to every project.
func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize an autosk project in the current directory",
		Long: `Create ./.autosk (tasks/, sessions/, extensions/) and register the
project with the daemon.

Idempotent: re-running is a no-op. Write commands auto-init the project
on first use, so calling init is optional.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cliClient()
			if err != nil {
				return err
			}
			info, err := cl.ProjectInit(cmd.Context())
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("initialized %s/.autosk\n", info.Root)
			}
			return nil
		},
	}
	return cmd
}
