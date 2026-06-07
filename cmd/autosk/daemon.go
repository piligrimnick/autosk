package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/rpcclient"
	"autosk/internal/timeformat"
)

// defaultSockPath returns the default ~/.autosk/daemon.sock for the
// current user. Falls back to ./.autosk/daemon.sock if HOME is unset.
func defaultSockPath() string {
	if env := os.Getenv("AUTOSK_SOCK"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".autosk/daemon.sock"
	}
	return filepath.Join(home, ".autosk", "daemon.sock")
}

// newDaemonCmd builds the `autosk daemon` parent. The daemon itself is now the
// Rust `autoskd` (the Go `daemon serve` was retired in the Phase-2 cutover);
// these subcommands are pure JSON-RPC clients of autoskd, auto-spawning it on
// first use.
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "daemon",
		Aliases: []string{"d"},
		Short:   "Inspect/control daemon jobs (client of the Rust autoskd over UDS)",
		Long: "autosk daemon subcommands are JSON-RPC clients of autoskd, the Rust\n" +
			"daemon that owns .autosk/db and drives workflow steps. autoskd is\n" +
			"auto-spawned on first use (language-server style). The retired Go\n" +
			"`daemon serve` is gone; run autoskd directly for a foreground daemon.",
	}
	cmd.AddCommand(
		newDaemonStatusCmd(),
		newDaemonMessagesCmd(),
		newDaemonCancelCmd(),
		newDaemonListCmd(),
	)
	return cmd
}

// clientFlags holds the per-subcommand --sock / --cwd selectors.
type clientFlags struct {
	sock string
	cwd  string
}

func addClientFlags(cmd *cobra.Command, c *clientFlags) {
	cmd.Flags().StringVar(&c.sock, "sock", "", "autoskd unix socket (default: $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().StringVar(&c.cwd, "cwd", "", "project root selector (default: current dir)")
}

// client builds an autoskd JSON-RPC client from the selectors + the global
// --db override (or $AUTOSK_DB).
func (c *clientFlags) client() (*rpcclient.Client, error) {
	return rpcclient.New(rpcclient.Options{
		Sock:       c.sock,
		Cwd:        c.cwd,
		DBOverride: dbOverride(),
	})
}

// dbOverride resolves the X-Autosk-DB equivalent: the global --db flag, then
// $AUTOSK_DB, absolutised.
func dbOverride() string {
	pick := flagDB
	if pick == "" {
		pick = os.Getenv("AUTOSK_DB")
	}
	if pick == "" {
		return ""
	}
	if abs, err := filepath.Abs(pick); err == nil {
		return abs
	}
	return pick
}

// ---- status --------------------------------------------------------------

func newDaemonStatusCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "status <job-id>",
		Short: "Show a single job's status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cf.client()
			if err != nil {
				return err
			}
			job, err := cl.GetJob(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(job)
			}
			printJobDetail(job)
			return nil
		},
	}
	addClientFlags(cmd, &cf)
	return cmd
}

func printJobDetail(j rpcclient.Job) {
	fmt.Printf("job:          %s\n", j.JobID)
	fmt.Printf("task:         %s\n", dashEmpty(j.TaskID))
	fmt.Printf("step:         %s\n", dashEmpty(j.StepID))
	fmt.Printf("status:       %s\n", j.Status)
	if j.TransitionID != nil {
		fmt.Printf("transition:   %d\n", *j.TransitionID)
	}
	if j.Error != "" {
		fmt.Printf("error:        %s\n", j.Error)
	}
	if j.PISessionID != "" {
		fmt.Printf("pi_session:   %s\n", j.PISessionID)
	}
	if j.SessionPath != "" {
		fmt.Printf("session_path: %s\n", j.SessionPath)
	}
	if j.ExitCode != nil {
		fmt.Printf("exit_code:    %d\n", *j.ExitCode)
	}
	if j.DurationMS > 0 {
		fmt.Printf("duration:     %s\n", time.Duration(j.DurationMS*int64(time.Millisecond)).Round(time.Millisecond))
	}
	fmt.Printf("corrections:  %d/%d\n", j.CorrectionsUsed, j.MaxCorrections)
}

// ---- messages -----------------------------------------------------------

func newDaemonMessagesCmd() *cobra.Command {
	var (
		cf    clientFlags
		limit int
		full  bool
	)
	cmd := &cobra.Command{
		Use:   "messages <job-id>",
		Short: "Tail recent session events for a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cf.client()
			if err != nil {
				return err
			}
			events, err := cl.Messages(cmd.Context(), args[0], full, limit)
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"job_id": args[0],
					"events": events,
				})
			}
			for _, e := range events {
				printEvent(e)
			}
			return nil
		},
	}
	addClientFlags(cmd, &cf)
	cmd.Flags().IntVar(&limit, "limit", 20, "max events (1..500); ignored with --full")
	cmd.Flags().BoolVar(&full, "full", false, "fetch the entire transcript")
	return cmd
}

func printEvent(e api.MessageEvent) {
	// Local-TZ HH:MM:SS for the operator. The JSON wire shape stays RFC3339
	// UTC; rendering routes through timeformat per AGENTS.md.
	ts := timeformat.FormatTime(e.TS)
	switch e.Kind {
	case "assistant_text", "user_text":
		fmt.Printf("%-9s [%s] %s\n", e.Kind, ts, oneLine(e.Text))
	case "assistant_thinking":
		fmt.Printf("%-9s [%s] %s\n", "thinking", ts, oneLine(e.Text))
	case "tool_call":
		fmt.Printf("%-9s [%s] %s\n", "tool", ts, e.Name)
	case "tool_result":
		marker := "ok"
		if e.IsError {
			marker = "ERR"
		}
		fmt.Printf("%-9s [%s] %s (%s) %s\n", "result", ts, e.Name, marker, oneLine(e.Text))
	default:
		fmt.Printf("%-9s [%s]\n", e.Kind, ts)
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:197] + "..."
	}
	return s
}

// ---- cancel --------------------------------------------------------------

func newDaemonCancelCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a running or queued job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cf.client()
			if err != nil {
				return err
			}
			job, err := cl.CancelJob(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(job)
			}
			fmt.Printf("%s: %s\n", job.JobID, job.Status)
			return nil
		},
	}
	addClientFlags(cmd, &cf)
	return cmd
}

// ---- list ---------------------------------------------------------------

func newDaemonListCmd() *cobra.Command {
	var (
		cf          clientFlags
		statuses    string
		taskID      string
		limit       int
		allProjects bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs (scoped to the current project unless --all-projects)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := cf.client()
			if err != nil {
				return err
			}
			if allProjects {
				h, err := cl.HealthzAll(cmd.Context())
				if err != nil {
					return err
				}
				if flagJSON {
					return json.NewEncoder(os.Stdout).Encode(h)
				}
				if len(h.Projects) == 0 {
					fmt.Fprintln(os.Stderr, "(no projects loaded)")
					return nil
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "ROOT\tDB\tQUEUED\tRUNNING\tOPENED")
				for _, p := range h.Projects {
					fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n",
						p.Root, p.DBPath, p.Queued, p.Running,
						timeformat.FormatTime(p.OpenedAt))
				}
				_ = w.Flush()
				return nil
			}
			var sts []string
			if statuses != "" && statuses != "all" {
				for _, s := range strings.Split(statuses, ",") {
					if s = strings.TrimSpace(s); s != "" {
						sts = append(sts, s)
					}
				}
			} else if statuses == "all" {
				sts = []string{} // non-nil empty → all statuses
			}
			jobs, err := cl.Jobs(cmd.Context(), rpcclient.JobListFilter{
				Statuses: sts,
				TaskID:   taskID,
				Limit:    limit,
			})
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"jobs": jobs})
			}
			if len(jobs) == 0 {
				fmt.Fprintln(os.Stderr, "(no jobs)")
				return nil
			}
			printJobTable(jobs)
			return nil
		},
	}
	addClientFlags(cmd, &cf)
	cmd.Flags().StringVar(&statuses, "status", "", "comma-separated status filter (queued,running,done,failed,cancel) or 'all'")
	cmd.Flags().StringVar(&taskID, "task", "", "filter by task id")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = no limit)")
	cmd.Flags().BoolVar(&allProjects, "all-projects", false, "show the per-project aggregate instead of this project's jobs")
	return cmd
}

// printJobTable renders a list of jobs as a compact ASCII table.
func printJobTable(jobs []rpcclient.Job) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tSTATUS\tTASK\tSTEP\tDURATION\tERROR")
	for _, j := range jobs {
		dur := ""
		if j.DurationMS > 0 {
			dur = time.Duration(j.DurationMS * int64(time.Millisecond)).Round(time.Second).String()
		}
		step := j.StepName
		if step == "" {
			step = j.StepID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			j.JobID, j.Status, dashEmpty(j.TaskID), dashEmpty(step),
			dashEmpty(dur), trimError(j.Error))
	}
	_ = w.Flush()
}

func dashEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trimError(s string) string {
	if len(s) > 50 {
		return s[:47] + "…"
	}
	return s
}
