package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/comments"
	"autosk/internal/store/doltlite"
)

func newCommentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comment",
		Short: "Append-only comments on a task",
		Long: "Comments are the cross-agent channel for a task: the workflow engine\n" +
			"surfaces every prior comment at the top of each step's prompt.\n" +
			"Authors default to $AUTOSK_AGENT (human if unset).",
	}
	cmd.AddCommand(
		newCommentAddCmd(),
		newCommentListCmd(),
	)
	return cmd
}

func newCommentAddCmd() *cobra.Command {
	var author string
	cmd := &cobra.Command{
		Use:   "add <task-id> [text]",
		Short: "Append a comment to a task (text may also be piped via stdin)",
		Long: "Append a comment to a task.\n\n" +
			"Text can be passed positionally; if omitted (or `-`), it is read\n" +
			"from stdin. The author defaults to $AUTOSK_AGENT (human).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]
			var text string
			switch {
			case len(args) == 2 && args[1] != "-":
				text = args[1]
			default:
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				text = string(b)
			}
			text = strings.TrimRight(text, "\n")
			if strings.TrimSpace(text) == "" {
				return errors.New("comment text is empty")
			}

			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			ag := agent.New(dl.DB())
			cs := comments.New(dl.DB())

			// Resolve author: --author NAME overrides $AUTOSK_AGENT.
			authorName := strings.TrimSpace(author)
			if authorName == "" {
				authorName = callerAgentName()
			}
			aRow, err := ag.EnsureByName(cmd.Context(), authorName)
			if err != nil {
				return fmt.Errorf("ensure author %q: %w", authorName, err)
			}
			c, err := cs.Add(cmd.Context(), taskID, aRow.ID, text)
			if err != nil {
				return err
			}
			_ = dl.DoltCommit(cmd.Context(), "comment add "+taskID)
			return emitComment(c)
		},
	}
	cmd.Flags().StringVar(&author, "author", "", "agent name to record as author (default $AUTOSK_AGENT or 'human')")
	return cmd
}

func newCommentListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <task-id>",
		Short: "List comments on a task (oldest first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			cs := comments.New(dl.DB())
			list, err := cs.ListByTask(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emitComments(list)
		},
	}
	return cmd
}

// ---- render --------------------------------------------------------------

type commentJSON struct {
	ID         int64  `json:"id"`
	TaskID     string `json:"task_id"`
	AuthorID   string `json:"author_id"`
	AuthorName string `json:"author"`
	Text       string `json:"text"`
	CreatedAt  string `json:"created_at"`
}

func toCommentJSON(c comments.Comment) commentJSON {
	return commentJSON{
		ID:         c.ID,
		TaskID:     c.TaskID,
		AuthorID:   c.AuthorID,
		AuthorName: c.AuthorName,
		Text:       c.Text,
		CreatedAt:  c.CreatedAt.Format(time.RFC3339),
	}
}

func emitComment(c comments.Comment) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toCommentJSON(c))
	}
	fmt.Printf("[%s@%s] (id=%d):\n%s\n",
		c.AuthorName, c.CreatedAt.Format(time.RFC3339), c.ID, c.Text)
	return nil
}

func emitComments(cs []comments.Comment) error {
	if flagJSON {
		out := make([]commentJSON, len(cs))
		for i, c := range cs {
			out[i] = toCommentJSON(c)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(cs) == 0 {
		fmt.Fprintln(os.Stderr, "(no comments)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tAUTHOR\tCREATED\tTEXT")
	for _, c := range cs {
		text := strings.ReplaceAll(c.Text, "\n", " ")
		if len(text) > 80 {
			text = text[:77] + "…"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
			c.ID, c.AuthorName, c.CreatedAt.Format("2006-01-02 15:04:05"), text)
	}
	return w.Flush()
}
