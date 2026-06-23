package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/api"
	"autosk/internal/timeformat"
)

// newSessionCmd is the session inspection/control group. A session is one
// invocation of an agent's onRun for one task step (the v2 replacement for the
// v1 daemon job). These verbs are JSON-RPC clients of autoskd's session.*
// methods; autoskd is auto-spawned on first use.
func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sess"},
		Short:   "Inspect and control agent sessions (client of autoskd over UDS)",
	}
	cmd.AddCommand(
		newSessionListCmd(),
		newSessionGetCmd(),
		newSessionTranscriptCmd(),
		newSessionAbortCmd(),
		newSessionInputCmd(),
	)
	return cmd
}

func newSessionListCmd() *cobra.Command {
	var taskID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions (optionally scoped to one task)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			sessions, err := cl.Sessions(cmd.Context(), taskID)
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(sessions)
			}
			if len(sessions) == 0 {
				fmt.Fprintln(os.Stderr, "(no sessions)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SESSION\tTASK\tSTEP\tAGENT\tSTATUS\tERROR")
			for _, s := range sessions {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.ID, dashEmpty(s.TaskID), dashEmpty(s.Step), dashEmpty(s.Agent),
					s.Status, trimError(s.Error))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&taskID, "task", "", "filter by task id")
	return cmd
}

func newSessionGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <session-id>",
		Short: "Show a single session's metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			s, err := cl.GetSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(s)
			}
			fmt.Printf("session:  %s\n", s.ID)
			fmt.Printf("task:     %s\n", dashEmpty(s.TaskID))
			fmt.Printf("workflow: %s\n", dashEmpty(s.Workflow))
			fmt.Printf("step:     %s\n", dashEmpty(s.Step))
			fmt.Printf("agent:    %s\n", dashEmpty(s.Agent))
			fmt.Printf("status:   %s\n", s.Status)
			if s.Error != "" {
				fmt.Printf("error:    %s\n", s.Error)
			}
			if s.StartedAt != nil {
				fmt.Printf("started:  %s\n", timeformat.FormatDateTime(*s.StartedAt))
			}
			if s.EndedAt != nil {
				fmt.Printf("ended:    %s\n", timeformat.FormatDateTime(*s.EndedAt))
			}
			return nil
		},
	}
}

func newSessionTranscriptCmd() *cobra.Command {
	var (
		fromLine int
		limit    int
	)
	cmd := &cobra.Command{
		Use:     "transcript <session-id>",
		Aliases: []string{"messages", "log"},
		Short:   "Print a session's pi-format transcript",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.Transcript(cmd.Context(), args[0], fromLine, limit)
			if err != nil {
				return err
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(res)
			}
			for _, line := range res.Entries {
				printTranscriptLine(line)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&fromLine, "from-line", 0, "1-based line to start from (header is line 1)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max lines (0 = no limit)")
	return cmd
}

func newSessionAbortCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abort <session-id>",
		Short: "Abort a running session (parks the task to human)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := cl.SessionAbort(cmd.Context(), args[0]); err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("aborted %s\n", args[0])
			}
			return nil
		},
	}
}

func newSessionInputCmd() *cobra.Command {
	var followup bool
	cmd := &cobra.Command{
		Use:   "input <session-id> <message>",
		Short: "Send a steer (or --followup) message to a live session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := "steer"
			if followup {
				kind = "followup"
			}
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := cl.SessionInput(cmd.Context(), args[0], args[1], kind); err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("%s: %s sent\n", args[0], kind)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&followup, "followup", false, "queue the message after the current turn instead of steering mid-turn")
	return cmd
}

// printTranscriptLine renders one pi-format transcript line for the human view.
func printTranscriptLine(line api.TranscriptLine) {
	switch line.Type {
	case "session":
		fmt.Printf("== session %s — %s/%s (%s) ==\n", line.ID, line.Workflow, line.Step, line.Agent)
	case "message":
		if line.Message == nil {
			return
		}
		m := line.Message
		switch m.Role {
		case "user":
			fmt.Printf("user      %s\n", oneLine(m.Text()))
		case "assistant":
			for _, b := range m.Blocks() {
				switch b.Type {
				case "text":
					fmt.Printf("assistant %s\n", oneLine(b.Text))
				case "thinking":
					fmt.Printf("thinking  %s\n", oneLine(b.Thinking))
				case "toolCall":
					fmt.Printf("tool      %s\n", b.Name)
				}
			}
		case "toolResult":
			marker := "ok"
			if m.IsError {
				marker = "ERR"
			}
			fmt.Printf("result    %s (%s)\n", m.ToolName, marker)
		}
	case "custom":
		fmt.Printf("%-9s %s\n", strings.TrimPrefix(line.CustomType, "autosk:"), string(line.Data))
	}
}

func dashEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncRunes shortens s to at most n runes, appending ellipsis when it
// had to cut. Rune-safe: slices on a []rune so a multi-byte UTF-8
// sequence is never split mid-character (error strings can carry
// non-ASCII even under the English-text rule).
func truncRunes(s string, n int, ellipsis string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := n - len([]rune(ellipsis))
	if cut < 0 {
		cut = 0
	}
	return string(r[:cut]) + ellipsis
}

func trimError(s string) string {
	return truncRunes(s, 50, "…")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return truncRunes(s, 200, "...")
}
