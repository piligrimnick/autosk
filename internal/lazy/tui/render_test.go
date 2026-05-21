package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// stripANSI is defined in refresh_test.go (a thin alias over
// ansiutil.Strip) and shared across render-pane tests so substring
// assertions can ignore SGR escapes layered over the visible text.

// TestTruncate_RuneSafe verifies that truncate doesn't slice inside a
// multibyte rune (which would crash for some boundary positions and
// produce mojibake for others). The previous byte-slicing version
// would panic on len("Рефакторинг") > 30 == false but on a longer
// Cyrillic title (e.g. 22 runes = 44 bytes) the byte slice would fall
// mid-codepoint.
func TestTruncate_RuneSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
	}{
		{"ascii_short", "abc", 10},
		{"ascii_long", strings.Repeat("a", 80), 30},
		{"cyrillic_long", strings.Repeat("Рефакторинг ", 4), 30},
		{"emoji_long", strings.Repeat("🚀 ", 20), 10},
		{"cjk_long", strings.Repeat("作業中", 10), 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncate panicked: %v", r)
				}
			}()
			got := truncate(tc.in, tc.n)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8 output: %q", got)
			}
			// utf8.RuneCountInString(input) <= n → unchanged.
			if utf8.RuneCountInString(tc.in) <= tc.n {
				if got != tc.in {
					t.Errorf("unchanged input was mutated: %q→%q", tc.in, got)
				}
				return
			}
			if utf8.RuneCountInString(got) != tc.n {
				t.Errorf("rune count: got %d want %d (output %q)",
					utf8.RuneCountInString(got), tc.n, got)
			}
		})
	}
}

// TestTruncate_NEdge: n < 1 → empty.
func TestTruncate_NEdge(t *testing.T) {
	if got := truncate("hello", 0); got != "" {
		t.Errorf("n=0: got %q want empty", got)
	}
	if got := truncate("hello", -3); got != "" {
		t.Errorf("n<0: got %q want empty", got)
	}
}

// fixedTime gives every fixture row a deterministic creation stamp
// so timeformat output stays stable across test runs and CI clocks.
var fixedTime = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

// TestRenderTaskDetail_Markdown verifies that markdown in
// task.Description is rendered through glamour and not surfaced as
// raw characters. The literal "# Title" marker disappears (heading
// styled, hash stripped) and the body picks up ANSI escapes.
func TestRenderTaskDetail_Markdown(t *testing.T) {
	task := datasource.Task{
		ID:          "as-0001",
		Title:       "fixture",
		Status:      store.StatusNew,
		Priority:    2,
		CreatedAt:   fixedTime,
		Description: "# Title\n\n- one\n- two\n",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	if out == "" {
		t.Fatal("renderTaskDetail returned empty")
	}
	visible := stripANSI(out)
	if !strings.Contains(visible, "Title") {
		t.Errorf("heading text missing from rendered output: %q", out)
	}
	// glamour styles the heading with its prefix ("# ") and ANSI;
	// the raw markdown "# Title" with no escape codes between the
	// hash and the title shouldn't survive verbatim.
	if strings.Contains(out, "# Title\n") {
		t.Errorf("raw markdown heading leaked unstyled into output: %q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI escapes in rendered output: %q", out)
	}
	// Bullet glyph (StyleConfig.Item.BlockPrefix = "• ") proves the
	// list went through markdown.Render, not raw passthrough.
	if !strings.Contains(visible, "•") {
		t.Errorf("bullet glyph missing from rendered list: %q", out)
	}
}

// TestRenderTaskDetail_EmptyDescription pins the "(no description)"
// placeholder (acceptance criterion #4). An empty description must
// NOT be routed through glamour — the placeholder is the contract.
func TestRenderTaskDetail_EmptyDescription(t *testing.T) {
	task := datasource.Task{
		ID:          "as-0002",
		Title:       "fixture",
		Status:      store.StatusNew,
		CreatedAt:   fixedTime,
		Description: "",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := stripANSI(out)
	if !strings.Contains(visible, "(no description)") {
		t.Errorf("missing (no description) placeholder: %q", out)
	}
}

// TestRenderTaskDetail_CommentsMultiline pins the comment-rendering
// contract from acceptance criterion #3: comments are not clipped
// to 70 chars, multi-line text renders in full, and the "last 5"
// cap is preserved.
func TestRenderTaskDetail_CommentsMultiline(t *testing.T) {
	long := strings.Repeat("word ", 30) // >70 chars on a single line
	multi := "first line\nsecond line\nthird line"
	comments := []datasource.Comment{
		{ID: 1, AuthorName: "alice", Text: "c1", CreatedAt: fixedTime},
		{ID: 2, AuthorName: "bob", Text: "c2", CreatedAt: fixedTime},
		{ID: 3, AuthorName: "carol", Text: long, CreatedAt: fixedTime},
		{ID: 4, AuthorName: "dave", Text: multi, CreatedAt: fixedTime},
		{ID: 5, AuthorName: "eve", Text: "c5", CreatedAt: fixedTime},
		{ID: 6, AuthorName: "frank", Text: "c6", CreatedAt: fixedTime},
	}
	task := datasource.Task{
		ID: "as-0003", Title: "f", Status: store.StatusNew, CreatedAt: fixedTime,
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := stripANSI(out)

	// Cap kept: the oldest comment (c1, by alice) is dropped; the
	// last 5 (bob..frank) are present.
	if strings.Contains(visible, "alice") {
		t.Errorf("oldest comment leaked past last-5 cap: %q", out)
	}
	for _, name := range []string{"bob", "carol", "dave", "eve", "frank"} {
		if !strings.Contains(visible, name) {
			t.Errorf("expected author %q in last-5 comments, got: %q", name, out)
		}
	}

	// Multi-line comment renders in full — every line of dave's text
	// makes it through markdown.Render (glamour collapses adjacent
	// lines into one paragraph, so the words have to be present even
	// if the line layout changes).
	for _, word := range []string{"first", "second", "third"} {
		if !strings.Contains(visible, word) {
			t.Errorf("multi-line word %q missing from rendered comment: %q", word, out)
		}
	}

	// Long comment is NOT clipped at 70 chars + ellipsis: every
	// repetition of "word" survives the render (glamour may wrap
	// but won't drop content).
	wordCount := strings.Count(visible, "word")
	if wordCount < 25 {
		t.Errorf("long comment looks clipped: only %d 'word' occurrences in %q",
			wordCount, out)
	}
	if strings.Contains(out, "…") && strings.Contains(out, "word…") {
		t.Errorf("comment appears clipped with ellipsis: %q", out)
	}
}

// TestRenderTaskDetail_EmptyCommentSkipped pins REMARK 3 from the
// as-a261 review: a comment with an empty body must not emit a
// wasted blank indented line under its header. autosk_comment
// rejects empty bodies upstream so this is purely defensive, but
// the rendering still has to behave sanely.
func TestRenderTaskDetail_EmptyCommentSkipped(t *testing.T) {
	comments := []datasource.Comment{
		{ID: 1, AuthorName: "alice", Text: "first body", CreatedAt: fixedTime},
		{ID: 2, AuthorName: "bob", Text: "", CreatedAt: fixedTime},
		{ID: 3, AuthorName: "carol", Text: "third body", CreatedAt: fixedTime},
	}
	task := datasource.Task{
		ID: "as-empty", Title: "f", Status: store.StatusNew, CreatedAt: fixedTime,
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := ansiutil.Strip(out)

	// All three headers are present; only the bob entry has no body.
	for _, name := range []string{"alice", "bob", "carol"} {
		if !strings.Contains(visible, name) {
			t.Errorf("author %q header missing: %q", name, out)
		}
	}

	// Locate bob's header line and assert that the next non-empty
	// line is carol's header — not a blank indented line from the
	// empty body. Scanning by line keeps the assertion robust to
	// future whitespace tweaks elsewhere in the pane.
	lines := strings.Split(visible, "\n")
	var bobIdx, carolIdx int = -1, -1
	for i, ln := range lines {
		if bobIdx < 0 && strings.Contains(ln, "bob") {
			bobIdx = i
		}
		if carolIdx < 0 && strings.Contains(ln, "carol") {
			carolIdx = i
		}
	}
	if bobIdx < 0 || carolIdx < 0 {
		t.Fatalf("bob/carol headers not located: %q", visible)
	}
	// Header lines for bob and carol should be adjacent: nothing
	// from bob's (empty) body should sit between them.
	if carolIdx-bobIdx != 1 {
		t.Errorf("expected bob header immediately followed by carol header (delta=1); got delta=%d:\n%s",
			carolIdx-bobIdx, visible)
	}
}

// TestRenderWorkflowDetail_Markdown verifies that workflow.Description
// is rendered through the same markdown pipeline as task descriptions.
func TestRenderWorkflowDetail_Markdown(t *testing.T) {
	wf := datasource.Workflow{
		Name:        "wf-test",
		Description: "# Workflow Heading\n\nSome **bold** text.",
		FirstStep:   "dev",
		Steps: []datasource.WorkflowStep{
			{Name: "dev", AgentName: "a1"},
		},
	}
	out := renderWorkflowDetail(wf, 80)
	visible := stripANSI(out)
	if !strings.Contains(visible, "Workflow Heading") {
		t.Errorf("heading text missing: %q", out)
	}
	if strings.Contains(out, "# Workflow Heading\n") {
		t.Errorf("raw markdown heading leaked unstyled: %q", out)
	}
	if strings.Contains(out, "**bold**") {
		t.Errorf("raw bold marker leaked: %q", out)
	}
	if !strings.Contains(visible, "bold") {
		t.Errorf("bold text payload missing: %q", out)
	}
}

// TestRenderTaskDetail_LongDescription_Scrollable pins REMARK 6 from
// the as-a261 review: with markdown rendering live, a multi-paragraph
// description must produce enough rendered lines for the detail
// pane's scroll machinery (gocui's View.oy, driven by detailScroll
// in inspector.go) to have something to scroll over. The previous
// truncate-to-70-chars rendering capped comments at one line each
// and descriptions at whatever fit; a future refactor that
// accidentally re-truncates would regress scrolling silently.
//
// This is a one-shot "the pane has scrollable content" assertion;
// the actual scroll-position-survives-clear contract belongs to
// gocui (View.Clear nulls line buffers but preserves oy) and is
// exercised by the lazy TUI's own integration runs, not by a unit
// test against renderTaskDetail.
func TestRenderTaskDetail_LongDescription_Scrollable(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# Long description\n\n")
	for i := 0; i < 100; i++ {
		sb.WriteString("- bullet line that is long enough to take a row in the pane\n")
	}
	task := datasource.Task{
		ID: "as-long", Title: "f", Status: store.StatusNew, CreatedAt: fixedTime,
		Description: sb.String(),
	}
	out := renderTaskDetail(task, nil, nil, 80)
	// Header + metadata lines are <10. A markdown render of 100
	// bullets should produce well over 50 newline-terminated lines;
	// pick a conservative floor so the test isn't a fragile
	// glamour-version pin while still catching a re-truncate.
	newlines := strings.Count(out, "\n")
	if newlines < 50 {
		t.Errorf("rendered detail too short for scrolling (%d lines); a re-truncate may have crept back in. out=%q",
			newlines, out)
	}
	// And no ellipsis chevron sneaking in from a truncate path.
	visible := ansiutil.Strip(out)
	if strings.Contains(visible, "bullet line that is long enough to take a row in the pane\u2026") {
		t.Errorf("bullet line was truncated with ellipsis; got %q", visible)
	}
}

// TestRenderTaskDetail_ZeroWidth pins the fail-open behaviour for
// the first-frame layout pass: when innerWidth returns 0 (the view
// hasn't been sized yet), markdown.Render emits the raw text and
// renderTaskDetail still produces a usable pane.
func TestRenderTaskDetail_ZeroWidth(t *testing.T) {
	task := datasource.Task{
		ID: "as-0004", Title: "f", Status: store.StatusNew,
		CreatedAt:   fixedTime,
		Description: "# Title\n\nbody",
	}
	out := renderTaskDetail(task, nil, nil, 0)
	visible := stripANSI(out)
	if !strings.Contains(visible, "# Title") {
		t.Errorf("width=0 should pass markdown through verbatim, missing source: %q", out)
	}
	if !strings.Contains(visible, "body") {
		t.Errorf("body text missing at width=0: %q", out)
	}
}
