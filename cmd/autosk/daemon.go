package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/compactor"
	"autosk/internal/daemon/poller"
	"autosk/internal/daemon/uds"
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

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "daemon",
		Aliases: []string{"d"},
		Short:   "Daemon: serve the multi-project pi orchestrator over a unix socket",
		Long: "autosk daemon hosts an HTTP-over-UDS API that runs autosk workflow steps.\n" +
			"It serves any number of projects from a single process; the project is\n" +
			"selected per request via X-Autosk-Cwd / X-Autosk-DB headers.\n" +
			"See docs/plans/20260518-Daemon-UDS-Plan.md.",
	}
	cmd.AddCommand(
		newDaemonServeCmd(),
		newDaemonStatusCmd(),
		newDaemonMessagesCmd(),
		newDaemonCancelCmd(),
		newDaemonListCmd(),
	)
	return cmd
}

// ---- serve --------------------------------------------------------------

func newDaemonServeCmd() *cobra.Command {
	var (
		sockPath       string
		workers        int
		grace          time.Duration
		idleTimeout    time.Duration
		pollInterval   time.Duration
		gcInterval     time.Duration
		piBin          string
		sessionDirRoot string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon (single-instance UDS listener)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Resolve socket path.
			if sockPath == "" {
				sockPath = defaultSockPath()
			}
			absSock, err := filepath.Abs(sockPath)
			if err != nil {
				return fmt.Errorf("resolve --sock: %w", err)
			}
			sockPath = absSock

			// Packages registry.
			reg, err := openPackagesRegistry()
			if err != nil {
				return fmt.Errorf("pkgregistry: %w", err)
			}
			if err := reg.EnsurePrefix(); err != nil {
				return fmt.Errorf("pkgregistry ensure prefix: %w", err)
			}

			core := buildDaemonCore(daemonCoreConfig{
				Reg:            reg,
				Workers:        workers,
				PIBin:          piBin,
				SessionDirRoot: sessionDirRoot,
				Grace:          grace,
				IdleTimeout:    idleTimeout,
				PollInterval:   pollInterval,
				GCInterval:     gcInterval,
			})
			mgr, sched, srv := core.Mgr, core.Sched, core.Srv

			if err := sched.Start(ctx); err != nil {
				return fmt.Errorf("scheduler start: %w", err)
			}

			ln, err := uds.Listen(sockPath)
			if err != nil {
				return err
			}
			httpSrv := &http.Server{
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}

			sigCh := make(chan os.Signal, 2)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			fmt.Fprintf(os.Stderr, "autosk daemon: listening on %s\n", sockPath)
			gcLabel := gcInterval.String()
			switch {
			case gcInterval == 0:
				gcLabel = compactor.DefaultInterval.String() + " (default)"
			case gcInterval < 0:
				gcLabel = "disabled"
			}
			fmt.Fprintf(os.Stderr, "autosk daemon: workers=%d poll=%s gc=%s grace=%s\n",
				workers, pollInterval, gcLabel, grace)

			serveErr := make(chan error, 1)
			go func() {
				if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					serveErr <- err
				}
				close(serveErr)
			}()

			select {
			case sig := <-sigCh:
				fmt.Fprintf(os.Stderr, "autosk daemon: %s received, shutting down\n", sig)
			case err := <-serveErr:
				if err != nil {
					return err
				}
				return nil
			}

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// Shutdown order (per docs/plans/20260518-Daemon-UDS-Plan.md §6.2):
			//   1. httpSrv.Shutdown  — stop accepting new requests.
			//   2. mgr.StopPollers   — quiesce per-project pollers so no new
			//                          daemon_runs rows are enqueued.
			//   3. sched.Stop        — cancel in-flight workers and wait for
			//                          them to flush MarkCancelled/MarkFailed.
			//   4. mgr.CloseDBs      — only now is it safe to close the
			//                          per-project doltlite stores.
			//   5. uds.Cleanup       — unlink the socket.
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: http shutdown: %v\n", err)
			}
			if err := mgr.StopPollers(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: stop pollers: %v\n", err)
			}
			if err := sched.Stop(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: scheduler stop: %v\n", err)
			}
			if err := mgr.CloseDBs(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: close dbs: %v\n", err)
			}
			if err := uds.Cleanup(sockPath); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: cleanup socket: %v\n", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sockPath, "sock", "", "unix socket path (default: $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().IntVar(&workers, "workers", 5, "max concurrent agent processes across all projects")
	cmd.Flags().DurationVar(&grace, "grace", 10*time.Second, "SIGTERM grace before SIGKILL")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 2*time.Hour, "per-turn idle timeout")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", poller.DefaultInterval, "how often each project scans work tasks")
	cmd.Flags().DurationVar(&gcInterval, "gc-interval", 0, "how often each project runs doltlite chunk-store GC (0=default 30m, <0=disabled)")
	cmd.Flags().StringVar(&piBin, "pi-bin", "", "pi binary (default: 'pi' on PATH)")
	cmd.Flags().StringVar(&sessionDirRoot, "session-dir-root", "", "literal parent dir for per-job session subdirs, shared across projects (default: <projectRoot>/.autosk/sessions)")
	return cmd
}

// ---- client commands -----------------------------------------------------

// daemonClient encapsulates the http.Client that talks to the daemon
// over UDS and the per-request X-Autosk-Cwd / X-Autosk-DB headers.
type daemonClient struct {
	sock string
	cwd  string
	cli  *http.Client
}

// addClientFlags wires the per-subcommand --sock / --cwd flags. The
// global --db flag (cmd/autosk/main.go) is reused as the X-Autosk-DB
// override.
func addClientFlags(cmd *cobra.Command, c *daemonClient) {
	cmd.Flags().StringVar(&c.sock, "sock", "", "unix socket path (default: $AUTOSK_SOCK or ~/.autosk/daemon.sock)")
	cmd.Flags().StringVar(&c.cwd, "cwd", "", "project root for X-Autosk-Cwd (default: current dir)")
}

// resolve fills in defaults and constructs the underlying http.Client.
func (c *daemonClient) resolve() error {
	if c.sock == "" {
		c.sock = defaultSockPath()
	}
	abs, err := filepath.Abs(c.sock)
	if err != nil {
		return fmt.Errorf("resolve --sock: %w", err)
	}
	c.sock = abs
	if c.cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		c.cwd = wd
	}
	absCwd, err := filepath.Abs(c.cwd)
	if err != nil {
		return fmt.Errorf("resolve --cwd: %w", err)
	}
	c.cwd = absCwd
	sock := c.sock
	c.cli = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		// Long timeout: SSE streams may hold the connection open.
		Timeout: 0,
	}
	return nil
}

// dbOverride returns the X-Autosk-DB value (or empty). Order:
// global --db flag, then AUTOSK_DB env.
func (c *daemonClient) dbOverride() string {
	if flagDB != "" {
		if abs, err := filepath.Abs(flagDB); err == nil {
			return abs
		}
		return flagDB
	}
	if env := os.Getenv("AUTOSK_DB"); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
		return env
	}
	return ""
}

// request fires one HTTP request with the project headers attached.
// Body is JSON-marshalled when non-nil.
func (c *daemonClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if c.cli == nil {
		if err := c.resolve(); err != nil {
			return nil, err
		}
	}
	var br io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		br = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://autosk"+path, br)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Autosk-Cwd", c.cwd)
	if db := c.dbOverride(); db != "" {
		req.Header.Set("X-Autosk-DB", db)
	}
	return c.cli.Do(req)
}

// requestNoCwd is for endpoints that explicitly opt into the daemon-
// level (cross-project) view, like /v1/healthz?all=true.
func (c *daemonClient) requestNoCwd(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if c.cli == nil {
		if err := c.resolve(); err != nil {
			return nil, err
		}
	}
	var br io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		br = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://autosk"+path, br)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.cli.Do(req)
}

// decodeResponse parses a 2xx JSON body into out; otherwise returns a
// formatted error using ErrorResponse (including any structured details).
func decodeResponse(resp *http.Response, out any) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var er api.ErrorResponse
		if err := json.Unmarshal(body, &er); err == nil && er.Error != "" {
			return fmt.Errorf("daemon (HTTP %d): %s%s", resp.StatusCode, er.Error, formatDetails(er.Details))
		}
		return fmt.Errorf("daemon (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// formatDetails renders an ErrorResponse.Details map as "\n  key: value"
// lines so the user sees diagnostic context (e.g. cwd, hint).
func formatDetails(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "\n  %s: %v", k, d[k])
	}
	return sb.String()
}

// printJobTable renders a list of jobs as a compact ASCII table.
func printJobTable(jobs []api.JobResponse) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tSTATUS\tTASK\tSTEP\tDURATION\tERROR")
	for _, j := range jobs {
		dur := ""
		if j.DurationMS > 0 {
			dur = time.Duration(j.DurationMS * int64(time.Millisecond)).Round(time.Second).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			j.JobID, j.Status, dashEmpty(j.TaskID), dashEmpty(j.StepID),
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

// ---- status --------------------------------------------------------------

func newDaemonStatusCmd() *cobra.Command {
	var client daemonClient
	cmd := &cobra.Command{
		Use:   "status <job-id>",
		Short: "Show a single job's status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := client.request(cmd.Context(), "GET", "/v1/jobs/"+args[0], nil)
			if err != nil {
				return err
			}
			var job api.JobResponse
			if err := decodeResponse(resp, &job); err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(job)
			}
			printJobDetail(job)
			return nil
		},
	}
	addClientFlags(cmd, &client)
	return cmd
}

func printJobDetail(j api.JobResponse) {
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
		client daemonClient
		limit  int
		full   bool
	)
	cmd := &cobra.Command{
		Use:   "messages <job-id>",
		Short: "Tail recent session events for a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := "?"
			if full {
				q += "full=true"
			} else {
				q += "limit=" + strconv.Itoa(limit)
			}
			resp, err := client.request(cmd.Context(), "GET", "/v1/jobs/"+args[0]+"/messages"+q, nil)
			if err != nil {
				return err
			}
			var out api.MessagesResponse
			if err := decodeResponse(resp, &out); err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			for _, e := range out.Events {
				printEvent(e)
			}
			if out.Truncated {
				fmt.Fprintln(os.Stderr, "(transcript truncated; use --full)")
			}
			return nil
		},
	}
	addClientFlags(cmd, &client)
	cmd.Flags().IntVar(&limit, "limit", 20, "max events (1..500); ignored with --full")
	cmd.Flags().BoolVar(&full, "full", false, "fetch the entire transcript")
	return cmd
}

func printEvent(e api.MessageEvent) {
	// Local-TZ HH:MM:SS for the operator. The JSON wire shape (the
	// daemon HTTP API in internal/daemon/api/types.go) keeps RFC3339
	// via stdlib time.Time marshaling — that does NOT route through
	// timeformat.
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
	var client daemonClient
	cmd := &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a running or queued job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := client.request(cmd.Context(), "DELETE", "/v1/jobs/"+args[0], nil)
			if err != nil {
				return err
			}
			var job api.JobResponse
			if err := decodeResponse(resp, &job); err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(job)
			}
			fmt.Printf("%s: %s\n", job.JobID, job.Status)
			return nil
		},
	}
	addClientFlags(cmd, &client)
	return cmd
}

// ---- list ---------------------------------------------------------------

func newDaemonListCmd() *cobra.Command {
	var (
		client      daemonClient
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
			if allProjects {
				// Aggregated view: hit /v1/healthz?all=true and print
				// the per-project summary. (Until we add a true
				// cross-project listing, this gives the operator a
				// clear "who's loaded, who's busy" view.)
				resp, err := client.requestNoCwd(cmd.Context(), "GET", "/v1/healthz?all=true", nil)
				if err != nil {
					return err
				}
				var h api.HealthResponse
				if err := decodeResponse(resp, &h); err != nil {
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
			q := []string{}
			if statuses != "" {
				q = append(q, "status="+statuses)
			}
			if taskID != "" {
				q = append(q, "task_id="+taskID)
			}
			if limit > 0 {
				q = append(q, "limit="+strconv.Itoa(limit))
			}
			path := "/v1/jobs"
			if len(q) > 0 {
				path += "?" + strings.Join(q, "&")
			}
			resp, err := client.request(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}
			var out api.ListResponse
			if err := decodeResponse(resp, &out); err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			if len(out.Jobs) == 0 {
				fmt.Fprintln(os.Stderr, "(no jobs)")
				return nil
			}
			printJobTable(out.Jobs)
			return nil
		},
	}
	addClientFlags(cmd, &client)
	cmd.Flags().StringVar(&statuses, "status", "", "comma-separated list (queued,running,done,failed,cancel) or 'all'")
	cmd.Flags().StringVar(&taskID, "task-id", "", "filter by autosk task id")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	cmd.Flags().BoolVar(&allProjects, "all-projects", false, "show every loaded project (aggregated health), not the scoped job list")
	return cmd
}
