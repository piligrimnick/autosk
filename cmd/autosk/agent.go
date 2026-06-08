package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/daemon/rpcclient"
	"autosk/internal/timeformat"
)

// envAgentName is the env var that names the agent invoking the CLI.
// Defaults to "human" when unset.
const envAgentName = "AUTOSK_AGENT"

// callerAgentName returns the name of the agent the CLI is running as.
// "human" is the default. Used to fill the write verbs' `caller` /
// comment-author fields (the daemon ensures the agents row).
func callerAgentName() string {
	name := strings.TrimSpace(os.Getenv(envAgentName))
	if name == "" {
		return "human"
	}
	return name
}

// agentFromWire projects a daemon agent.list view onto the storage-shaped
// agent.Agent the CLI renderers consume (DB columns only; package metadata is
// layered on client-side from the local registry).
func agentFromWire(w rpcclient.Agent) agent.Agent {
	return agent.Agent{
		ID:        w.ID,
		Name:      w.Name,
		IsHuman:   w.IsHuman,
		CreatedAt: w.CreatedAt,
	}
}

// ---- root command --------------------------------------------------------

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agents (install/uninstall/list/show npm-package agents)",
		Long: "Agents are named actors (humans or background workers) that can own\n" +
			"autosk tasks. The DB stores only id + name + is_human; executable\n" +
			"config (model, thinking, first-message prompt, optional custom runner)\n" +
			"lives in npm packages installed into ~/.autosk/packages/ via the agent\n" +
			"install command. See docs/plans/20260518-Agent-Packages.md.",
	}
	cmd.AddCommand(
		newAgentInstallCmd(),
		newAgentUninstallCmd(),
		newAgentListCmd(),
		newAgentShowCmd(),
		newAgentRuntimeCmd(),
	)
	return cmd
}

// newAgentInstallCmd: `autosk agent install <pkg-or-path> [--version SPEC]`.
//
// The daemon owns the DB and performs the npm install + agents-row commit
// (agent.install RPC). In local mode it installs into the same packages prefix
// the CLI reads, so the rich output (kind / install dir / version) is layered
// on client-side from the shared registry.
//
// `<pkg-or-path>` is either an npm registry name (e.g. `@autosk/dev`)
// or a local file path starting with `./`, `../`, `/` or `~/` (e.g.
// `./agents/developer`). For local installs, the registered agent name
// is read from the package's package.json `name` field; --version is
// ignored.
func newAgentInstallCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "install <npm-name-or-path>",
		Short: "Install an npm package as an autosk agent",
		Long: "Installs the given npm package or local-path package into the\n" +
			"global agents prefix (~/.autosk/packages by default) and registers\n" +
			"it as an agent for workflows. The agent's name in the DB is the\n" +
			"package's package.json `name` field. Requires `npm` on PATH at\n" +
			"install time only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := strings.TrimSpace(args[0])
			ctx := cmd.Context()

			name, spec, err := resolveInstallSpec(arg, version)
			if err != nil {
				return err
			}

			cl, err := writeClient(ctx)
			if err != nil {
				return err
			}

			// Registry install → pass name@version; local-path install →
			// pass the resolved absolute directory as the explicit spec.
			installVersion, installSpec := "", ""
			if isLocalPath(arg) {
				installSpec = spec
			} else {
				installVersion = version
			}
			created, err := cl.AgentInstall(ctx, name, installVersion, installSpec)
			if err != nil {
				return err
			}

			// Layer package metadata for the rich output from the (shared)
			// local registry the daemon just installed into.
			reg, regErr := openPackagesRegistry()
			var (
				entry pkgregistry.Entry
				cfg   pkgregistry.PackageConfig
			)
			if regErr == nil {
				entry, _ = reg.Get(name)
				cfg, _ = reg.Resolve(name)
			}

			if flagQuiet {
				return nil
			}
			kind := "standard (pi-spawn)"
			if cfg.Runner != "" {
				kind = "custom runner: " + cfg.Runner
			}
			if flagJSON {
				out := map[string]any{
					"agent":   toJSON(agentFromWire(created)),
					"package": entryToJSON(entry),
					"kind":    kind,
				}
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			ver := entry.Version
			if ver == "" {
				ver = created.Version
			}
			fmt.Printf("installed %s@%s\n", name, ver)
			fmt.Printf("kind:       %s\n", kind)
			fmt.Printf("agent_id:   %s\n", created.ID)
			if regErr == nil {
				fmt.Printf("install:    %s\n", reg.PackageInstallDir(name))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "npm version spec (default: latest)")
	return cmd
}

// newAgentUninstallCmd: `autosk agent uninstall <pkg>`.
//
// Delegates to the daemon's agent.uninstall, which refuses if any workflow step
// references the agent (unless --force) and shells `npm uninstall`. Does NOT
// delete the agents row (workflows already reference it); users wanting a hard
// remove can `autosk sql --write` directly.
func newAgentUninstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "uninstall <npm-name>",
		Short: "Uninstall an agent package",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			ctx := cmd.Context()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			if err := cl.AgentUninstall(ctx, name, force); err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("uninstalled %s\n", name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "uninstall even if the agent is referenced by a workflow step")
	return cmd
}

func newAgentListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed agents (union of DB rows + installed packages)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			wireAgents, err := cl.Agents(ctx)
			if err != nil {
				return err
			}
			dbRows := make([]agent.Agent, len(wireAgents))
			for i, a := range wireAgents {
				dbRows[i] = agentFromWire(a)
			}
			reg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			var entries []pkgregistry.Entry
			if e, err := reg.List(); err == nil {
				entries = e
			}
			return emitAgentUnion(dbRows, entries)
		},
	}
	return cmd
}

func newAgentShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one agent (registry + DB)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx := cmd.Context()
			cl, err := readClient(ctx)
			if err != nil {
				return err
			}
			wireAgents, err := cl.Agents(ctx)
			if err != nil {
				return err
			}
			var (
				a    agent.Agent
				inDB bool
			)
			for _, w := range wireAgents {
				if w.Name == name {
					a = agentFromWire(w)
					inDB = true
					break
				}
			}
			reg, regErr := openPackagesRegistry()
			var (
				entry  pkgregistry.Entry
				cfg    pkgregistry.PackageConfig
				gotPkg bool
			)
			if regErr == nil {
				if e, err := reg.Get(name); err == nil {
					entry = e
					gotPkg = true
					if c, err := reg.Resolve(name); err == nil {
						cfg = c
					}
				}
			}
			if !inDB && !gotPkg {
				return fmt.Errorf("agent not found: %s", name)
			}
			return emitAgentDetailed(name, a, inDB, entry, cfg, gotPkg)
		},
	}
	return cmd
}

// ---- runtime subcommands -------------------------------------------------

func newAgentRuntimeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage the Node bootstrapper used by custom-runner agents",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Eagerly install " + pkgregistry.RuntimePackageName,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := openPackagesRegistry()
				if err != nil {
					return err
				}
				if err := reg.EnsurePrefix(); err != nil {
					return err
				}
				if err := reg.EnsureRuntime(cmd.Context(), ""); err != nil {
					return err
				}
				if !flagQuiet {
					fmt.Printf("runtime installed at %s\n", reg.PackageInstallDir(pkgregistry.RuntimePackageName))
				}
				return nil
			},
		},
	)
	return cmd
}

// ---- render --------------------------------------------------------------

// agentJSON is the JSON wire shape for `agent install` and as one
// element of the array returned by `agent list --json`.
type agentJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsHuman   bool   `json:"is_human"`
	CreatedAt string `json:"created_at"`
}

func toJSON(a agent.Agent) agentJSON {
	return agentJSON{
		ID:        a.ID,
		Name:      a.Name,
		IsHuman:   a.IsHuman,
		CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

type entryJSON struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	InstalledAt string `json:"installed_at"`
}

func entryToJSON(e pkgregistry.Entry) entryJSON {
	return entryJSON{
		Name:        e.Name,
		Version:     e.Version,
		InstalledAt: e.InstalledAt.Format(time.RFC3339),
	}
}

// emitAgentUnion prints the merged view of DB rows + installed packages.
//
//	NAME                  SOURCE       VERSION  IN_DB
//	human                 builtin      -        yes
//	@autosk/developer     package      0.3.1    yes
//	@autosk/orphan        package      0.1.0    no
//	@org/lostagent        db_only      -        yes
func emitAgentUnion(dbRows []agent.Agent, entries []pkgregistry.Entry) error {
	type row struct {
		Name    string `json:"name"`
		Source  string `json:"source"` // builtin|package|db_only
		Version string `json:"version,omitempty"`
		InDB    bool   `json:"in_db"`
	}
	dbSet := make(map[string]agent.Agent, len(dbRows))
	for _, a := range dbRows {
		dbSet[a.Name] = a
	}
	regSet := make(map[string]pkgregistry.Entry, len(entries))
	for _, e := range entries {
		regSet[e.Name] = e
	}

	rows := make([]row, 0, len(dbSet)+len(regSet))
	for name, a := range dbSet {
		r := row{Name: name, InDB: true}
		switch {
		case a.IsHuman || name == agent.HumanAgentName:
			r.Source = "builtin"
		default:
			if e, ok := regSet[name]; ok {
				r.Source = "package"
				r.Version = e.Version
			} else {
				r.Source = "db_only"
			}
		}
		rows = append(rows, r)
	}
	for name, e := range regSet {
		if _, ok := dbSet[name]; ok {
			continue
		}
		rows = append(rows, row{Name: name, Source: "package", Version: e.Version, InDB: false})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "(no agents)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tVERSION\tIN_DB")
	for _, r := range rows {
		ver := r.Version
		if ver == "" {
			ver = "-"
		}
		inDB := "no"
		if r.InDB {
			inDB = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.Source, ver, inDB)
	}
	return w.Flush()
}

func emitAgentDetailed(name string, a agent.Agent, inDB bool, e pkgregistry.Entry, cfg pkgregistry.PackageConfig, hasPkg bool) error {
	if flagJSON {
		out := map[string]any{
			"name":  name,
			"in_db": inDB,
		}
		if inDB {
			out["agent"] = toJSON(a)
		}
		if hasPkg {
			out["package"] = entryToJSON(e)
			out["runner"] = cfg.Runner
			out["install_dir"] = cfg.InstallDir
			out["model"] = cfg.Model
			out["thinking"] = cfg.Thinking
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if flagQuiet {
		return nil
	}
	fmt.Printf("name:       %s\n", name)
	if inDB {
		fmt.Printf("agent_id:   %s\n", a.ID)
		fmt.Printf("is_human:   %t\n", a.IsHuman)
		// Text output uses operator's local TZ; toJSON keeps RFC3339 UTC.
		fmt.Printf("created_at: %s\n", timeformat.FormatDateTime(a.CreatedAt))
	} else {
		fmt.Println("(not in DB)")
	}
	if hasPkg {
		fmt.Printf("version:    %s\n", e.Version)
		fmt.Printf("install:    %s\n", cfg.InstallDir)
		if cfg.Runner != "" {
			fmt.Printf("runner:     %s\n", cfg.Runner)
		} else {
			fmt.Println("runner:     (standard pi-spawn)")
			if cfg.Model != "" {
				fmt.Printf("model:      %s\n", cfg.Model)
			}
			if cfg.Thinking != "" {
				fmt.Printf("thinking:   %s\n", cfg.Thinking)
			}
		}
	} else if name != agent.HumanAgentName {
		fmt.Println("package:    (not installed; run `autosk agent install " + name + "`)")
	}
	return nil
}
