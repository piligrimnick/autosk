package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"autosk/internal/lazy/ansiutil"
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

// TestSortTasksByRecency pins the dashboard ordering: newest activity
// (UpdatedAt if set, else CreatedAt) bubbles to the top, regardless
// of status. The panel surfaces ALL statuses, so a recently-closed
// task must outrank an older-but-still-open one.
func TestSortTasksByRecency(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := []datasource.Task{
		// Oldest open task.
		{ID: "as-old", Status: store.StatusNew, CreatedAt: t0, UpdatedAt: t0},
		// Edited an hour later.
		{ID: "as-mid", Status: store.StatusWork,
			CreatedAt: t0, UpdatedAt: t0.Add(1 * time.Hour)},
		// Just-closed task should rank first — by-recency, not by status.
		{ID: "as-new", Status: store.StatusDone,
			CreatedAt: t0, UpdatedAt: t0.Add(2 * time.Hour)},
		// No UpdatedAt — falls back to CreatedAt, which is later than t0.
		{ID: "as-fb", Status: store.StatusNew,
			CreatedAt: t0.Add(30 * time.Minute)},
	}
	got := sortTasksByRecency(in)
	want := []string{"as-new", "as-mid", "as-fb", "as-old"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want=%d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("pos %d: got %q want %q", i, got[i].ID, w)
		}
	}
	// The function MUST NOT mutate its input — the slice handed back
	// from the datasource is owned by the caller, who may still hold a
	// reference (e.g. for diffing).
	if in[0].ID != "as-old" {
		t.Errorf("input mutated: in[0]=%q want as-old", in[0].ID)
	}
}

// TestSpinnerFrame pins the braille animation's frame order: the
// spinner column in the Tasks panel advances tick → tick+1 by one
// position through this list, and the design relies on every frame
// being one cell wide so the title column never shifts. Catching a
// future refactor that swaps in wider codepoints (e.g. spinner
// emojis) saves operators a width-of-column off-by-one bug.
func TestSpinnerFrame(t *testing.T) {
	want := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for i, w := range want {
		if got := spinnerFrame(uint64(i)); got != w {
			t.Errorf("tick %d: got %q want %q", i, got, w)
		}
	}
	// Wraps modulo len(frames) — tick is monotonic and uncapped.
	if got := spinnerFrame(uint64(len(want))); got != want[0] {
		t.Errorf("wrap: got %q want %q", got, want[0])
	}
	if got := spinnerFrame(uint64(len(want)*7 + 3)); got != want[3] {
		t.Errorf("wrap*7+3: got %q want %q", got, want[3])
	}
}

// TestTaskJobIndex_PartitionsByStatus pins the contract the Tasks
// panel's job-marker column relies on: Active is the subset of
// taskIDs with a RUNNING job; Any is every taskID with ANY job. The
// renderer picks the spinner (Active) before the static ">" marker
// (Any \ Active), so the partition has to be right or rows will
// either flicker or under-mark.
func TestTaskJobIndex_PartitionsByStatus(t *testing.T) {
	jobs := []datasource.Job{
		// Task A: one running + one queued — lands in BOTH sets.
		mkJob("job-1", "as-a", "running"),
		mkJob("job-2", "as-a", "queued"),
		// Task B: one done only — Any but NOT Active.
		mkJob("job-3", "as-b", "done"),
		// Task C: failed + cancel — Any but NOT Active.
		mkJob("job-4", "as-c", "failed"),
		mkJob("job-5", "as-c", "cancel"),
		// Daemon row with no TaskID (legacy) — must be skipped.
		mkJob("job-6", "", "running"),
	}
	idx := taskJobIndexFromJobs(jobs)
	if !idx.Active["as-a"] {
		t.Errorf("as-a missing from Active")
	}
	if idx.Active["as-b"] || idx.Active["as-c"] {
		t.Errorf("non-running task leaked into Active: %+v", idx.Active)
	}
	for _, id := range []string{"as-a", "as-b", "as-c"} {
		if !idx.Any[id] {
			t.Errorf("%s missing from Any", id)
		}
	}
	if idx.Any[""] || idx.Active[""] {
		t.Errorf("empty TaskID leaked into index: %+v", idx)
	}
	// Active \u2286 Any invariant.
	for id := range idx.Active {
		if !idx.Any[id] {
			t.Errorf("%s in Active but not Any — invariant violated", id)
		}
	}
}

// TestFilterJobsByTaskID pins the client-side TaskID slicer that
// fetchRefresh uses to recreate the Jobs-panel narrowing after
// dropping the server-side TaskID filter (so the marker index
// could see every task's jobs).
func TestFilterJobsByTaskID(t *testing.T) {
	jobs := []datasource.Job{
		mkJob("job-1", "as-a", "running"),
		mkJob("job-2", "as-a", "done"),
		mkJob("job-3", "as-b", "running"),
	}
	if got := filterJobsByTaskID(jobs, ""); len(got) != len(jobs) {
		t.Errorf("empty id should pass-through: got %d want %d", len(got), len(jobs))
	}
	got := filterJobsByTaskID(jobs, "as-a")
	if len(got) != 2 {
		t.Fatalf("as-a: got %d jobs want 2", len(got))
	}
	for _, j := range got {
		if j.TaskID != "as-a" {
			t.Errorf("leaked %q into as-a slice", j.TaskID)
		}
	}
	if got := filterJobsByTaskID(jobs, "as-missing"); len(got) != 0 {
		t.Errorf("unknown id should return empty: got %d", len(got))
	}
}

// TestTaskJobIndex_SurvivesTaskScope_Regression is the regression
// for the bug where pressing Space (scope.TaskID = "as-X") made the
// Tasks-panel ">" marker disappear from every row except as-X. The
// fix moved the marker index off the FILTERED jobs slice (used for
// the Jobs panel) and onto the TaskID-unfiltered one, so as long as
// fetchRefresh hands taskJobIndexFromJobs the full result the marker
// keeps painting on every task that has any job, regardless of which
// task the Jobs panel happens to be scoped to.
func TestTaskJobIndex_SurvivesTaskScope_Regression(t *testing.T) {
	// Simulate what fetchRefresh sees: the FULL jobs list (no
	// server-side TaskID filter) plus the scope.TaskID value the
	// operator just committed via Space.
	allJobs := []datasource.Job{
		mkJob("job-1", "as-a", "running"),
		mkJob("job-2", "as-b", "done"),
		mkJob("job-3", "as-c", "done"),
	}
	const scopedTaskID = "as-a"

	// The Jobs panel slice is narrowed client-side.
	jobsForPanel := filterJobsByTaskID(allJobs, scopedTaskID)
	if len(jobsForPanel) != 1 {
		t.Fatalf("panel slice: got %d jobs want 1", len(jobsForPanel))
	}

	// The marker index is built from the UNFILTERED list — every
	// task that has any job must remain marked.
	idx := taskJobIndexFromJobs(allJobs)
	for _, id := range []string{"as-a", "as-b", "as-c"} {
		if !idx.Any[id] {
			t.Errorf("task %s lost its > marker after scope.TaskID=%s", id, scopedTaskID)
		}
	}

	// And building the index from the FILTERED slice (the old, buggy
	// path) demonstrably loses the other rows — keep this assertion
	// so a future refactor that re-introduces "index from filtered"
	// trips the test instead of the operator.
	buggyIdx := taskJobIndexFromJobs(jobsForPanel)
	if buggyIdx.Any["as-b"] || buggyIdx.Any["as-c"] {
		t.Errorf("sanity: filtered slice unexpectedly contains as-b/as-c — test setup is wrong")
	}
}

// TestRenderTasksPanel_JobMarker pins the screenshot-driven marker
// column. We can't compare ANSI colours under `go test` (lipgloss
// drops them without a TTY), but the VISIBLE characters in the
// marker cell are stable: spinner braille for Active, ">" for Any-
// not-Active, single space when neither.
func TestRenderTasksPanel_JobMarker(t *testing.T) {
	tasks := []datasource.Task{
		{ID: "as-run", Title: "running task", Priority: 1, Status: store.StatusWork},
		{ID: "as-done", Title: "done task", Priority: 2, Status: store.StatusDone},
		{ID: "as-bare", Title: "jobless task", Priority: 2, Status: store.StatusNew},
	}
	idx := taskJobIndex{
		Active: map[string]bool{"as-run": true},
		Any:    map[string]bool{"as-run": true, "as-done": true},
	}
	// Tick 0 → spinnerFrames[0] = "⠋".
	body, _ := renderTasksPanel(tasks, 0, scope{}, "", 80, 0, idx)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d rows want 3: %v", len(lines), lines)
	}
	wantMarker := []string{"⠋", ">", " "}
	for i, line := range lines {
		plain := stripANSI(line)
		// Format: "P{n} as-XXXXXX  {marker} {status} ..."
		// The marker is the first non-space character after the
		// padded id column — walk char-by-char for robustness against
		// future width tweaks.
		got := jobMarkerOf(plain)
		if got != wantMarker[i] {
			t.Errorf("row %d (%s): marker=%q want %q (line=%q)",
				i, tasks[i].ID, got, wantMarker[i], plain)
		}
	}
}

// jobMarkerOf extracts the marker cell from a rendered Tasks-panel
// row by locating the id column's right edge: the id is
// "as-XXXX" (regex-shape) at exactly 7 cells, preceded by "P{prio} "
// (3 cells, prio+separator). So the marker starts at rune offset
// 3 + 7 + 1 = 11, and is exactly one cell wide.
func jobMarkerOf(plainRow string) string {
	const markerOffset = 3 + 7 + 1 // "P{n} as-XXXX "
	rs := []rune(plainRow)
	if len(rs) <= markerOffset {
		return ""
	}
	return string(rs[markerOffset])
}

// mkJob builds a minimal Job for the table tests above.
func mkJob(jobID, taskID, status string) datasource.Job {
	var j datasource.Job
	j.JobID = jobID
	j.TaskID = taskID
	j.Status = status
	return j
}

// TestStyleForTaskStatus_Distinct guards the screenshot-driven
// requirement that every status reads as a distinct colour. We can't
// rely on Style.Render() to emit ANSI in `go test` (lipgloss
// auto-detects the absence of a TTY and falls back to NoColor),
// so we compare the underlying Foreground values directly. A future
// palette tweak that accidentally collapses two statuses onto the
// same hue fails here before the operator notices on-screen.
func TestStyleForTaskStatus_Distinct(t *testing.T) {
	seen := map[string]store.Status{}
	for _, s := range store.AllStatuses() {
		fg := fmt.Sprintf("%v", styleForTaskStatus(s).GetForeground())
		if other, dup := seen[fg]; dup {
			t.Errorf("status %s collapses onto %s (same foreground %q)", s, other, fg)
		}
		seen[fg] = s
	}
}

// TestEntityStyles_Distinct guards the per-entity colour contract:
// task id, job id, agent name, workflow name, and step name must
// each render with a distinct foreground hue, otherwise the
// dashboard's "same colour means same kind of thing" affordance
// collapses. Compares Foreground values directly (lipgloss falls
// back to NoColor under `go test`, so Style.Render() output would
// be ambiguous).
func TestEntityStyles_Distinct(t *testing.T) {
	entities := map[string]lipgloss.Style{
		"task":     styleTaskID,
		"job":      styleJobID,
		"agent":    styleAgentName,
		"workflow": styleWorkflowName,
		"step":     styleStepName,
	}
	seen := map[string]string{}
	for name, st := range entities {
		fg := fmt.Sprintf("%v", st.GetForeground())
		if other, dup := seen[fg]; dup {
			t.Errorf("entity %s collapses onto %s (same foreground %q)", name, other, fg)
		}
		seen[fg] = name
	}
}

// TestEntityHelpers_Empty pins the renderEntity* short-circuit on
// empty inputs. The Tasks/Jobs detail panes pass through nullable
// fields (AuthorName, StepName, ...) and an empty Render call would
// otherwise emit a stray reset escape that shifts the next column.
func TestEntityHelpers_Empty(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) string
	}{
		{"task", renderTaskID},
		{"job", renderJobID},
		{"agent", renderAgentName},
		{"workflow", renderWorkflowName},
		{"step", renderStepName},
	}
	for _, c := range cases {
		if got := c.fn(""); got != "" {
			t.Errorf("render%sName(\"\")=%q want empty string", c.name, got)
		}
	}
}

// TestRenderWorkflowStep pins the layout the operator sees in the
// Jobs panel: "feature-dev-generic:review" renders as
// <yellow wf><default colon><red step>, with the empty/missing
// fallbacks matching the daemon's contract for legacy rows.
func TestRenderWorkflowStep(t *testing.T) {
	cases := []struct {
		wf, step string
		want     string // visible characters, ignoring ANSI
	}{
		{"feature-dev-generic", "review", "feature-dev-generic:review"},
		{"", "", "(no-wf)"},
		{"feature-dev", "", "feature-dev"},
	}
	for _, tc := range cases {
		got := stripANSI(renderWorkflowStep(tc.wf, tc.step))
		if got != tc.want {
			t.Errorf("wf=%q step=%q: got %q want %q", tc.wf, tc.step, got, tc.want)
		}
		if w := workflowStepPlainWidth(tc.wf, tc.step); w != len(tc.want) {
			t.Errorf("wf=%q step=%q: plainWidth=%d want %d", tc.wf, tc.step, w, len(tc.want))
		}
	}
}

// stripANSI is a thin shim around the canonical ansiutil.Strip so
// existing call sites in this test file (and across render_test.go)
// keep their short name. The implementation moved to
// internal/lazy/ansiutil so the markdown package's tests share the
// exact same fixer — see the as-a261 review comment trail for the
// drift that led to the dedup.
func stripANSI(s string) string { return ansiutil.Strip(s) }

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
