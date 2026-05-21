// Package render produces human-readable and JSON output for tasks.
//
// JSON shapes are stable and golden-tested. Renaming or removing a field is a
// breaking change. See docs/plans/20260513-Init-Plan.md §10.2.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"autosk/internal/store"
	"autosk/internal/timeformat"
)

// TaskJSON is the wire shape for a single task. The derived `blocked`
// flag and edge arrays are present here; renderers fill them from
// caller-supplied data so storage doesn't need to know about wire format.
type TaskJSON struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Priority     int       `json:"priority"`
	AuthorID     string    `json:"author_id,omitempty"`
	WorkflowID   string    `json:"workflow_id,omitempty"`
	CurrentStep  string    `json:"current_step,omitempty"`  // step name, not id
	CurrentAgent string    `json:"current_agent,omitempty"` // derived: steps[current_step_id].agent.name
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	Blocked   bool     `json:"blocked"`
	BlockedBy []string `json:"blocked_by"`
	Blocks    []string `json:"blocks"`

	// Metadata mirrors the task's free-form tasks.metadata JSON blob.
	// Always emitted as an object (empty `{}` when the column was NULL
	// or empty) so callers can rely on its presence in --json output.
	Metadata map[string]any `json:"metadata"`

	// Worktree, when non-nil, is the worktree-isolation block surfaced
	// by `autosk show` for tasks whose workflow has isolation=worktree.
	// Omitted entirely for tasks that don't qualify (no workflow / non-
	// isolated workflow) so existing JSON consumers don't see a new
	// always-present key.
	Worktree *WorktreeJSON `json:"worktree,omitempty"`

	// Display-only joins. Not emitted in JSON (those carry the raw id);
	// only used by the human renderer to build the `[id]: name` form.
	authorName     string
	workflowName   string
	currentAgentID string
	visitsSummary  string
}

// WorktreeJSON is the per-task worktree-isolation block surfaced by
// `autosk show --json` (and the matching text-mode lines). Path /
// Branch are derived deterministically from (canonicalProjectRoot,
// task.id); Exists is computed at render time via stat.
type WorktreeJSON struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Exists bool   `json:"exists"`
}

// ToWire converts a store.Task into the wire shape. blocked / arrays /
// current_step / current_agent default to zero values — callers populate
// them via Options or a Decorator when relevant. Metadata defaults to
// an empty object (NULL on disk is indistinguishable from `{}`).
func ToWire(t store.Task) TaskJSON {
	md := t.Metadata
	if md == nil {
		md = map[string]any{}
	}
	return TaskJSON{
		ID:          t.ID,
		Title:       t.Title,
		Description: t.Description,
		Status:      string(t.Status),
		Priority:    t.Priority,
		AuthorID:    t.AuthorID,
		WorkflowID:  t.WorkflowID,
		CreatedAt:   t.CreatedAt.UTC(),
		UpdatedAt:   t.UpdatedAt.UTC(),
		BlockedBy:   []string{},
		Blocks:      []string{},
		Metadata:    md,
	}
}

// WithStep sets the human-friendly step name and the derived
// current_agent. agentID is optional (only used by the human renderer to
// format `[ag-XXXX]: name`); passing "" omits the id prefix there.
func WithStep(stepName, agentName, agentID string) Option {
	return func(t *TaskJSON) {
		t.CurrentStep = stepName
		t.CurrentAgent = agentName
		t.currentAgentID = agentID
	}
}

// WithAuthor sets the human-friendly author name (with id) for the
// task's author. Used by the human renderer only; the JSON wire shape
// keeps the raw author_id.
func WithAuthor(name string) Option {
	return func(t *TaskJSON) { t.authorName = name }
}

// WithWorkflow sets the human-friendly workflow name for the task's
// workflow_id. Used by the human renderer only.
func WithWorkflow(name string) Option {
	return func(t *TaskJSON) { t.workflowName = name }
}

// WithMetadata supplies the task's metadata blob and an optional one-
// line summary of step_visits for the human renderer. Passing nil for
// md is fine — the renderer treats it as an empty object. The summary
// is human-only; the JSON wire shape always carries the raw map.
func WithMetadata(md map[string]any, visitsSummary string) Option {
	return func(t *TaskJSON) {
		if md != nil {
			t.Metadata = md
		} else if t.Metadata == nil {
			t.Metadata = map[string]any{}
		}
		t.visitsSummary = visitsSummary
	}
}

// WithWorktree adds the worktree-isolation block to the rendered
// output (both text and JSON forms). Pass nil to omit the block;
// callers that detect an isolated workflow construct WorktreeJSON
// from internal/worktree's deterministic helpers and pass it here.
func WithWorktree(wt *WorktreeJSON) Option {
	return func(t *TaskJSON) { t.Worktree = wt }
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
//
// Every field appears on a stable line, even when the underlying value
// is null/empty (rendered as `-`). This gives a uniform, scannable
// shape regardless of whether the task is currently in a workflow.
// The header uses the `[id]: title` form, and references to other
// entities (author, workflow, current_agent) use the same convention
// when the caller supplied their names via WithAuthor/WithWorkflow/
// WithStep. Lists (blocked_by/blocks) print `-` when empty.
//
// JSON output keeps `omitempty` semantics — this `-`-everywhere is
// human-only.
func Task(w io.Writer, t store.Task, opts ...Option) error {
	wire := applyOptions(ToWire(t), opts...)
	fmt.Fprintf(w, "[%s]: %s\n", wire.ID, wire.Title)
	fmt.Fprintf(w, "status:        %s\n", wire.Status)
	fmt.Fprintf(w, "priority:      %d\n", wire.Priority)
	fmt.Fprintf(w, "workflow:      %s\n", bracketedRefOrDash(wire.WorkflowID, wire.workflowName))
	fmt.Fprintf(w, "current_step:  %s\n", dashIfEmpty(wire.CurrentStep))
	fmt.Fprintf(w, "current_agent: %s\n", bracketedRefOrDash(wire.currentAgentID, wire.CurrentAgent))
	fmt.Fprintf(w, "author:        %s\n", bracketedRefOrDash(wire.AuthorID, wire.authorName))
	if wire.Blocked {
		fmt.Fprintf(w, "blocked:       yes\n")
	} else {
		fmt.Fprintf(w, "blocked:       no\n")
	}
	fmt.Fprintf(w, "blocked_by:    %s\n", joinOrDash(wire.BlockedBy))
	fmt.Fprintf(w, "blocks:        %s\n", joinOrDash(wire.Blocks))
	// Human text output: local TZ + 'YYYY-MM-DD HH:MM:SS'. The JSON
	// wire shape (TaskJSON above) keeps RFC3339 UTC.
	fmt.Fprintf(w, "created_at:    %s\n", timeformat.FormatDateTime(wire.CreatedAt))
	fmt.Fprintf(w, "updated_at:    %s\n", timeformat.FormatDateTime(wire.UpdatedAt))
	if wire.visitsSummary != "" {
		fmt.Fprintf(w, "visits:        %s\n", wire.visitsSummary)
	}
	if wire.Worktree != nil {
		presence := "missing"
		if wire.Worktree.Exists {
			presence = "exists"
		}
		// shortenHome matches the `worktree list` convention so the
		// path reads `~/.autosk/worktrees/...` for any worktree
		// rooted under the user's home. JSON output keeps the
		// absolute form (Worktree.Path), since that's the field
		// machine consumers want.
		fmt.Fprintf(w, "worktree:      %s (%s)\n", shortenHome(wire.Worktree.Path), presence)
		fmt.Fprintf(w, "branch:        %s\n", wire.Worktree.Branch)
	}
	if wire.Description != "" {
		fmt.Fprintf(w, "description:\n%s\n", indent(wire.Description, "  "))
	} else {
		fmt.Fprintf(w, "description:   -\n")
	}
	return nil
}

// dashIfEmpty returns "-" for an empty string, otherwise the string.
// Used by the human task renderer to keep field-line shape uniform.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// bracketedRefOrDash is bracketedRef with the convention that an empty
// id (no reference at all) prints `-` rather than an empty bracket.
func bracketedRefOrDash(id, name string) string {
	if id == "" {
		return "-"
	}
	return bracketedRef(id, name)
}

// joinOrDash joins ss with ", " or returns "-" when ss is empty.
func joinOrDash(ss []string) string {
	if len(ss) == 0 {
		return "-"
	}
	return strings.Join(ss, ", ")
}

// bracketedRef formats a reference to another entity as `[id]: name`
// when both are known, or `[id]` when the name lookup wasn't supplied.
// Empty id is a programming error; we still return something useful.
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

// shortenHome replaces a leading $HOME with `~` for readable output.
// Used by the human-mode worktree line so it matches the convention
// in cmd/autosk/worktree.go. JSON output keeps the absolute form.
func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
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
