package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newWorkflowCmd is the read-only workflow registry view. v2 workflows are code
// registered by extensions (no create/delete/update); the registry exposes the
// rendered projection via registry.workflow.list/get.
func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Inspect the workflows registered by the project's extensions",
		Long: "Workflows are code registered by extensions (the bundled feature-dev\n" +
			"plus any project/global extensions). This is a read-only view; there\n" +
			"is no create/delete/update in v2 — editing a workflow means editing\n" +
			"its extension code.",
	}
	cmd.AddCommand(newWorkflowListCmd(), newWorkflowShowCmd())
	return cmd
}

func newWorkflowListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered workflows",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wfs, err := cl.Workflows(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(wfs)
			}
			if len(wfs) == 0 {
				fmt.Fprintln(os.Stderr, "(no workflows registered)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tFIRST_STEP\tSTEPS\tISOLATION")
			for _, wf := range wfs {
				names := make([]string, 0, len(wf.Steps))
				for _, s := range wf.Steps {
					names = append(names, s.Name)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", wf.Name, wf.FirstStep, strings.Join(names, ","), wf.Isolation)
			}
			return w.Flush()
		},
	}
}

func newWorkflowShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one registered workflow (steps, agents, isolation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			wf, err := cl.GetWorkflow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(wf)
			}
			fmt.Printf("name:       %s\n", wf.Name)
			if wf.Description != "" {
				fmt.Printf("description: %s\n", wf.Description)
			}
			fmt.Printf("first_step: %s\n", wf.FirstStep)
			fmt.Printf("isolation:  %s\n", wf.Isolation)
			fmt.Println("steps:")
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  STEP\tAGENT\tTARGETS")
			for _, s := range wf.Steps {
				agent := s.Agent
				if s.Human {
					agent = "(human)"
				}
				if agent == "" {
					agent = "-"
				}
				fmt.Fprintf(w, "  %s\t%s\t%s\n", s.Name, agent, joinTargets(s.Targets))
			}
			return w.Flush()
		},
	}
}

func joinTargets(ts []rpcclient.StepTarget) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.Step != "" {
			out = append(out, t.Step)
		} else if t.Status != "" {
			out = append(out, "→"+t.Status)
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}
