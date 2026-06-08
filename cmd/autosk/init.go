package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newInitCmd: explicit `autosk init`. A client of the daemon's
// project.init, which is idempotent — runs migrations on an existing DB
// and seeds the default `feature-dev-generic` workflow on first run
// (canonical definition: internal/bootstrap/feature-dev-generic.json,
// embedded in autoskd) + auto-installs the `@autogent/generic` agent it
// references.
func newInitCmd() *cobra.Command {
	var (
		prefix        string
		skipBootstrap bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize an autosk database in the current directory",
		Long: `Create ./.autosk/db and apply schema migrations.

Idempotent: running on an existing project re-applies any pending migrations.

After the schema is current, init seeds the default workflow
` + "`feature-dev-generic`" + ` (canonical definition:
internal/bootstrap/feature-dev-generic.json) and auto-installs the
` + "`@autogent/generic`" + ` agent it references. The bootstrap step is
idempotent (re-running init is a no-op once the workflow row exists) and
failures are non-fatal — a warning is printed to stderr and the DB is
still left in a usable state.

Write commands auto-init the database on first use, so calling init is
optional unless you want the default workflow seeded or you want to set
a custom ID prefix (not yet implemented).

Use --skip-bootstrap to opt out of the workflow seed (useful for tests,
CI and offline machines).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if prefix != "" {
				// Reserved for when we persist config.
				return errors.New("--prefix is reserved for a future release (current default: 'as')")
			}
			cl, err := cliClient()
			if err != nil {
				return err
			}
			res, err := cl.ProjectInit(cmd.Context(), skipBootstrap)
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("initialized %s (schema_version=%d)\n", res.DBPath, res.SchemaVersion)
			}
			if skipBootstrap {
				return nil
			}
			// Bootstrap outcome (mirrors the pre-daemon init lines):
			//   - Bootstrapped == "<workflow>"      → freshly seeded.
			//   - Bootstrapped == "bootstrap skipped: …" → non-fatal warning.
			//   - Bootstrapped == nil                → already present.
			switch {
			case res.Bootstrapped == nil:
				if !flagQuiet {
					fmt.Printf("bootstrap: workflow %q already present, skipping\n", "feature-dev-generic")
				}
			case strings.HasPrefix(*res.Bootstrapped, "bootstrap skipped:"):
				reason := strings.TrimSpace(strings.TrimPrefix(*res.Bootstrapped, "bootstrap skipped:"))
				fmt.Fprintf(os.Stderr,
					"warning: could not bootstrap default workflow: %s (re-run with --skip-bootstrap to opt out of the seed)\n",
					reason)
			default:
				if !flagQuiet {
					name := *res.Bootstrapped
					suffix := bootstrapAgentSuffix(cmd.Context(), cl, name)
					if suffix != "" {
						fmt.Printf("bootstrapped workflow %s %s\n", name, suffix)
					} else {
						fmt.Printf("bootstrapped workflow %s\n", name)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "ID prefix (reserved; currently always 'as')")
	cmd.Flags().BoolVar(&skipBootstrap, "skip-bootstrap", false,
		"do not seed the default feature-dev-generic workflow")
	return cmd
}

// bootstrapAgentSuffix renders the parenthesised `(agent NAME@VER
// installed)` suffix appended to the bootstrap success line (plan §4.6),
// reconstructed from the seeded workflow's distinct scoped agents + the
// versions reported by agent.list. Returns "" when the lookup fails or
// the workflow has no scoped agents.
func bootstrapAgentSuffix(ctx context.Context, cl *rpcclient.Client, wfName string) string {
	wf, err := cl.GetWorkflow(ctx, wfName)
	if err != nil {
		return ""
	}
	seen := map[string]bool{}
	var names []string
	for _, s := range wf.Steps {
		if s.AgentName != "" && !seen[s.AgentName] {
			seen[s.AgentName] = true
			names = append(names, s.AgentName)
		}
	}
	if len(names) == 0 {
		return ""
	}
	agents, err := cl.Agents(ctx)
	if err != nil {
		return ""
	}
	ver := make(map[string]string, len(agents))
	for _, a := range agents {
		ver[a.Name] = a.Version
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"@"+ver[n])
	}
	label := "agent"
	if len(parts) > 1 {
		label = "agents"
	}
	return "(" + label + " " + strings.Join(parts, ", ") + " installed)"
}

// newMigrateCmd: explicit migrate (also runs implicitly on every daemon
// project resolve). Reports the resulting schema version.
func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply any pending schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			// project.init(skip_bootstrap=true) is an idempotent migrate that
			// returns the resulting schema version.
			res, err := cl.ProjectInit(cmd.Context(), true)
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("schema_version=%d\n", res.SchemaVersion)
			}
			return nil
		},
	}
}

// newHistoryCmd: stub. Reserves the verb until doltlite log integration lands.
func newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <id>",
		Short: "Show change history for a task (stub — coming in v0.2)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "history: delegates to doltlite log; planned for v0.2")
			return errSilentExit1
		},
	}
}
