package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newAgentCmd is the read-only agent registry view. v2 agents are code shipped
// by extensions (no install/uninstall/runtime); the registry exposes only their
// names via registry.agent.list.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Inspect the agents registered by the project's extensions",
		Long: "Agents are named actors registered in code by extensions (the bundled\n" +
			"@autosk/pi-agent roles plus any project/global extensions). This is a\n" +
			"read-only view; there is no install/uninstall in v2.",
	}
	cmd.AddCommand(newAgentListCmd(), newAgentShowCmd())
	return cmd
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agents registered by the project's extensions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			agents, err := cl.Agents(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(agents)
			}
			if len(agents) == 0 {
				fmt.Fprintln(os.Stderr, "(no agents registered)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME")
			for _, a := range agents {
				fmt.Fprintln(w, a.Name)
			}
			return w.Flush()
		},
	}
}

func newAgentShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one registered agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			agents, err := cl.Agents(cmd.Context())
			if err != nil {
				return err
			}
			for _, a := range agents {
				if a.Name == args[0] {
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(a)
					}
					fmt.Printf("name: %s\n", a.Name)
					return nil
				}
			}
			return fmt.Errorf("agent not found: %s", args[0])
		},
	}
}
