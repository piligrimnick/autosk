package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/api"
	"autosk/internal/daemon/executor"
	"autosk/internal/daemon/runstore"
	"autosk/internal/daemon/scheduler"
	"autosk/internal/daemon/server"
	"autosk/internal/projectdb"
	"autosk/internal/store/doltlite"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "daemon",
		Aliases: []string{"d"},
		Short:   "Daemon: serve pi-orchestrator HTTP API; submit/get/cancel jobs",
		Long: "autosk daemon hosts an HTTP API that spawns pi --mode rpc to work on autosk tasks.\n" +
			"Submit, inspect status, and tail messages. See docs/plans/20260517-Daemon-Plan.md.",
	}
	cmd.AddCommand(
		newDaemonServeCmd(),
		newDaemonSubmitCmd(),
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
		bind         string
		tokenFile    string
		workers      int
		cwd          string
		grace        time.Duration
		idleTimeout  time.Duration
		piBin        string
		sessionDir   string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Resolve project DB.
			if cwd == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				cwd = wd
			}
			dbPath, _, err := projectdb.ResolveOrInit(cwd, flagDB)
			if err != nil {
				if errors.Is(err, projectdb.ErrNotFound) {
					return errors.New("no .autosk/db found; run `autosk init` or a write command first")
				}
				return err
			}
			tasks := doltlite.New()
			if err := tasks.Open(ctx, dbPath); err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer tasks.Close()
			if err := tasks.Migrate(ctx); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			runs := runstore.New(tasks.DB())

			// Resolve token.
			var token string
			if tokenFile != "" {
				b, err := os.ReadFile(tokenFile)
				if err != nil {
					return fmt.Errorf("read token file: %w", err)
				}
				token = strings.TrimSpace(string(b))
			}

			// Executor.
			exec := executor.New(runs, tasks, executor.DefaultFactory, executor.Config{
				PIBin:          piBin,
				SessionDirRoot: sessionDir,
				Grace:          grace,
				IdleTimeout:    idleTimeout,
			})
			sched := scheduler.New(runs, scheduler.ExecutorFunc(func(ctx context.Context, jobID string) error {
				return exec.Run(ctx, jobID)
			}), scheduler.Config{Workers: workers})
			if err := sched.Start(ctx); err != nil {
				return fmt.Errorf("scheduler start: %w", err)
			}

			srv := server.New(server.Deps{
				Runs:       runs,
				Tasks:      tasks,
				Sched:      sched,
				Workers:    workers,
				Token:      token,
				DefaultCwd: cwd,
				DBPath:     dbPath,
			})
			httpSrv := &http.Server{
				Addr:              bind,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}

			// Signal trapping for graceful shutdown.
			sigCh := make(chan os.Signal, 2)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			fmt.Fprintf(os.Stderr, "autosk daemon: listening on %s\n", bind)
			fmt.Fprintf(os.Stderr, "autosk daemon: db=%s cwd=%s workers=%d\n", dbPath, cwd, workers)
			if token != "" {
				fmt.Fprintln(os.Stderr, "autosk daemon: bearer token required (from --token-file)")
			}

			serveErr := make(chan error, 1)
			go func() {
				if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: http shutdown: %v\n", err)
			}
			if err := sched.Stop(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "autosk daemon: scheduler stop: %v\n", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1:7878", "listen address")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "if set, contents are required as Bearer token")
	cmd.Flags().IntVar(&workers, "workers", 2, "max concurrent pi processes")
	cmd.Flags().StringVar(&cwd, "cwd", "", "default cwd for jobs (default: current dir)")
	cmd.Flags().DurationVar(&grace, "grace", 10*time.Second, "SIGTERM grace before SIGKILL")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 30*time.Minute, "per-turn idle timeout")
	cmd.Flags().StringVar(&piBin, "pi-bin", "", "pi binary (default: 'pi' on PATH)")
	cmd.Flags().StringVar(&sessionDir, "session-dir", "", "parent dir for per-job session subdirs (default: <cwd>/.autosk/sessions)")
	return cmd
}

// ---- client commands -----------------------------------------------------

// daemonClient is the shared --daemon-url / --daemon-token-file plumbing.
type daemonClient struct {
	url   string
	token string
}

func addClientFlags(cmd *cobra.Command, c *daemonClient) {
	cmd.Flags().StringVar(&c.url, "daemon-url", "http://127.0.0.1:7878", "daemon base URL")
	cmd.Flags().StringVar(&c.token, "daemon-token-file", "", "Bearer token file (matches daemon --token-file)")
}

func (c daemonClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var br io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		br = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, br)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		b, err := os.ReadFile(c.token)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	return http.DefaultClient.Do(req)
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
// lines so the user sees diagnostic context (e.g. daemon_db_path, hint).
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
	fmt.Fprintln(w, "JOB\tSTATUS\tTASK\tMODEL\tDURATION\tCLOSURE\tERROR")
	for _, j := range jobs {
		dur := ""
		if j.DurationMS > 0 {
			dur = time.Duration(j.DurationMS * int64(time.Millisecond)).Round(time.Second).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.JobID, j.Status, dashEmpty(j.TaskID), dashEmpty(j.Model),
			dashEmpty(dur), dashEmpty(j.ClosureKind), trimError(j.Error))
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

// ---- submit --------------------------------------------------------------

func newDaemonSubmitCmd() *cobra.Command {
	var (
		client         daemonClient
		prompt         string
		model          string
		thinking       string
		cwd            string
		maxCorrections int
		noClaim        bool
		clientCwd      bool
	)
	cmd := &cobra.Command{
		Use:   "submit [as-id]",
		Short: "Submit a task (or ad-hoc prompt) to the daemon",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := api.SubmitRequest{
				Prompt:   prompt,
				Model:    model,
				Thinking: thinking,
				Cwd:      cwd,
			}
			if len(args) == 1 {
				req.TaskID = args[0]
			}
			if cmd.Flags().Changed("max-corrections") {
				req.MaxCorrections = &maxCorrections
			}
			if noClaim {
				v := false
				req.AutoClaim = &v
			}
			if clientCwd && req.Cwd == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				req.Cwd = wd
			}
			if req.TaskID == "" && req.Prompt == "" {
				return errors.New("provide either a task id or --prompt")
			}
			resp, err := client.request(cmd.Context(), "POST", "/v1/jobs", req)
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
			fmt.Println(job.JobID)
			return nil
		},
	}
	addClientFlags(cmd, &client)
	cmd.Flags().StringVar(&prompt, "prompt", "", "prompt text (ad-hoc; overrides task description)")
	cmd.Flags().StringVar(&model, "model", "", "pi --model")
	cmd.Flags().StringVar(&thinking, "thinking", "", "pi --thinking (off|minimal|low|medium|high|xhigh)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory for pi (default: daemon's default)")
	cmd.Flags().IntVar(&maxCorrections, "max-corrections", 3, "kickback attempts before failing")
	cmd.Flags().BoolVar(&noClaim, "no-claim", false, "do not auto-claim the autosk task")
	cmd.Flags().BoolVar(&clientCwd, "use-cwd", false, "send the client's cwd to the daemon")
	return cmd
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
	fmt.Printf("status:       %s\n", j.Status)
	if j.ClosureKind != "" {
		fmt.Printf("closure_kind: %s\n", j.ClosureKind)
	}
	if j.Error != "" {
		fmt.Printf("error:        %s\n", j.Error)
	}
	if j.Model != "" {
		fmt.Printf("model:        %s\n", j.Model)
	}
	if j.Thinking != "" {
		fmt.Printf("thinking:     %s\n", j.Thinking)
	}
	fmt.Printf("cwd:          %s\n", j.Cwd)
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
	fmt.Printf("auto_claim:   %t\n", j.AutoClaim)
	fmt.Printf("corrections:  %d/%d\n", j.CorrectionsUsed, j.MaxCorrections)
	if j.PromptPreview != "" {
		fmt.Println()
		fmt.Println(j.PromptPreview)
	}
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
	ts := ""
	if !e.TS.IsZero() {
		ts = e.TS.Format("15:04:05")
	}
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
		client   daemonClient
		statuses string
		taskID   string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringVar(&statuses, "status", "", "comma-separated list (queued,running,done,failed,cancelled) or 'all'")
	cmd.Flags().StringVar(&taskID, "task-id", "", "filter by autosk task id")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}
