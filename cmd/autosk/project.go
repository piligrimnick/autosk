package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newProjectCmd is the project management group. v2 projects are .autosk/
// directories registered in ~/.autosk/projects.json; the daemon resolves them
// by walk-up from {cwd}.
func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Inspect and manage autosk projects",
	}
	cmd.AddCommand(newProjectListCmd(), newProjectAddCmd(), newProjectDiagnosticsCmd())
	return cmd
}

func newProjectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the projects registered with the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cliClient()
			if err != nil {
				return err
			}
			projects, err := cl.Projects(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(projects)
			}
			if len(projects) == 0 {
				fmt.Fprintln(os.Stderr, "(no projects)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tROOT")
			for _, p := range projects {
				fmt.Fprintf(w, "%s\t%s\n", p.Name, p.Root)
			}
			return w.Flush()
		},
	}
}

func newProjectAddCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Register the current directory's project with the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cliClient()
			if err != nil {
				return err
			}
			info, err := cl.ProjectAdd(cmd.Context(), name)
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("registered %s (%s)\n", info.Name, info.Root)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "project name (default: basename of the root)")
	return cmd
}

func newProjectDiagnosticsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "diagnostics",
		Aliases: []string{"diag"},
		Short:   "Show extension load errors for the current project",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			diag, err := cl.Diagnostics(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(diag)
			}
			fmt.Printf("project: %s\n", diag.Root)
			if len(diag.Extensions) == 0 {
				fmt.Println("extensions: ok (no load errors)")
				return nil
			}
			fmt.Printf("extension load errors (%d):\n", len(diag.Extensions))
			for _, e := range diag.Extensions {
				fmt.Printf("  - %s: %s\n", e.Source, e.Error)
			}
			return nil
		},
	}
}
