package render_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"autosk/internal/render"
	"autosk/internal/store"
)

func fixedIsolatedTask() store.Task {
	return store.Task{
		ID:          "ask-150001",
		Title:       "Isolated task",
		Description: "lives in a worktree",
		Status:      store.StatusWork,
		Priority:    1,
		CreatedAt:   time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
	}
}

// TestRender_WithWorktreeJSON verifies the worktree block lands in
// --json output when WithWorktree is supplied.
func TestRender_WithWorktreeJSON(t *testing.T) {
	wt := render.WorktreeJSON{
		Path:   "/Users/x/.autosk/worktrees/proj-aabbccdd/ask-150001",
		Branch: "autosk/ask-150001",
		Exists: true,
	}
	var buf bytes.Buffer
	if err := render.TaskJSONTo(&buf, fixedIsolatedTask(),
		render.WithBlocked(false, nil, nil),
		render.WithWorktree(&wt)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	wtm, ok := got["worktree"].(map[string]any)
	if !ok {
		t.Fatalf("worktree key missing or wrong type: %T %v", got["worktree"], got["worktree"])
	}
	if wtm["path"] != wt.Path {
		t.Errorf("worktree.path: %v", wtm["path"])
	}
	if wtm["branch"] != wt.Branch {
		t.Errorf("worktree.branch: %v", wtm["branch"])
	}
	if wtm["exists"] != true {
		t.Errorf("worktree.exists: %v", wtm["exists"])
	}
}

// TestRender_WithoutWorktreeOmitsBlock verifies the worktree key is
// omitted entirely when WithWorktree is not used. This keeps the
// existing JSON shape (and existing goldens) green for non-isolated
// tasks.
func TestRender_WithoutWorktreeOmitsBlock(t *testing.T) {
	var buf bytes.Buffer
	if err := render.TaskJSONTo(&buf, fixedIsolatedTask(),
		render.WithBlocked(false, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"worktree"`) {
		t.Fatalf("expected no worktree key, got %s", buf.String())
	}
}

// TestRender_TextHasWorktreeLines verifies the human renderer emits
// the worktree + branch lines when the block is supplied.
func TestRender_TextHasWorktreeLines(t *testing.T) {
	wt := render.WorktreeJSON{
		Path:   "/tmp/w/ask-150001",
		Branch: "autosk/ask-150001",
		Exists: true,
	}
	var buf bytes.Buffer
	if err := render.Task(&buf, fixedIsolatedTask(),
		render.WithBlocked(false, nil, nil),
		render.WithWorktree(&wt)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "worktree:      /tmp/w/ask-150001 (exists)") {
		t.Errorf("missing worktree line:\n%s", out)
	}
	if !strings.Contains(out, "branch:        autosk/ask-150001") {
		t.Errorf("missing branch line:\n%s", out)
	}
}

// TestRender_TextWorktreeMissingFlag verifies the "missing" suffix
// renders when the directory isn't on disk.
func TestRender_TextWorktreeMissingFlag(t *testing.T) {
	wt := render.WorktreeJSON{
		Path:   "/tmp/w/ask-150001",
		Branch: "autosk/ask-150001",
		Exists: false,
	}
	var buf bytes.Buffer
	if err := render.Task(&buf, fixedIsolatedTask(),
		render.WithBlocked(false, nil, nil),
		render.WithWorktree(&wt)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(missing)") {
		t.Errorf("expected (missing) suffix:\n%s", buf.String())
	}
}
