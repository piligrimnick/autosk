package tui

import (
	"testing"

	"autosk/internal/lazy/datasource"
)

// TestFilterSessionsByWorkflow locks in the review regression (#955): under a
// workflow-only scope the Sessions panel must be narrowed to the active
// workflow client-side, because session.list carries no server-side workflow
// filter. An empty workflow is a pass-through.
func TestFilterSessionsByWorkflow(t *testing.T) {
	in := []datasource.Session{
		{ID: "se-1", Workflow: "feature-dev"},
		{ID: "se-2", Workflow: "human-flow"},
		{ID: "se-3", Workflow: "feature-dev"},
	}

	got := filterSessionsByWorkflow(in, "feature-dev")
	if len(got) != 2 || got[0].ID != "se-1" || got[1].ID != "se-3" {
		t.Errorf("workflow filter = %v, want se-1+se-3 only", got)
	}

	if all := filterSessionsByWorkflow(in, ""); len(all) != len(in) {
		t.Errorf("empty workflow should pass through unchanged, got %d of %d", len(all), len(in))
	}

	if none := filterSessionsByWorkflow(in, "nope"); len(none) != 0 {
		t.Errorf("no-match workflow should yield empty, got %v", none)
	}
}
