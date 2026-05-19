package tui

import (
	"strings"
	"testing"

	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// TestApplyFacetFilter pins the parser behaviour the `/` panel uses
// to translate "p:1 status:done auth" into a TaskFilter plus free
// text. Also checks that wf:<name> resolves to a wf-id via the
// caller-provided map (and uses a sentinel for unknown names).
func TestApplyFacetFilter(t *testing.T) {
	wfByName := map[string]string{
		"feature-dev": "wf-feat",
		"ops":         "wf-ops",
	}
	tests := []struct {
		in        string
		wantP     *int
		wantStat  []store.Status
		wantAgent string
		wantWfID  string
		wantFree  string
	}{
		{"", nil, nil, "", "", ""},
		{"p:1", intPtr(1), nil, "", "", ""},
		{"status:done", nil, []store.Status{store.StatusDone}, "", "", ""},
		{"agent:human", nil, nil, "human", "", ""},
		{"p:0 agent:foo refactor", intPtr(0), nil, "foo", "", "refactor"},
		{"auth", nil, nil, "", "", "auth"},
		{"unknown:val", nil, nil, "", "", "unknown:val"},
		{"wf:feature-dev", nil, nil, "", "wf-feat", ""},
		{"wf:FEATURE-DEV", nil, nil, "", "wf-feat", ""},
		{"wf:missing", nil, nil, "", "\x00missing", ""},
		{"wf:feature-dev foo", nil, nil, "", "wf-feat", "foo"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			f := datasource.TaskFilter{}
			applyFacetFilter(&f, tc.in, wfByName)
			if !samePtr(f.Priority, tc.wantP) {
				t.Errorf("p: got %v want %v", deref(f.Priority), deref(tc.wantP))
			}
			if !sameStatuses(f.Statuses, tc.wantStat) {
				t.Errorf("statuses: got %v want %v", f.Statuses, tc.wantStat)
			}
			if f.AgentName != tc.wantAgent {
				t.Errorf("agent: got %q want %q", f.AgentName, tc.wantAgent)
			}
			if f.WorkflowID != tc.wantWfID {
				t.Errorf("wf id: got %q want %q", f.WorkflowID, tc.wantWfID)
			}
			if strings.TrimSpace(f.Search) != tc.wantFree {
				t.Errorf("free: got %q want %q", f.Search, tc.wantFree)
			}
		})
	}
}

// TestApplyFacetFilter_NoDoubleApply pins the BLOCKER 2 regression:
// after applyFacetFilter has eaten the structured tokens, only the
// remaining free text should drive applyTaskSearch — NOT the original
// expression. The previous bug fed 'p:1' to applyTaskSearch, which
// then filtered every task whose id+title didn't contain the literal
// substring 'p:1' (i.e. all of them).
func TestApplyFacetFilter_NoDoubleApply(t *testing.T) {
	f := datasource.TaskFilter{}
	applyFacetFilter(&f, "p:1 refactor", nil)
	if strings.TrimSpace(f.Search) != "refactor" {
		t.Fatalf("expected post-facet free text to be %q, got %q", "refactor", f.Search)
	}
	// Now apply substring search against a representative set; only
	// the title-matching row should pass.
	in := []datasource.Task{
		{ID: "as-1", Title: "refactor auth", Priority: 1},
		{ID: "as-2", Title: "unrelated", Priority: 1},
		// p:1 must not appear as substring in either id or title; if
		// it does, the test stays correct because applyTaskSearch is
		// fed the post-facet remainder, not the original expression.
	}
	got := applyTaskSearch(in, f.Search)
	if len(got) != 1 || got[0].ID != "as-1" {
		t.Fatalf("post-facet search: got %+v", got)
	}
}

// TestApplyTaskSearch ensures the in-memory free-text filter is
// case-insensitive across id and title.
func TestApplyTaskSearch(t *testing.T) {
	in := []datasource.Task{
		{ID: "as-aaaa", Title: "Refactor token validation"},
		{ID: "as-bbbb", Title: "Logging cleanup"},
		{ID: "as-cccc", Title: "Auth refactor"},
	}
	got := applyTaskSearch(in, "REFACTOR")
	if len(got) != 2 {
		t.Fatalf("expected 2 hits for REFACTOR, got %d", len(got))
	}
}

// TestClampCursor pins the cursor-stability behaviour used after
// refresh swaps a panel's slice underneath the cursor.
func TestClampCursor(t *testing.T) {
	tests := []struct {
		cur, n, want int
	}{
		{0, 0, 0},
		{5, 0, 0},
		{-3, 5, 0},
		{2, 5, 2},
		{99, 5, 4},
	}
	for _, tc := range tests {
		if got := clampCursor(tc.cur, tc.n); got != tc.want {
			t.Errorf("clamp(%d,%d)=%d want %d", tc.cur, tc.n, got, tc.want)
		}
	}
}

func intPtr(i int) *int { return &i }
func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
func samePtr(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
func sameStatuses(a, b []store.Status) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
