package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newInstallCmd is the extension-management group, modeled on pi's package
// management. The PARENT verb installs a source (`autosk install npm:@scope/pkg`
// / `autosk install ./my-ext`); `list` and `remove` are subcommands (the
// top-level `list`/`remove` verbs are already taken). A source is either
// `npm:<spec>` (optionally `@version`) or a local path (`/abs`, `./rel`,
// `../rel`, `~/path`) — there is no implicit bare-name → npm form.
//
// A GLOBAL install (the default) writes ~/.autosk; `-l/--local` installs into
// the current project's .autosk/ instead. There is no hot-reload: a freshly
// installed extension is picked up on the next daemon start / first project
// open, so the commands print a restart hint.
func newInstallCmd() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "install <source>",
		Short: "Install an autosk extension (npm:<spec> or a local path)",
		Long: "Install an autosk extension and record it in settings.json.\n\n" +
			"A source is either an npm spec (npm:@scope/pkg, npm:@scope/pkg@1.2.3) or a\n" +
			"local path (/abs, ./rel, ../rel, ~/path). npm packages install into a\n" +
			"packages prefix; a local path is referenced in place (never copied).\n\n" +
			"By default the extension is installed globally (~/.autosk); pass -l/--local\n" +
			"to install into the current project's .autosk/ instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cliClient()
			if err != nil {
				return err
			}
			res, err := cl.InstallExtension(cmd.Context(), args[0], local)
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			if !flagQuiet {
				verb := "registered"
				if res.Installed {
					verb = "installed"
				}
				fmt.Printf("%s %s (%s scope)\n", verb, res.Source, res.Scope)
				fmt.Printf("  settings: %s\n", res.SettingsPath)
				fmt.Fprintln(os.Stderr, "note: restart the daemon (or reopen the project) for the change to take effect")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&local, "local", "l", false, "install into the current project (.autosk/) instead of globally")
	cmd.AddCommand(newInstallListCmd(), newInstallRemoveCmd())
	return cmd
}

func newInstallListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed extensions (global + project)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cliClient()
			if err != nil {
				return err
			}
			res, err := cl.ListExtensions(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			if len(res.Entries) == 0 {
				fmt.Fprintln(os.Stderr, "(no extensions)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SCOPE\tKIND\tRESOLVED\tSOURCE")
			for _, e := range res.Entries {
				resolved := "yes"
				if !e.Resolved {
					resolved = "no"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Scope, e.Kind, resolved, e.Source)
			}
			return w.Flush()
		},
	}
}

func newInstallRemoveCmd() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "remove <source>",
		Short: "Remove an extension entry from settings.json",
		Long: "Remove an extension entry from settings.json (match by name for npm, by\n" +
			"path for local — any version). node_modules is left untouched.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cliClient()
			if err != nil {
				return err
			}
			res, err := cl.RemoveExtension(cmd.Context(), args[0], local)
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			if !flagQuiet {
				if res.Removed {
					fmt.Printf("removed %s (%s scope)\n", res.Source, res.Scope)
					fmt.Fprintln(os.Stderr, "note: restart the daemon (or reopen the project) for the change to take effect")
				} else {
					fmt.Printf("no matching entry for %s (%s scope)\n", res.Source, res.Scope)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&local, "local", "l", false, "remove from the current project (.autosk/) instead of globally")
	return cmd
}
