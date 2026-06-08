package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/daemon/rpcclient"
	"autosk/internal/meta"
)

// newMetadataCmd is the parent for the per-task metadata CLI. Every
// mutation routes through the daemon (which owns the dotted-path descent,
// the reserved step_visits validation, the change-detecting commit gate,
// and the byte-identical commit messages); the cobra layer here parses
// the CLI flags and renders the result.
//
// See docs/plans/20260520-Step-Visit-Limits.md and docs/workflows.md.
func newMetadataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metadata",
		Short: "Read and edit a task's metadata JSON blob",
		Long: "Per-task metadata is a free-form JSON object stored alongside the\n" +
			"task. The engine reserves the top-level key `step_visits` for\n" +
			"per-step visit counters used by the max_visits cap.\n\n" +
			"Use `metadata show` to inspect; `metadata set` / `metadata unset`\n" +
			"to edit individual keys; and `metadata reset-visits` to clear the\n" +
			"engine-managed visit counter (the escape hatch for the cap).",
	}
	cmd.AddCommand(
		newMetadataShowCmd(),
		newMetadataSetCmd(),
		newMetadataUnsetCmd(),
		newMetadataResetVisitsCmd(),
	)
	return cmd
}

// ---- show ----------------------------------------------------------------

func newMetadataShowCmd() *cobra.Command {
	var visitsPretty bool
	cmd := &cobra.Command{
		Use:   "show <task-id>",
		Short: "Print a task's metadata as JSON",
		Long: "Print the task's metadata as pretty JSON. `--visits-pretty` reformats\n" +
			"the reserved `step_visits` sub-object with step names and per-step\n" +
			"max_visits caps for human consumption.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := readClient(cmd.Context())
			if err != nil {
				return err
			}
			tk, err := cl.GetTask(cmd.Context(), args[0])
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if flagQuiet {
				return nil
			}
			if visitsPretty {
				var wf *rpcclient.Workflow
				if tk.WorkflowID != "" && tk.WorkflowName != "" {
					if g, gerr := cl.GetWorkflow(cmd.Context(), tk.WorkflowName); gerr == nil {
						wf = &g
					}
				}
				return printVisitsPretty(os.Stdout, wf, tk.Metadata)
			}
			return printMetadataJSON(os.Stdout, tk.Metadata)
		},
	}
	cmd.Flags().BoolVar(&visitsPretty, "visits-pretty", false, "render step_visits using step names and max_visits caps")
	return cmd
}

// ---- set -----------------------------------------------------------------

func newMetadataSetCmd() *cobra.Command {
	var (
		key      string
		valueArg string
		jsonFile string
		hasValue bool
	)
	cmd := &cobra.Command{
		Use:   "set <task-id> --key K [--value V | --json-value FILE]",
		Short: "Set a key in the task's metadata",
		Long: "Set a key inside the task's metadata. `--key` is a dotted JSON\n" +
			"object path (e.g. `step_visits.st-abcd`, `tags`, `foo.bar.baz`).\n" +
			"Array indexing is not supported.\n\n" +
			"`--value V` parses V as a JSON literal when V starts with one of\n" +
			"`[`, `{`, `\"`, `t`, `f`, `n`, `-`, or `0`..`9`; otherwise V is\n" +
			"treated as a string. `--json-value FILE` reads a JSON value from\n" +
			"a file (or `-` for stdin). Exactly one of --value / --json-value.\n\n" +
			"Writes under the reserved `step_visits` key are validated against\n" +
			"the engine's typed shape (non-negative integer leaves).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if key == "" {
				return errors.New("--key is required")
			}
			hasValue = cmd.Flags().Changed("value")
			if hasValue == (jsonFile != "") {
				return errors.New("exactly one of --value or --json-value must be set")
			}
			parsed, err := parseSetValue(valueArg, hasValue, jsonFile)
			if err != nil {
				return err
			}

			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.MetadataSet(cmd.Context(), cliSource, args[0], key, parsed, false)
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if flagQuiet {
				return nil
			}
			if !res.Changed {
				fmt.Fprintln(os.Stderr, "(no change)")
			}
			return printMetadataJSON(os.Stdout, res.Task.Metadata)
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "dotted JSON object path (e.g. `step_visits.st-abcd`)")
	cmd.Flags().StringVar(&valueArg, "value", "", "JSON literal (or bare string when not JSON-shaped)")
	cmd.Flags().StringVar(&jsonFile, "json-value", "", "read a JSON value from this file (`-` for stdin)")
	return cmd
}

// ---- unset ---------------------------------------------------------------

func newMetadataUnsetCmd() *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "unset <task-id> --key K",
		Short: "Remove a key from a task's metadata",
		Long: "Remove the value at the dotted JSON path `--key`. Any parent\n" +
			"objects that become empty are also removed, so the metadata\n" +
			"column round-trips back to SQL NULL once everything is cleared.\n\n" +
			"No-op (exit 0) when the key isn't present.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if key == "" {
				return errors.New("--key is required")
			}
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.MetadataUnset(cmd.Context(), args[0], key)
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if flagQuiet {
				return nil
			}
			if !res.Changed {
				fmt.Fprintln(os.Stderr, "(no change)")
			}
			return printMetadataJSON(os.Stdout, res.Task.Metadata)
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "dotted JSON object path to remove")
	return cmd
}

// ---- reset-visits --------------------------------------------------------

func newMetadataResetVisitsCmd() *cobra.Command {
	var (
		stepName string
		stepID   string
	)
	cmd := &cobra.Command{
		Use:   "reset-visits <task-id> [--step NAME | --step-id ID]",
		Short: "Clear engine-managed step visit counters",
		Long: "Clear the reserved `step_visits` sub-object on a task's metadata.\n\n" +
			"With no flags, removes the entire counter map. With --step NAME,\n" +
			"resolves (workflow_id, NAME) → step_id and removes only that\n" +
			"entry; the task must have a workflow_id. With --step-id ID,\n" +
			"removes the entry for ID directly (useful for orphaned counters).\n\n" +
			"This is the human escape hatch from `step_max_visits_exceeded`.\n" +
			"After resetting, `autosk resume <id>` returns the task to the\n" +
			"step it was parked on.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stepName != "" && stepID != "" {
				return errors.New("--step and --step-id are mutually exclusive")
			}
			cl, err := writeClient(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.MetadataResetVisits(cmd.Context(), args[0], stepName, stepID)
			if err != nil {
				if apiErr, ok := rpcclient.IsAPIError(err); ok && apiErr.Code == rpcclient.CodeNotFound {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if flagQuiet {
				return nil
			}
			if !res.Changed {
				fmt.Fprintln(os.Stderr, "(no change)")
			}
			return printMetadataJSON(os.Stdout, res.Task.Metadata)
		},
	}
	cmd.Flags().StringVar(&stepName, "step", "", "reset only this step's counter (requires task.workflow_id)")
	cmd.Flags().StringVar(&stepID, "step-id", "", "reset only this step_id's counter (useful for orphaned counters)")
	return cmd
}

// ---- internals -----------------------------------------------------------

// parseSetValue resolves --value or --json-value into a parsed Go value.
// When hasValue is true, valueArg is parsed as JSON if it starts with a
// JSON-shape leading byte; otherwise it's treated as a string literal.
// When jsonFile is non-empty, its contents (or stdin for "-") are
// json.Unmarshal'd directly. The parsed value is sent to the daemon,
// which performs the dotted-path descent + reserved-key validation.
func parseSetValue(valueArg string, hasValue bool, jsonFile string) (any, error) {
	if jsonFile != "" {
		var data []byte
		if jsonFile == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("read --json-value from stdin: %w", err)
			}
			data = b
		} else {
			b, err := os.ReadFile(jsonFile)
			if err != nil {
				return nil, fmt.Errorf("read --json-value %s: %w", jsonFile, err)
			}
			data = b
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("parse --json-value: %w", err)
		}
		return v, nil
	}
	if !hasValue {
		return nil, errors.New("--value or --json-value is required")
	}
	trimmed := strings.TrimSpace(valueArg)
	if trimmed != "" {
		c := trimmed[0]
		switch {
		case c == '[' || c == '{' || c == '"' || c == '-' || c == 't' || c == 'f' || c == 'n':
			var v any
			if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
				return v, nil
			}
		case c >= '0' && c <= '9':
			var v any
			if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
				return v, nil
			}
		}
	}
	return valueArg, nil
}

// printMetadataJSON renders the metadata blob as stable pretty-printed
// JSON. nil / empty maps render as `{}` so the output is always a valid
// JSON object.
func printMetadataJSON(w io.Writer, m map[string]any) error {
	if m == nil {
		m = map[string]any{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// printVisitsPretty renders metadata.step_visits with step names + caps
// resolved against the task's workflow (wf, possibly nil for orphaned
// counters). Falls back to bare step ids when a step id is unknown.
func printVisitsPretty(w io.Writer, wf *rpcclient.Workflow, md map[string]any) error {
	sv := meta.GetStepVisits(md)
	if len(sv) == 0 && len(md) == 0 {
		fmt.Fprintln(w, "(no metadata)")
		return nil
	}
	byID := map[string]rpcclient.WorkflowStep{}
	if wf != nil {
		for _, s := range wf.Steps {
			byID[s.ID] = s
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(sv) > 0 {
		fmt.Fprintln(tw, "step_visits:")
		ids := make([]string, 0, len(sv))
		for k := range sv {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		for _, id := range ids {
			st, found := byID[id]
			label := "<unknown:" + id + ">"
			if found {
				label = st.Name
			}
			line := fmt.Sprintf("  %s\t%d", label, sv[id])
			if found && st.MaxVisits > 0 {
				line += fmt.Sprintf(" / %d", st.MaxVisits)
				if sv[id] >= st.MaxVisits {
					line += "  *"
				}
			}
			fmt.Fprintln(tw, line)
		}
	}
	other := make(map[string]any, len(md))
	for k, v := range md {
		if k == meta.StepVisitsKey {
			continue
		}
		other[k] = v
	}
	if len(other) > 0 {
		fmt.Fprintln(tw, "other keys:")
		keys := make([]string, 0, len(other))
		for k := range other {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, _ := json.Marshal(other[k])
			fmt.Fprintf(tw, "  %s\t= %s\n", k, string(b))
		}
	}
	return tw.Flush()
}
