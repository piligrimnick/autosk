// Package render produces human-readable and JSON output for tasks.
//
// JSON shapes are stable and golden-tested. Renaming or removing a field is a
// breaking change. See docs/plans/20260513-Init-Plan.md §10.2.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"autosk/internal/store"
)

// TaskJSON is the wire shape for a single task. The derived `blocked` flag
// and edge arrays are present here; renderers fill them from caller-supplied
// data so storage doesn't need to know about wire format.
type TaskJSON struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Priority    int       `json:"priority"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Derived fields. Populated by the CLI layer; zero-valued is acceptable
	// during P2/P3 (P4 wires the real values).
	Blocked   bool     `json:"blocked"`
	BlockedBy []string `json:"blocked_by"`
	Blocks    []string `json:"blocks"`
}

// ToWire converts a store.Task into the wire shape. blocked / arrays
// default to false / nil — callers populate them when available.
func ToWire(t store.Task) TaskJSON {
	return TaskJSON{
		ID:          t.ID,
		Title:       t.Title,
		Description: t.Description,
		Status:      string(t.Status),
		Priority:    t.Priority,
		CreatedAt:   t.CreatedAt.UTC(),
		UpdatedAt:   t.UpdatedAt.UTC(),
		BlockedBy:   []string{}, // emit `[]` not `null` for stability
		Blocks:      []string{},
	}
}

// TaskJSONTo writes a single task as JSON (one line, no trailing newline
// is added by Marshal; we add one to be POSIX-friendly).
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
	fmt.Fprintf(w, "id:          %s\n", wire.ID)
	fmt.Fprintf(w, "title:       %s\n", wire.Title)
	fmt.Fprintf(w, "status:      %s\n", wire.Status)
	fmt.Fprintf(w, "priority:    %d\n", wire.Priority)
	if wire.Blocked {
		fmt.Fprintf(w, "blocked:     yes\n")
	} else {
		fmt.Fprintf(w, "blocked:     no\n")
	}
	if len(wire.BlockedBy) > 0 {
		fmt.Fprintf(w, "blocked_by:  %s\n", strings.Join(wire.BlockedBy, ", "))
	}
	if len(wire.Blocks) > 0 {
		fmt.Fprintf(w, "blocks:      %s\n", strings.Join(wire.Blocks, ", "))
	}
	fmt.Fprintf(w, "created_at:  %s\n", wire.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "updated_at:  %s\n", wire.UpdatedAt.Format(time.RFC3339))
	if wire.Description != "" {
		fmt.Fprintf(w, "description:\n%s\n", indent(wire.Description, "  "))
	}
	return nil
}

// Tasks writes a compact table for a slice of tasks.
func Tasks(w io.Writer, ts []store.Task, deco Decorator) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tP\tSTATUS\tTITLE")
	for _, t := range ts {
		wt := ToWire(t)
		if deco != nil {
			deco(&wt)
		}
		statusStr := wt.Status
		if wt.Blocked {
			statusStr = wt.Status + "*" // asterisk = blocked
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", wt.ID, wt.Priority, statusStr, truncate(wt.Title, 60))
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
