package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/timeformat"
)

// lipglossWidth is the on-screen cell width of s (ANSI-aware).
// Tiny alias around lipgloss.Width so the box-dimensions test reads
// the way the prose narrative talks about "visible width".
func lipglossWidth(s string) int { return lipgloss.Width(s) }

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

// TestRenderTaskDetail_EmptyCommentSkipped pins the new
// box-rendering contract for empty comments: a comment with an
// empty body must NOT produce a box at all (header included).
// The previous design (REMARK 3 from the as-a261 review) emitted
// a header line and skipped the body, leaving a wasted indented
// line behind; with each comment now wrapped in its own labeled
// box, the right thing to do is drop the entry entirely — a
// labeled box with no content conveys less than no entry at all.
// autosk_comment rejects empty bodies upstream so this is
// defensive, but rendering still has to behave sanely.
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

	// alice and carol have non-empty bodies — their author header
	// shows up on the box's top border.
	for _, name := range []string{"alice", "carol"} {
		if !strings.Contains(visible, name) {
			t.Errorf("author %q header missing from rendered output: %q", name, out)
		}
	}
	// bob's empty-body comment must be skipped entirely — no
	// labeled box with just a header and no content.
	if strings.Contains(visible, "bob") {
		t.Errorf("empty-body comment author %q leaked into rendered output: %q", "bob", out)
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

// TestRenderTaskDetail_HeaderRow pins the first-row contract from
// the as-a261 spec: P{prio} id status wf:step agent, no "task"
// label and no per-token labels ("wf=", "step=", "agent=").
// Priority is muted-gray so the row reads the same way the Tasks
// panel does.
func TestRenderTaskDetail_HeaderRow(t *testing.T) {
	task := datasource.Task{
		ID: "as-a261", Title: "—", Status: store.StatusHuman, Priority: 2,
		CreatedAt:    fixedTime,
		WorkflowName: "feature-dev-generic",
		StepName:     "validator",
		AgentName:    "@autogent/generic",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	firstLine := strings.SplitN(visible, "\n", 2)[0]
	want := "P2 as-a261 human feature-dev-generic:validator @autogent/generic"
	if firstLine != want {
		t.Errorf("first row mismatch:\n got: %q\nwant: %q", firstLine, want)
	}
	if strings.Contains(firstLine, "task ") {
		t.Errorf("unexpected \"task\" label literal on first row: %q", firstLine)
	}
	if strings.Contains(firstLine, "wf=") || strings.Contains(firstLine, "agent=") {
		t.Errorf("unexpected key=value tokens on first row: %q", firstLine)
	}
}

// TestRenderTaskDetail_BlockedRow pins the conditional "blocked by"
// row: it shows up only when the task is actually blocked and we
// have ids; the word "blocked" wears the err (red) hue.
func TestRenderTaskDetail_BlockedRow(t *testing.T) {
	task := datasource.Task{
		ID: "as-a261", Status: store.StatusWork, Priority: 1,
		CreatedAt: fixedTime,
		Blocked:   true,
		BlockedBy: []string{"as-0011", "as-0022"},
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "blocked by: as-0011, as-0022") {
		t.Errorf("missing blocked-by row with both ids: %q", visible)
	}
	// The word "blocked" must be styled in styleErr (red). We look for
	// the styled prefix the renderer emits — styleErr.Render("blocked")
	// wraps the word in an SGR escape ending with the visible token.
	if !strings.Contains(out, styleErr.Render("blocked")) {
		t.Errorf("\"blocked\" label not painted with styleErr (red): %q", out)
	}

	// And the row must NOT appear when Blocked=false, even if
	// BlockedBy still carries closed-blocker ids from a stale snapshot.
	task.Blocked = false
	out = renderTaskDetail(task, nil, nil, 80)
	visible = ansiutil.Strip(out)
	if strings.Contains(visible, "blocked by:") {
		t.Errorf("blocked-by row leaked when Blocked=false: %q", visible)
	}
}

// TestRenderTaskDetail_StatsRow pins the created+comments row: time
// without date for today (FormatDateTimeSmart contract), full
// datetime otherwise, and comma-separated formatting.
func TestRenderTaskDetail_StatsRow(t *testing.T) {
	// Reference "now" matching the test's CreatedAt local date so
	// FormatDateTimeSmart drops the date — we exercise the today
	// branch by passing a fixed CreatedAt and letting time.Now() act
	// as the smart-format reference. To keep the assertion stable
	// across CI clocks we use the formatted time string and check the
	// row contains it (instead of pinning a literal HH:MM:SS).
	task := datasource.Task{
		ID: "as-a261", Status: store.StatusNew,
		CreatedAt:    time.Now().Add(-30 * time.Minute),
		CommentCount: 12,
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	wantStamp := timeformat.FormatDateTimeSmart(task.CreatedAt)
	wantRow := "created: " + wantStamp + ", comments: 12"
	if !strings.Contains(visible, wantRow) {
		t.Errorf("stats row missing or mis-formatted; want substring %q in %q", wantRow, visible)
	}
	// Today branch: the rendered stamp has no date dashes.
	if strings.Contains(wantStamp, "-") {
		t.Fatalf("test fixture broken: smart stamp for now-30m should be time-only, got %q", wantStamp)
	}
}

// TestRenderTaskDetail_TitleBox pins the Title box contract: the
// task title sits inside a rounded box labelled "Title".
func TestRenderTaskDetail_TitleBox(t *testing.T) {
	task := datasource.Task{
		ID: "as-box", Status: store.StatusNew, CreatedAt: fixedTime,
		Title: "feat: markdown rendering of tasks description",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Title ") {
		t.Errorf("missing Title box top border: %q", visible)
	}
	if !strings.Contains(visible, task.Title) {
		t.Errorf("task title body missing from Title box: %q", visible)
	}
	if !strings.Contains(visible, "╰─") {
		t.Errorf("missing rounded-bottom-left corner anywhere in output: %q", visible)
	}
}

// TestRenderTaskDetail_DescriptionBox pins the Description box: the
// rendered markdown sits inside a box labelled "Description".
func TestRenderTaskDetail_DescriptionBox(t *testing.T) {
	task := datasource.Task{
		ID: "as-box", Status: store.StatusNew, CreatedAt: fixedTime,
		Description: "# Heading\n\nbody paragraph.\n",
	}
	out := renderTaskDetail(task, nil, nil, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Description ") {
		t.Errorf("missing Description box top border: %q", visible)
	}
	if !strings.Contains(visible, "Heading") {
		t.Errorf("description heading missing from box: %q", visible)
	}
}

// TestRenderTaskDetail_SignalsBoxAll pins the new contract that the
// Recent signals box now shows ALL signals, not just the last 3.
// Box label includes the count.
func TestRenderTaskDetail_SignalsBoxAll(t *testing.T) {
	task := datasource.Task{
		ID: "as-sig", Status: store.StatusWork, CreatedAt: fixedTime,
	}
	signals := []datasource.Signal{
		{StepName: "step-a", Target: "step-b", CreatedAt: fixedTime},
		{StepName: "step-b", Target: "step-c", CreatedAt: fixedTime},
		{StepName: "step-c", Target: "step-d", CreatedAt: fixedTime},
		{StepName: "step-d", Target: "step-e", CreatedAt: fixedTime},
		{StepName: "step-e", Target: "step-f", CreatedAt: fixedTime},
		{StepName: "step-f", Target: "done", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, nil, signals, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "╭─ Recent signals (6) ") {
		t.Errorf("missing \"Recent signals (6)\" box header: %q", visible)
	}
	// Every signal row must render — no last-3 truncation.
	for _, want := range []string{"step-a", "step-b", "step-c", "step-d", "step-e", "step-f"} {
		if !strings.Contains(visible, want) {
			t.Errorf("signal step %q missing from box; the box must show ALL signals, not just the last 3: %q", want, visible)
		}
	}
}

// TestRenderTaskDetail_SignalsTargetColored pins the contract that
// the right-hand side of each signal arrow wears its canonical
// entity colour: a sibling step name gets the step-name hue, while
// a lifecycle terminal (done/cancel/human) gets its task-status hue.
func TestRenderTaskDetail_SignalsTargetColored(t *testing.T) {
	task := datasource.Task{
		ID: "as-sig", Status: store.StatusWork, CreatedAt: fixedTime,
	}
	signals := []datasource.Signal{
		{StepName: "developer", Target: "validator", CreatedAt: fixedTime},
		{StepName: "validator", Target: "done", CreatedAt: fixedTime},
		{StepName: "developer", Target: "cancel", CreatedAt: fixedTime},
		{StepName: "developer", Target: "human", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, nil, signals, 80)

	// Step-name target wears renderStepName's hue.
	if !strings.Contains(out, renderStepName("validator")) {
		t.Errorf("step-name target \"validator\" not styled with step-name hue: %q", out)
	}
	// Lifecycle-terminal targets wear their task-status hue (same
	// styling renderWorkflowDetail uses on the step graph).
	for _, st := range []store.Status{store.StatusDone, store.StatusCancel, store.StatusHuman} {
		want := styleForTaskStatus(st).Render(string(st))
		if !strings.Contains(out, want) {
			t.Errorf("lifecycle terminal %q not styled with task-status hue: out=%q", st, out)
		}
	}
}

// TestRenderTaskDetail_FullWidthWrap pins the contract that the
// description body wraps at the FULL inner-box width, not at the
// old 120-cell readability cap. Symptom on a wide pane was a dead
// column of padding inside the right border because markdown
// wrapped at 120 even though the box extended further.
func TestRenderTaskDetail_FullWidthWrap(t *testing.T) {
	const paneW = 200
	long := strings.Repeat("word ", 80) // ~400 cells of fillable content
	task := datasource.Task{
		ID: "as-wide", Status: store.StatusNew, CreatedAt: fixedTime,
		Description: long,
	}
	out := renderTaskDetail(task, nil, nil, paneW)
	visible := ansiutil.Strip(out)

	// Locate the description body line(s) inside the box. Lines
	// inside the box start with "│ " (frame + padding). Measure the
	// LONGEST body line's visible-cell width and assert it stretches
	// closer to the box's inner content width than to the legacy
	// 120-cell cap.
	contentW := paneW - 4 // 2 frame cells + 2 padding cells
	maxBodyW := 0
	for _, ln := range strings.Split(visible, "\n") {
		if !strings.HasPrefix(ln, "│ ") || !strings.HasSuffix(ln, " │") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(ln, "│ "), " │")
		inner = strings.TrimRight(inner, " ")
		if w := utf8.RuneCountInString(inner); w > maxBodyW {
			maxBodyW = w
		}
	}
	// At paneW=200, contentW=196. We expect the wrap to land near
	// contentW (within a word's worth of slack). A regression to the
	// old 120-cell cap would land maxBodyW at ~120.
	if maxBodyW < contentW-10 {
		t.Errorf("description body wraps too early: maxBodyW=%d, contentW=%d (a regression to the legacy 120-cell cap would land here at ~120):\n%s",
			maxBodyW, contentW, visible)
	}
}

// TestRenderTaskDetail_CommentBoxLabel pins the per-comment box
// label contract: "<smart-time> <author>" on the top border, body
// inside the frame, no leftover "─ comments (N) ─" section header.
func TestRenderTaskDetail_CommentBoxLabel(t *testing.T) {
	task := datasource.Task{
		ID: "as-c", Status: store.StatusNew, CreatedAt: fixedTime,
	}
	comments := []datasource.Comment{
		{ID: 1, AuthorName: "alice", Text: "hello world", CreatedAt: fixedTime},
	}
	out := renderTaskDetail(task, comments, nil, 80)
	visible := ansiutil.Strip(out)
	wantStamp := timeformat.FormatDateTimeSmart(fixedTime)
	wantHeader := "╭─ " + wantStamp + " alice "
	if !strings.Contains(visible, wantHeader) {
		t.Errorf("missing comment-box header %q in: %q", wantHeader, visible)
	}
	if !strings.Contains(visible, "hello world") {
		t.Errorf("comment body missing inside box: %q", visible)
	}
	// The old "─ comments (N) ─" section header must NOT survive —
	// each comment now wears its own box label.
	if strings.Contains(visible, "─ comments (") {
		t.Errorf("legacy \"─ comments (N) ─\" section header leaked: %q", visible)
	}
}

// TestWrapPlain spot-checks the plain-text word wrap helper used by
// the Title box.
func TestWrapPlain(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"empty", "", 10, ""},
		{"short", "abc def", 10, "abc def"},
		{"wraps_at_word", "alpha beta gamma", 10, "alpha beta\ngamma"},
		{"hard_break_long_word", "abcdefghijklmnop", 5, "abcde\nfghij\nklmno\np"},
		// width=6 fits one 4-rune word per line but not "два три"
		// together (3+1+3=7>6), so the wrap drops to one word per
		// line for the short middles too.
		{"cyrillic", "Один два три четыре", 6, "Один\nдва\nтри\nчетыре"},
		{"cyrillic_wider", "Один два три четыре", 8, "Один два\nтри\nчетыре"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapPlain(tc.in, tc.width)
			if got != tc.want {
				t.Errorf("wrapPlain(%q, %d):\n got:  %q\n want: %q", tc.in, tc.width, got, tc.want)
			}
		})
	}
}

// TestDrawLabeledBox_Dimensions pins the box's width invariant: top
// and bottom borders are exactly `width` cells wide regardless of
// label length, and each body line is padded to the same visible
// width as the borders.
func TestDrawLabeledBox_Dimensions(t *testing.T) {
	const width = 30
	out := drawLabeledBox("Lbl", "hello\nworld", width)
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (top + 2 body + bottom); got %d: %q", len(lines), out)
	}
	for i, ln := range lines {
		w := lipglossWidth(ln)
		if w != width {
			t.Errorf("line %d width %d, want %d: %q", i, w, width, ln)
		}
	}
	// Frame-less fallback at impossibly narrow widths.
	if got := drawLabeledBox("x", "hi", 3); got != "hi" {
		t.Errorf("narrow box should fall back to body; got %q", got)
	}
}
