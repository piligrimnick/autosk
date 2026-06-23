// Package render produces human-readable and JSON output for tasks (v2 shape).
//
// JSON shapes are stable and golden-tested. Renaming or removing a field is a
// breaking change. v2 dropped priority/author/metadata/worktree from the task
// and renders workflow/step as names.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"autosk/internal/store"
	"autosk/internal/timeformat"
)

// TaskJSON is the wire shape for a single task (v2). The derived `blocked` flag,
// edge arrays, and comment_count are filled by callers via Options so the store
// shape stays minimal.
type TaskJSON struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Workflow    string    `json:"workflow,omitempty"`
	Step        string    `json:"step,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	Blocked      bool     `json:"blocked"`
	BlockedBy    []string `json:"blocked_by"`
	Blocks       []string `json:"blocks"`
	CommentCount int      `json:"comment_count"`

	// Metadata is the free-form, human-editable bag (always emitted; {} when
	// none). The engine reserves the `step_visits` sub-object inside it.
	Metadata map[string]any `json:"metadata"`
}

// ToWire converts a store.Task into the wire shape. Derived fields default to
// zero values — callers populate them via Options or a Decorator.
func ToWire(t store.Task) TaskJSON {
	// Metadata is always present on the wire ({} when none), mirroring the SDK
	// TaskView contract, so `show --json` carries a stable `metadata` object.
	meta := t.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return TaskJSON{
		ID:          t.ID,
		Title:       t.Title,
		Description: t.Description,
		Status:      string(t.Status),
		Workflow:    t.Workflow,
		Step:        t.Step,
		CreatedAt:   t.CreatedAt.UTC(),
		UpdatedAt:   t.UpdatedAt.UTC(),
		BlockedBy:   []string{},
		Blocks:      []string{},
		Metadata:    meta,
	}
}

// TaskJSONTo writes a single task as JSON.
func TaskJSONTo(w io.Writer, t store.Task, opts ...Option) error {
	wire := applyOptions(ToWire(t), opts...)
	b, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// TasksJSONTo writes a slice of tasks as a JSON array.
func TasksJSONTo(w io.Writer, ts []store.Task, deco Decorator) error {
	out := make([]TaskJSON, 0, len(ts))
	for _, t := range ts {
		wt := ToWire(t)
		if deco != nil {
			deco(&wt)
		}
		out = append(out, wt)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Task writes a key/value block for a single task (human format).
func Task(w io.Writer, t store.Task, opts ...Option) error {
	wire := applyOptions(ToWire(t), opts...)
	fmt.Fprintf(w, "[%s]: %s\n", wire.ID, wire.Title)
	fmt.Fprintf(w, "status:        %s\n", wire.Status)
	fmt.Fprintf(w, "workflow:      %s\n", dashIfEmpty(wire.Workflow))
	fmt.Fprintf(w, "step:          %s\n", dashIfEmpty(wire.Step))
	if wire.Blocked {
		fmt.Fprintf(w, "blocked:       yes\n")
	} else {
		fmt.Fprintf(w, "blocked:       no\n")
	}
	fmt.Fprintf(w, "blocked_by:    %s\n", joinOrDash(wire.BlockedBy))
	fmt.Fprintf(w, "blocks:        %s\n", joinOrDash(wire.Blocks))
	fmt.Fprintf(w, "comments:      %d\n", wire.CommentCount)
	// Human text output: local TZ + 'YYYY-MM-DD HH:MM:SS'. The JSON wire shape
	// keeps RFC3339 UTC.
	fmt.Fprintf(w, "created_at:    %s\n", timeformat.FormatDateTime(wire.CreatedAt))
	fmt.Fprintf(w, "updated_at:    %s\n", timeformat.FormatDateTime(wire.UpdatedAt))
	if wire.Description != "" {
		fmt.Fprintf(w, "description:\n%s\n", indent(wire.Description, "  "))
	} else {
		fmt.Fprintf(w, "description:   -\n")
	}
	return nil
}

// dashIfEmpty returns "-" for an empty string, otherwise the string.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// joinOrDash joins ss with ", " or returns "-" when ss is empty.
func joinOrDash(ss []string) string {
	if len(ss) == 0 {
		return "-"
	}
	return strings.Join(ss, ", ")
}

// bracketedRef formats a reference to another entity as `[id]: name` when both
// are known, or `[id]` when the name lookup wasn't supplied.
func bracketedRef(id, name string) string {
	if id == "" {
		return name
	}
	if name == "" {
		return "[" + id + "]"
	}
	return "[" + id + "]: " + name
}

// BracketedRef is the exported form used by other render callers (e.g.
// cmd/autosk/agent.go) so the format stays in one place.
func BracketedRef(id, name string) string { return bracketedRef(id, name) }

// Tasks writes a compact table for a slice of tasks.
func Tasks(w io.Writer, ts []store.Task, deco Decorator) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTITLE")
	for _, t := range ts {
		wt := ToWire(t)
		if deco != nil {
			deco(&wt)
		}
		statusStr := wt.Status
		if wt.Blocked {
			statusStr = wt.Status + "*" // asterisk = blocked
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", wt.ID, statusStr, truncate(wt.Title, 60))
	}
	return tw.Flush()
}

// ---- helpers --------------------------------------------------------------

// Decorator mutates a TaskJSON in place, typically to add derived fields.
type Decorator func(*TaskJSON)

// Option configures a single-task render.
type Option func(*TaskJSON)

// WithBlocked sets the derived blocked flag and edge arrays.
func WithBlocked(blocked bool, blockedBy, blocks []string) Option {
	return func(t *TaskJSON) {
		t.Blocked = blocked
		if blockedBy == nil {
			blockedBy = []string{}
		}
		if blocks == nil {
			blocks = []string{}
		}
		t.BlockedBy = blockedBy
		t.Blocks = blocks
	}
}

// WithCommentCount sets the derived comment_count.
func WithCommentCount(n int) Option {
	return func(t *TaskJSON) { t.CommentCount = n }
}

func applyOptions(t TaskJSON, opts ...Option) TaskJSON {
	for _, o := range opts {
		o(&t)
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}
