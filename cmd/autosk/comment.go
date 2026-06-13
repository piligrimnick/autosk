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

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/timeformat"
)

func newCommentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comment",
		Short: "Comments on a task (the cross-agent channel)",
		Long: "Comments are the cross-agent channel for a task: the workflow engine\n" +
			"surfaces every prior comment at the top of each step's prompt.\n" +
			"Authors default to $AUTOSK_AGENT (human if unset). v2 comments are\n" +
			"editable and deletable (the daemon is the sole writer).",
	}
	cmd.AddCommand(
		newCommentAddCmd(),
		newCommentListCmd(),
		newCommentEditCmd(),
		newCommentDeleteCmd(),
	)
	return cmd
}

func newCommentAddCmd() *cobra.Command {
	var author string
	cmd := &cobra.Command{
		Use:   "add <task-id> [text]",
		Short: "Append a comment to a task (text may also be piped via stdin)",
		Args:  cobra.RangeArgs(1, 2),
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
			authorName := strings.TrimSpace(author)
			if authorName == "" {
				authorName = callerAgentName()
			}

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			c, err := cl.AddComment(cmd.Context(), taskID, authorName, text)
			if err != nil {
				return err
			}
			return emitComment(c)
		},
	}
	cmd.Flags().StringVar(&author, "author", "", "agent name to record as author (default $AUTOSK_AGENT or 'human')")
	return cmd
}

func newCommentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <task-id>",
		Short: "List comments on a task (oldest first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			list, err := cl.Comments(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emitComments(list)
		},
	}
}

func newCommentEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <task-id> <comment-id> [text]",
		Short: "Rewrite a comment's text (text may also be piped via stdin)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID, commentID := args[0], args[1]
			var text string
			switch {
			case len(args) == 3 && args[2] != "-":
				text = args[2]
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
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			c, err := cl.EditComment(cmd.Context(), taskID, commentID, text)
			if err != nil {
				return err
			}
			return emitComment(c)
		},
	}
	return cmd
}

func newCommentDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <task-id> <comment-id>",
		Aliases: []string{"rm"},
		Short:   "Delete a comment",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			if err := cl.DeleteComment(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("deleted comment %s\n", args[1])
			}
			return nil
		},
	}
}

// ---- render --------------------------------------------------------------

type commentJSON struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toCommentJSON(c rpcclient.Comment) commentJSON {
	return commentJSON{
		ID:        c.ID,
		Author:    c.Author,
		Text:      c.Text,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func emitComment(c rpcclient.Comment) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toCommentJSON(c))
	}
	fmt.Printf("[%s@%s] (id=%s):\n%s\n",
		c.Author, timeformat.FormatDateTime(c.CreatedAt), c.ID, c.Text)
	return nil
}

func emitComments(cs []rpcclient.Comment) error {
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
		text := truncRunes(strings.ReplaceAll(c.Text, "\n", " "), 80, "…")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			c.ID, c.Author, timeformat.FormatDateTime(c.CreatedAt), text)
	}
	return w.Flush()
}
