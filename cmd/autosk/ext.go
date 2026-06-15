package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newExtCmd is the extension-management group (`autosk ext …`), modeled on pi's
// package management. `add` installs a source (`autosk ext add npm:@scope/pkg` /
// `autosk ext add ./my-ext`); `list` / `remove` / `update` are siblings. A source
// is either `npm:<spec>` (optionally `@version`) or a local path (`/abs`, `./rel`,
// `../rel`, `~/path`) — there is no implicit bare-name → npm form.
//
// A GLOBAL operation (the default) targets ~/.autosk; `-l/--local` targets the
// current project's .autosk/ instead. There is no hot-reload: a freshly
// installed or updated extension is picked up on the next daemon start / first
// project open, so the commands print a restart hint when something changed.
func newExtCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ext",
		Short: "Manage autosk extensions (add / list / remove / update)",
		Long: "Manage the extensions recorded in settings.json.\n\n" +
			"A source is either an npm spec (npm:@scope/pkg, npm:@scope/pkg@1.2.3) or a\n" +
			"local path (/abs, ./rel, ../rel, ~/path). npm packages install into a\n" +
			"packages prefix; a local path is referenced in place (never copied).\n\n" +
			"By default these target the global scope (~/.autosk); pass -l/--local to\n" +
			"target the current project's .autosk/ instead.",
	}
	cmd.AddCommand(newExtAddCmd(), newExtListCmd(), newExtRemoveCmd(), newExtUpdateCmd())
	return cmd
}

func newExtAddCmd() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "add <source>",
		Short: "Add an autosk extension (npm:<spec> or a local path)",
		Long: "Add an autosk extension and record it in settings.json.\n\n" +
			"A source is either an npm spec (npm:@scope/pkg, npm:@scope/pkg@1.2.3) or a\n" +
			"local path (/abs, ./rel, ../rel, ~/path). npm packages install into a\n" +
			"packages prefix; a local path is referenced in place (never copied).\n\n" +
			"By default the extension is added globally (~/.autosk); pass -l/--local\n" +
			"to add it to the current project's .autosk/ instead.",
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
	cmd.Flags().BoolVarP(&local, "local", "l", false, "add to the current project (.autosk/) instead of globally")
	return cmd
}

func newExtListCmd() *cobra.Command {
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

func newExtRemoveCmd() *cobra.Command {
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

// newExtUpdateCmd bumps installed npm extensions to newer registry versions in
// place. Outside a project it updates the global scope only; inside a project it
// updates the union of global + project. Version-pinned npm entries
// (npm:foo@1.2.3) and local-path entries are skipped (nothing to update); only
// floating npm entries (npm:foo) with a newer registry version are bumped.
func newExtUpdateCmd() *cobra.Command {
	var local, global, dryRun bool
	cmd := &cobra.Command{
		Use:   "update [source]",
		Short: "Update installed npm extensions to newer registry versions",
		Long: "Bump installed npm extensions to newer registry versions in place.\n\n" +
			"Outside an autosk project this updates the GLOBAL scope only (~/.autosk);\n" +
			"inside a project it updates the UNION of global + project scopes. Pass\n" +
			"--global to force global-only, or -l/--local to force project-only.\n\n" +
			"Version-pinned npm entries (npm:foo@1.2.3) and local-path entries are\n" +
			"skipped — only floating npm entries (npm:foo) are updated. An optional\n" +
			"[source] (npm:<name>) targets a single extension. Use --dry-run/--check\n" +
			"to report available updates without installing anything.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if local && global {
				return fmt.Errorf("-l/--local and --global are mutually exclusive")
			}
			scope := ""
			switch {
			case local:
				scope = "project"
			case global:
				scope = "global"
			}
			var source string
			if len(args) == 1 {
				source = args[0]
			}
			cl, err := cliClient()
			if err != nil {
				return err
			}
			res, err := cl.UpdateExtensions(cmd.Context(), source, scope, dryRun)
			if err != nil {
				return err
			}
			// Decide the exit code once, independent of the output format: a
			// real run that left any package in status "failed" exits non-zero
			// for both --json (the automation path) and the table. Dry-run
			// never produces "failed", so it always exits 0.
			failed := extUpdateFailed(res)
			if flagJSON {
				if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
					return err
				}
			} else if err := renderExtUpdate(res); err != nil {
				return err
			}
			if failed {
				return errSilentExit1
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&local, "local", "l", false, "update only the current project (.autosk/)")
	cmd.Flags().BoolVar(&global, "global", false, "update only the global scope (~/.autosk)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report available updates without installing anything")
	cmd.Flags().BoolVar(&dryRun, "check", false, "alias for --dry-run")
	return cmd
}

// extUpdateFailed reports whether any entry left the registry in a failed state.
// Used to decide the process exit code independently of the output format so
// that the --json automation path honors the same non-zero exit as the table.
func extUpdateFailed(res rpcclient.ExtensionUpdateResult) bool {
	for _, e := range res.Entries {
		if e.Status == "failed" {
			return true
		}
	}
	return false
}

// renderExtUpdate prints the human-readable update table + summary and the
// restart hint (stderr, only when something changed in a real run). The exit
// code on failure is decided by the caller via extUpdateFailed, not here, so
// the --json and table paths stay symmetric.
func renderExtUpdate(res rpcclient.ExtensionUpdateResult) error {
	if len(res.Entries) == 0 {
		fmt.Fprintln(os.Stderr, "(no updatable extensions)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCOPE\tPACKAGE\tFROM\tTO\tSTATUS")
	var updated, upToDate, skipped, failed, available, unknown int
	for _, e := range res.Entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Scope, e.Name, dash(e.FromVersion), dash(e.ToVersion), e.Status)
		switch e.Status {
		case "updated":
			updated++
		case "up-to-date":
			upToDate++
		case "skipped":
			skipped++
		case "failed":
			failed++
		case "unknown":
			unknown++
		default:
			available++
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if res.DryRun {
		fmt.Printf("available %d · unknown %d · up-to-date %d · skipped %d\n", available, unknown, upToDate, skipped)
	} else {
		fmt.Printf("updated %d · up-to-date %d · skipped %d · failed %d\n", updated, upToDate, skipped, failed)
		if res.Changed {
			fmt.Fprintln(os.Stderr, "note: restart the daemon (or reopen the project) for the change to take effect")
		}
	}
	return nil
}

// dash renders an empty version cell as "-" for the update table.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
