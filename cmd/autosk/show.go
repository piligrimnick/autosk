package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/render"
)

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			w, err := cl.GetTask(cmd.Context(), args[0])
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return errors.New("task not found: " + args[0])
				}
				return err
			}
			// The wire view carries workflow/step names + derived
			// blocked/blocked_by/blocks/comment_count; render directly (a read
			// verb prints regardless of --quiet).
			opts := []render.Option{wireBlockedOpt(w), render.WithCommentCount(w.CommentCount)}
			t := taskFromWire(w)
			if flagJSON {
				return render.TaskJSONTo(os.Stdout, t, opts...)
			}
			return render.Task(os.Stdout, t, opts...)
		},
	}
	return cmd
}
