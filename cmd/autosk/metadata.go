package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
)

// newMetadataCmd: `autosk metadata {show,set,unset}` — read/write a task's
// free-form metadata bag. Visit counters live under the reserved `step_visits`
// key; reset them with `metadata unset step_visits` or `metadata set
// step_visits.<step> 0` (there is no dedicated reset-visits verb in v1).
func newMetadataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metadata",
		Short: "Read/write a task's free-form metadata",
		Long: "Inspect and edit a task's free-form metadata object.\n\n" +
			"The engine reserves the `step_visits` key (a step→count map used by\n" +
			"workflow visit caps); reset a counter with `metadata unset step_visits`\n" +
			"or `metadata set step_visits.<step> 0`.",
	}
	cmd.AddCommand(newMetadataShowCmd(), newMetadataSetCmd(), newMetadataUnsetCmd())
	return cmd
}

// newMetadataShowCmd: `autosk metadata show <id>` — prints the metadata object
// (pretty by default; compact with --json). A read verb: prints regardless of
// --quiet.
func newMetadataShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task's metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.GetTask(cmd.Context(), args[0])
			if err != nil {
				return mapMetadataNotFound(err, args[0])
			}
			return printMetadata(t.Metadata)
		},
	}
}

// newMetadataSetCmd: `autosk metadata set <id> <dotted.key> <value>` — sets one
// dot-path key. `value` is parsed as a JSON literal (number/bool/null/object/
// array/quoted string), falling back to a bare string when it is not valid JSON.
func newMetadataSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <id> <dotted.key> <value>",
		Short: "Set one metadata key (dot-path), value parsed as JSON or string",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, key, raw := args[0], args[1], args[2]
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			var value any
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				value = raw // not valid JSON → treat the argument as a plain string
			}
			t, err := cl.SetTaskMetadata(cmd.Context(), id, map[string]any{key: value})
			if err != nil {
				return mapMetadataNotFound(err, id)
			}
			if flagQuiet {
				return nil
			}
			return printMetadata(t.Metadata)
		},
	}
}

// newMetadataUnsetCmd: `autosk metadata unset <id> <dotted.key>...` — removes one
// or more dot-path keys, pruning emptied parent objects.
func newMetadataUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <id> <dotted.key>...",
		Short: "Remove metadata key(s) (dot-path), pruning emptied parents",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			keys := args[1:]
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			t, err := cl.UnsetTaskMetadata(cmd.Context(), id, keys)
			if err != nil {
				return mapMetadataNotFound(err, id)
			}
			if flagQuiet {
				return nil
			}
			return printMetadata(t.Metadata)
		},
	}
}

// printMetadata writes a metadata bag as JSON: compact (one line) under --json,
// pretty (2-space indent) otherwise. A nil/absent bag renders as `{}`.
func printMetadata(meta map[string]any) error {
	if meta == nil {
		meta = map[string]any{}
	}
	var b []byte
	var err error
	if flagJSON {
		b, err = json.Marshal(meta)
	} else {
		b, err = json.MarshalIndent(meta, "", "  ")
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(b))
	return err
}

// mapMetadataNotFound turns the daemon's NOT_FOUND into a clean CLI message.
func mapMetadataNotFound(err error, id string) error {
	if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
		return errors.New("task not found: " + id)
	}
	return err
}
