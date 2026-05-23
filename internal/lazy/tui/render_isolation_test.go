package tui

import (
	"strings"
	"testing"

	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/datasource"
)

// TestRenderWorkflowsPanel_WTMarker pins that the `[wt]` marker is
// emitted exactly for non-synthetic workflows whose Isolation =
// "worktree". Synthetic rows never carry the marker; rows with
// Isolation == "" or "none" likewise don't.
func TestRenderWorkflowsPanel_WTMarker(t *testing.T) {
	wfs := []datasource.Workflow{
		{Name: "iso-wf", Isolation: "worktree", FirstStep: "dev"},
		{Name: "plain-wf", Isolation: "none", FirstStep: "dev"},
		{Name: "empty-wf" /* Isolation: "" */, FirstStep: "dev"},
		{Name: "single:@autosk/dev", Isolation: "none", IsSynthetic: true, FirstStep: "do"},
	}
	out, _ := renderWorkflowsPanel(wfs, 0, "")
	visible := ansiutil.Strip(out)
	lines := strings.Split(strings.TrimRight(visible, "\n"), "\n")
	if len(lines) != len(wfs) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(wfs), len(lines), visible)
	}
	// First row: iso-wf → contains [wt].
	if !strings.Contains(lines[0], "[wt]") {
		t.Errorf("iso-wf row missing [wt]:\n%q", lines[0])
	}
	// plain-wf and empty-wf and synthetic must NOT carry [wt].
	for i, name := range []string{"plain-wf", "empty-wf", "single:@autosk/dev"} {
		if strings.Contains(lines[i+1], "[wt]") {
			t.Errorf("%s row should NOT carry [wt]:\n%q", name, lines[i+1])
		}
	}
}

// TestRenderWorkflowsPanel_NonIsolatedUnchanged pins the existing
// behaviour: a workflow without isolation produces a row with no
// new visual surprises (no spurious marker tokens).
func TestRenderWorkflowsPanel_NonIsolatedUnchanged(t *testing.T) {
	wfs := []datasource.Workflow{
		{Name: "plain", Isolation: "none", FirstStep: "dev"},
	}
	out, _ := renderWorkflowsPanel(wfs, 0, "")
	visible := ansiutil.Strip(out)
	if strings.Contains(visible, "[wt]") {
		t.Errorf("non-isolated row should not carry [wt]:\n%q", visible)
	}
	// Plain row should still contain the workflow name + the muted
	// 'first=' chunk verbatim — pin the column anchors that existed
	// before the marker column was inserted.
	for _, want := range []string{"plain", "first="} {
		if !strings.Contains(visible, want) {
			t.Errorf("plain row missing %q: %q", want, visible)
		}
	}
}

// TestRenderWorkflowDetail_IsolationLine pins the workflow-inspector
// isolation line: always present, includes the non-terminal-task
// count when applicable.
func TestRenderWorkflowDetail_IsolationLine(t *testing.T) {
	cases := []struct {
		name string
		w    datasource.Workflow
		want []string
	}{
		{
			name: "isolated_with_tasks",
			w: datasource.Workflow{
				Name: "iso", Isolation: "worktree", FirstStep: "dev",
				NonTerminalTaskCount: 3,
			},
			want: []string{"isolation: worktree", "3 non-terminal task(s)"},
		},
		{
			name: "isolated_no_tasks",
			w: datasource.Workflow{
				Name: "iso", Isolation: "worktree", FirstStep: "dev",
			},
			want: []string{"isolation: worktree"},
		},
		{
			name: "plain_empty_string",
			w: datasource.Workflow{
				Name: "plain", Isolation: "", FirstStep: "dev",
			},
			want: []string{"isolation: none"},
		},
		{
			name: "synthetic_pinned_note",
			w: datasource.Workflow{
				Name: "single:@autosk/dev", Isolation: "none", IsSynthetic: true, FirstStep: "do",
			},
			want: []string{"isolation: none", "pinned: synthetic workflows always 'none'"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			visible := ansiutil.Strip(renderWorkflowDetail(c.w, 80))
			for _, want := range c.want {
				if !strings.Contains(visible, want) {
					t.Errorf("missing %q in:\n%s", want, visible)
				}
			}
		})
	}
}
