package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/store/doltlite"
)

// envAgentName is the env var that names the agent invoking the CLI.
// Defaults to "human" when unset.
const envAgentName = "AUTOSK_AGENT"

// callerAgentName returns the name of the agent the CLI is running as.
// "human" is the default.
func callerAgentName() string {
	name := strings.TrimSpace(os.Getenv(envAgentName))
	if name == "" {
		return "human"
	}
	return name
}

// resolveCallerAgent ensures the caller's agent row exists in the DB and
// returns it. Used by future write commands to fill author_id columns.
// Lazy semantics:
//   - If $AUTOSK_AGENT names an existing agent, return it.
//   - If it names a new one, insert (is_human=true only for "human").
//   - When unset, return the seeded `human` row.
func resolveCallerAgent(ctx context.Context, ag *agent.Store) (agent.Agent, error) {
	return ag.EnsureByName(ctx, callerAgentName())
}

// agentStoreFromCmd opens the project DB and returns an agent.Store +
// the doltlite handle (so commits can be issued) + a close func.
func agentStoreFromCmd(ctx context.Context, writeOK bool) (*agent.Store, *doltlite.Store, func(), error) {
	s, closeFn, err := openStore(ctx, writeOK)
	if err != nil {
		return nil, nil, nil, err
	}
	dl, ok := s.(*doltlite.Store)
	if !ok {
		closeFn()
		return nil, nil, nil, errors.New("agent CLI: doltlite store required")
	}
	return agent.New(dl.DB()), dl, closeFn, nil
}

// ---- root command --------------------------------------------------------

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agents (entities that can own a task)",
		Long: "Agents are named actors (humans or background workers) that can own\n" +
			"autosk tasks. The DB stores only id + name + is_human; executable\n" +
			"config (model, thinking, system prompt) lives in .autosk/agents/<name>.toml.",
	}
	cmd.AddCommand(
		newAgentCreateCmd(),
		newAgentListCmd(),
		newAgentShowCmd(),
	)
	return cmd
}

func newAgentCreateCmd() *cobra.Command {
	var human bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ag, dl, closeFn, err := agentStoreFromCmd(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			isHuman := human || name == "human"
			created, err := ag.Create(cmd.Context(), name, isHuman)
			if err != nil {
				if errors.Is(err, agent.ErrAlreadyExist) {
					return fmt.Errorf("agent %q already exists", name)
				}
				return err
			}
			_ = dl.DoltCommit(cmd.Context(), "agent create "+created.Name)
			return emitAgent(created)
		},
	}
	cmd.Flags().BoolVar(&human, "human", false, "mark this agent as a human (sets is_human=1)")
	return cmd
}

func newAgentListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ag, _, closeFn, err := agentStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			agents, err := ag.List(cmd.Context())
			if err != nil {
				return err
			}
			return emitAgents(agents)
		},
	}
	return cmd
}

func newAgentShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ag, _, closeFn, err := agentStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			a, err := ag.GetByName(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, agent.ErrNotFound) {
					return fmt.Errorf("agent not found: %s", args[0])
				}
				return err
			}
			return emitAgent(a)
		},
	}
	return cmd
}

// ---- render --------------------------------------------------------------

// agentJSON is the JSON wire shape for `agent show/create` and as one
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

func emitAgent(a agent.Agent) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toJSON(a))
	}
	fmt.Printf("id:         %s\n", a.ID)
	fmt.Printf("name:       %s\n", a.Name)
	fmt.Printf("is_human:   %t\n", a.IsHuman)
	fmt.Printf("created_at: %s\n", a.CreatedAt.Format("2006-01-02T15:04:05Z"))
	return nil
}

func emitAgents(agents []agent.Agent) error {
	if flagJSON {
		out := make([]agentJSON, len(agents))
		for i, a := range agents {
			out[i] = toJSON(a)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(agents) == 0 {
		fmt.Fprintln(os.Stderr, "(no agents)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tHUMAN\tCREATED")
	for _, a := range agents {
		marker := "no"
		if a.IsHuman {
			marker = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			a.ID, a.Name, marker, a.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	return w.Flush()
}
