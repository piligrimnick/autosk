package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/meta"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// newMetadataCmd is the parent for the per-task metadata CLI.
//
// `autosk metadata` lets humans inspect and edit the free-form JSON
// blob attached to each task (the tasks.metadata column). Today the
// only engine-reserved sub-object is `step_visits`, which the visit-
// limit feature uses to enforce per-step max_visits caps.
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
			s, closeFn, err := openStore(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			ag := agent.New(dl.DB())
			wfs := workflow.New(dl.DB(), ag)
			tk, err := getTaskOrErr(cmd.Context(), s, args[0])
			if err != nil {
				return err
			}
			if flagQuiet {
				return nil
			}
			if visitsPretty {
				return printVisitsPretty(cmd.Context(), os.Stdout, wfs, tk)
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

			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			pathParts, err := splitMetadataKey(key)
			if err != nil {
				return err
			}
			// Validate engine-reserved shapes BEFORE mutating.
			if pathParts[0] == meta.StepVisitsKey {
				if err := validateReservedWrite(pathParts, parsed); err != nil {
					return err
				}
			}
			updated, changed, err := s.UpdateMetadata(cmd.Context(), args[0], func(m map[string]any) error {
				return setMetadataPath(m, pathParts, parsed)
			})
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if changed {
				commitWrite(cmd.Context(), s, "metadata set "+args[0]+" --key "+key)
			}
			if flagQuiet {
				return nil
			}
			if !changed {
				fmt.Fprintln(os.Stderr, "(no change)")
			}
			return printMetadataJSON(os.Stdout, updated)
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
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			pathParts, err := splitMetadataKey(key)
			if err != nil {
				return err
			}
			updated, changed, err := s.UpdateMetadata(cmd.Context(), args[0], func(m map[string]any) error {
				unsetMetadataPath(m, pathParts)
				return nil
			})
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			if changed {
				commitWrite(cmd.Context(), s, "metadata unset "+args[0]+" --key "+key)
			}
			if flagQuiet {
				return nil
			}
			if !changed {
				fmt.Fprintln(os.Stderr, "(no change)")
			}
			return printMetadataJSON(os.Stdout, updated)
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
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			dl := s.(*doltlite.Store)
			ag := agent.New(dl.DB())
			wfs := workflow.New(dl.DB(), ag)

			resolvedID := stepID
			if stepName != "" {
				tk, err := getTaskOrErr(cmd.Context(), s, args[0])
				if err != nil {
					return err
				}
				if tk.WorkflowID == "" {
					return fmt.Errorf("--step requires the task to have a workflow_id; use --step-id ID for orphaned counters")
				}
				st, err := wfs.FindStepByName(cmd.Context(), tk.WorkflowID, stepName)
				if err != nil {
					if errors.Is(err, workflow.ErrNotFound) {
						return fmt.Errorf("step %q not found in this task's workflow", stepName)
					}
					return err
				}
				resolvedID = st.ID
			}

			updated, changed, err := s.UpdateMetadata(cmd.Context(), args[0], func(m map[string]any) error {
				meta.MutateStepVisits(m, func(sv meta.StepVisits) {
					if resolvedID == "" {
						for k := range sv {
							delete(sv, k)
						}
						return
					}
					delete(sv, resolvedID)
				})
				return nil
			})
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("task not found: %s", args[0])
				}
				return err
			}
			var hint string
			if resolvedID != "" {
				hint = " --step-id " + resolvedID
			}
			if changed {
				commitWrite(cmd.Context(), s, "metadata reset-visits "+args[0]+hint)
			}
			if flagQuiet {
				return nil
			}
			if !changed {
				fmt.Fprintln(os.Stderr, "(no change)")
			}
			return printMetadataJSON(os.Stdout, updated)
		},
	}
	cmd.Flags().StringVar(&stepName, "step", "", "reset only this step's counter (requires task.workflow_id)")
	cmd.Flags().StringVar(&stepID, "step-id", "", "reset only this step_id's counter (useful for orphaned counters)")
	return cmd
}

// ---- internals -----------------------------------------------------------

func getTaskOrErr(ctx context.Context, s store.Store, id string) (store.Task, error) {
	tk, err := s.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, fmt.Errorf("task not found: %s", id)
		}
		return store.Task{}, err
	}
	return tk, nil
}

// splitMetadataKey splits a dotted path into its component parts.
// Empty segments (from leading, trailing, or doubled dots) are an
// error rather than silently dropped — the engine validates writes
// under `step_visits` against pathParts[0], so accidental input like
// `.step_visits.st-x` must not be reinterpreted as `step_visits.st-x`
// and squeak past validation by accident. Returns the trimmed key
// components or an error explaining the malformed input.
func splitMetadataKey(k string) ([]string, error) {
	trimmed := strings.TrimSpace(k)
	if trimmed == "" {
		return nil, errors.New("--key cannot be empty")
	}
	raw := strings.Split(trimmed, ".")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("--key %q has an empty dotted segment; leading/trailing/doubled dots are not allowed", k)
		}
		out = append(out, p)
	}
	return out, nil
}

// parseSetValue resolves --value or --json-value into a parsed Go value.
// When hasValue is true, valueArg is parsed as JSON if it starts with a
// JSON-shape leading byte; otherwise it's treated as a string literal.
// When jsonFile is non-empty, its contents (or stdin for "-") are
// json.Unmarshal'd directly.
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

// setMetadataPath stores `value` at the dotted `path` inside m,
// creating intermediate JSON objects as needed. Errors when an
// intermediate path element points at a non-object.
func setMetadataPath(m map[string]any, path []string, value any) error {
	cur := m
	for i, key := range path[:len(path)-1] {
		existing, ok := cur[key]
		if !ok || existing == nil {
			next := make(map[string]any)
			cur[key] = next
			cur = next
			continue
		}
		nextMap, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot descend into %q at path %s: not an object",
				key, strings.Join(path[:i+1], "."))
		}
		cur = nextMap
	}
	cur[path[len(path)-1]] = value
	return nil
}

// unsetMetadataPath removes the value at `path`. Returns true when a
// removal actually happened. Empty parent objects along the path are
// pruned bottom-up so the metadata column round-trips back to NULL
// once the last key is gone.
func unsetMetadataPath(m map[string]any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	// Walk to the parent of the leaf, collecting the chain so we can
	// prune empty containers afterwards.
	chain := []map[string]any{m}
	for _, key := range path[:len(path)-1] {
		next, ok := chain[len(chain)-1][key].(map[string]any)
		if !ok {
			return false
		}
		chain = append(chain, next)
	}
	leafKey := path[len(path)-1]
	parent := chain[len(chain)-1]
	if _, ok := parent[leafKey]; !ok {
		return false
	}
	delete(parent, leafKey)
	// Prune empty containers, but never delete the root itself.
	for i := len(chain) - 1; i > 0; i-- {
		if len(chain[i]) > 0 {
			break
		}
		parent := chain[i-1]
		key := path[i-1]
		delete(parent, key)
	}
	return true
}

// validateReservedWrite enforces the typed shape of engine-managed
// keys. Today only `step_visits` is reserved; the engine refuses any
// write that would put a non-integer leaf in there.
func validateReservedWrite(path []string, value any) error {
	if len(path) == 0 || path[0] != meta.StepVisitsKey {
		return nil
	}
	switch len(path) {
	case 1:
		// Writing the entire step_visits object.
		return meta.ValidateStepVisitsObject(value)
	case 2:
		// Writing one leaf inside step_visits.
		return meta.ValidateStepVisitsLeaf(value)
	default:
		return fmt.Errorf("step_visits has a flat shape (step_id → int); cannot nest under %s",
			strings.Join(path, "."))
	}
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
// resolved against the task's workflow. Falls back to bare step ids
// when the lookup fails (e.g. orphaned counters from a dropped wf).
func printVisitsPretty(ctx context.Context, w io.Writer, wfs *workflow.Store, tk store.Task) error {
	sv := meta.GetStepVisits(tk.Metadata)
	if len(sv) == 0 && len(tk.Metadata) == 0 {
		fmt.Fprintln(w, "(no metadata)")
		return nil
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
			name, maxV, found := lookupStepNameAndCap(ctx, wfs, id)
			label := id
			if found {
				label = name
			} else {
				label = "<unknown:" + id + ">"
			}
			line := fmt.Sprintf("  %s\t%d", label, sv[id])
			if maxV > 0 {
				line += fmt.Sprintf(" / %d", maxV)
				if sv[id] >= maxV {
					line += "  *"
				}
			}
			fmt.Fprintln(tw, line)
		}
	}
	other := make(map[string]any, len(tk.Metadata))
	for k, v := range tk.Metadata {
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

// lookupStepNameAndCap resolves a step id to (name, max_visits, found)
// via the workflow store. Used by the pretty renderer; safe to call
// on orphan ids (returns found=false).
func lookupStepNameAndCap(ctx context.Context, wfs *workflow.Store, stepID string) (string, int, bool) {
	if wfs == nil || stepID == "" {
		return "", 0, false
	}
	st, err := wfs.FindStepByID(ctx, stepID)
	if err != nil {
		return "", 0, false
	}
	return st.Name, st.MaxVisits, true
}

// renderVisitsSummary builds a single-line summary of step_visits for
// the human `autosk show` renderer. Returns "" when no counters are
// present. Format: `dev 3/5, review 5/5*, validator 1`.
func renderVisitsSummary(ctx context.Context, wfs *workflow.Store, tk store.Task) string {
	sv := meta.GetStepVisits(tk.Metadata)
	if len(sv) == 0 {
		return ""
	}
	ids := make([]string, 0, len(sv))
	for k := range sv {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		name, maxV, found := lookupStepNameAndCap(ctx, wfs, id)
		label := name
		if !found {
			label = "<unknown:" + id + ">"
		}
		var seg string
		if maxV > 0 {
			marker := ""
			if sv[id] >= maxV {
				marker = "*"
			}
			seg = fmt.Sprintf("%s %d/%d%s", label, sv[id], maxV, marker)
		} else {
			seg = fmt.Sprintf("%s %d", label, sv[id])
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ", ")
}
