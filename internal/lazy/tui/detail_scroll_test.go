package tui

import (
	"strings"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
)

// detailScrollFixture wires a headless gui big enough to host a
// dashboard layout AND a non-trivial winDetail body. The fixture is
// shared by the regression tests pinning the three bugs the user
// reported on a recent lazy session:
//
//  1. Selecting a status=work task should NOT auto-scroll the
//     Detail pane to its bottom (only Job Detail's live transcript
//     wants the sticky-tail anchor).
//  2. Selecting a status=work task should NOT make winJobInput
//     appear just because the Jobs panel's cursor happens to point
//     at a live job — the input is a Job-Detail attachment, not a
//     dashboard-wide modifier.
//  3. Switching the entity rendered into the Detail pane (e.g.
//     Tasks panel cursor moves from task-A to task-B, or focus
//     jumps from Tasks to Jobs) should reset winDetail's viewport
//     origin so the new entity starts from its natural anchor;
//     same entity repaints should preserve the operator's scroll
//     position.
func detailScrollFixture(t *testing.T) *Gui {
	t.Helper()
	gu := newHeadlessGui(t, 120, 40)
	gu.ds = &refreshFakeDS{}
	return gu
}

// TestRenderViews_Task_NoAutoScrollToBottom pins bug 1 of the
// reported set: selecting a status=work task must not yank the
// Detail viewport to the bottom (which would hide the title +
// description above the tail of comments). The sticky-tail
// behaviour is reserved for Job Detail's live transcript.
func TestRenderViews_Task_NoAutoScrollToBottom(t *testing.T) {
	gu := detailScrollFixture(t)
	// Seed a task whose rendered Detail body fits in the viewport
	// comfortably enough that the bug's symptom (scrolling past
	// the top) is visible: many comments forces the body to be
	// taller than winDetail's inner height.
	manyComments := make([]datasource.Comment, 20)
	for i := range manyComments {
		manyComments[i] = datasource.Comment{
			ID:         int64(i + 1),
			AuthorName: "tester",
			Text:       strings.Repeat("comment line\n", 5),
			CreatedAt:  time.Now().Add(time.Duration(-i) * time.Minute),
		}
	}
	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{{
			ID:           "ask-aaaaaa",
			Title:        "the title",
			Description:  "the description body",
			Status:       store.StatusWork,
			Priority:     2,
			CreatedAt:    time.Now(),
			CommentCount: len(manyComments),
		}}
		gu.st.taskCursor = 0
		gu.st.focused = panelTasks
		gu.st.comments["ask-aaaaaa"] = manyComments
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, err := gu.g.View(winDetail)
	if err != nil || v == nil {
		t.Fatalf("winDetail missing: %v", err)
	}
	_, oy := v.Origin()
	if oy != 0 {
		t.Errorf("Detail viewport auto-scrolled past the top of a Task body: oy=%d, want 0", oy)
	}
	// Sanity: the body is taller than the viewport (otherwise the
	// regression couldn't manifest at all, and the test would pass
	// trivially).
	_, h := v.InnerSize()
	bodyLines := strings.Count(v.Buffer(), "\n")
	if bodyLines <= h {
		t.Logf("note: task body (%d lines) <= viewport height (%d); the regression's preconditions weren't met but the assertion still holds", bodyLines, h)
	}
}

// TestLayout_JobInput_HiddenWhenDetailShowsTask pins bug 2:
// winJobInput must NOT appear just because the Jobs panel's
// cursor happens to point at a live job — it's an attachment to
// the Job-Detail body, so it should be gated on "the Detail pane
// is currently showing that job".
//
// Scenario: the operator is on the Tasks panel (Detail shows the
// task body); the Jobs panel's cursor is on a running job (one
// row out of view, but it's the model's "selected job"). The
// input must stay hidden.
func TestLayout_JobInput_HiddenWhenDetailShowsTask(t *testing.T) {
	gu := detailScrollFixture(t)
	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{{
			ID: "ask-aaaaaa", Title: "task on screen",
			Status: store.StatusWork,
		}}
		gu.st.taskCursor = 0
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-live", Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		// Focus is on Tasks, NOT Jobs.
		gu.st.focused = panelTasks
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	if _, err := gu.g.View(winJobInput); err == nil {
		t.Errorf("winJobInput unexpectedly visible while Detail shows a Task (Jobs cursor points at a live job, but the Detail body is the task)")
	}
}

// TestLayout_JobInput_VisibleWhenDetailFocusedShowsJob pins the
// flip side of bug 2: when the operator has focused the Detail
// pane via '0' (state.focused == panelDetail) AND detailFocus
// records that Job Detail is being shown, winJobInput must
// appear — the operator can still author input while reading
// the transcript above. This is the existing "input stays pinned
// when Detail is focused" affordance, and the new gate must not
// break it.
func TestLayout_JobInput_VisibleWhenDetailFocusedShowsJob(t *testing.T) {
	gu := detailScrollFixture(t)
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-live", Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelDetail
		gu.st.detailFocus = panelJobs
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	if _, err := gu.g.View(winJobInput); err != nil {
		t.Errorf("winJobInput missing while Detail (focused) shows a live Job: %v", err)
	}
}

// TestRenderViews_DetailOriginResetsOnEntityChange pins the
// scroll-anchor contract: when the entity rendered into the
// Detail pane changes (cursor moves to a different task, focus
// jumps between panels), the viewport origin must reset to 0 so
// the new entity starts from its top. When the same entity is
// repainted (e.g. a refresh tick with no entity change), the
// operator's scroll position must be preserved.
func TestRenderViews_DetailOriginResetsOnEntityChange(t *testing.T) {
	gu := detailScrollFixture(t)
	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{
			{ID: "ask-aaaaaa", Title: strings.Repeat("alpha ", 20), Status: store.StatusWork},
			{ID: "ask-bbbbbb", Title: strings.Repeat("beta ", 20), Status: store.StatusWork},
		}
		gu.st.taskCursor = 0
		gu.st.focused = panelTasks
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (task-A): %v", err)
	}
	v, _ := gu.g.View(winDetail)
	if v == nil {
		t.Fatalf("winDetail missing")
	}
	// Pretend the operator scrolled the Detail pane down by a few
	// rows (simulating j inside Detail, or a wheel-down on the
	// pane). The next layout for the SAME task must preserve that
	// scroll position.
	v.SetOrigin(0, 3)
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (task-A repaint): %v", err)
	}
	if _, oy := v.Origin(); oy != 3 {
		t.Errorf("repaint of the same task clobbered scroll position: oy=%d, want 3", oy)
	}
	// Now move the cursor to task-B. The entity in Detail changes
	// → viewport origin MUST reset to 0.
	gu.st.withLock(func() { gu.st.taskCursor = 1 })
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (task-B): %v", err)
	}
	if _, oy := v.Origin(); oy != 0 {
		t.Errorf("Detail viewport did not reset on task-A → task-B switch: oy=%d, want 0", oy)
	}
}

// TestRenderViews_DetailJobStillStickyTails pins the job-side
// invariant: switching INTO Job Detail (or repainting a job
// transcript while at the bottom) must still snap to the tail so
// new live events are visible without manual scroll. This is the
// behaviour the original writeViewSticky was added for; the new
// entity-change-resets-origin path must not regress it.
func TestRenderViews_DetailJobStillStickyTails(t *testing.T) {
	gu := detailScrollFixture(t)
	// Seed a job with a large transcript already populated so the
	// initial render forces the viewport past the top. The default
	// "(loading...)" placeholder would otherwise leave nothing to
	// scroll.
	now := time.Now()
	events := make([]datasource.MessageEvent, 50)
	for i := range events {
		events[i] = datasource.MessageEvent{
			Kind: "assistant_text",
			Text: "event " + strings.Repeat("X", 60),
			TS:   now.Add(time.Duration(i) * time.Second),
		}
	}
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-X", Status: "running", Streaming: true},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobTranscript["job-X"] = &jobTranscriptEntry{
			events:   events,
			loadedAt: now,
		}
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, _ := gu.g.View(winDetail)
	if v == nil {
		t.Fatalf("winDetail missing")
	}
	// Sticky-tail must have advanced the origin past 0; otherwise
	// the operator would only see the system prompt / oldest events
	// at the top of the transcript, not the latest output.
	_, oy := v.Origin()
	if oy == 0 {
		bufLines := strings.Count(v.Buffer(), "\n")
		_, h := v.InnerSize()
		t.Errorf("Job Detail did not sticky-tail on first render: oy=0 (bufLines=%d, innerH=%d)", bufLines, h)
	}
}

// TestRenderViews_DetailKeyTracking_FocusJumpResetsOrigin
// extends TestRenderViews_DetailOriginResetsOnEntityChange to the
// focus-jump scenario: pressing the panel-jump keys (1/2/3/4) or
// Tab moves the entity rendered into Detail from one kind to
// another. The reset must fire on these jumps too, not only on
// per-panel cursor moves.
func TestRenderViews_DetailKeyTracking_FocusJumpResetsOrigin(t *testing.T) {
	gu := detailScrollFixture(t)
	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{{ID: "ask-a", Title: "t", Status: store.StatusWork}}
		gu.st.jobs = []datasource.Job{{JobResponse: api.JobResponse{JobID: "job-1", Status: "running"}}}
		gu.st.workflows = []datasource.Workflow{{ID: "wf-1", Name: "feature-dev"}}
		gu.st.focused = panelTasks
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (tasks): %v", err)
	}
	v, _ := gu.g.View(winDetail)
	if v == nil {
		t.Fatalf("winDetail missing")
	}
	v.SetOrigin(0, 5)

	// Jump focus to Workflows. Detail now shows the workflow body
	// (different entity kind), so the origin must reset.
	gu.st.withLock(func() { gu.st.focused = panelWorkflows })
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (workflows): %v", err)
	}
	if _, oy := v.Origin(); oy != 0 {
		t.Errorf("focus-jump Tasks → Workflows did not reset Detail origin: oy=%d, want 0", oy)
	}
}

// TestDetailEntityKey_PanelDetailFocusUsesDetailFocus pins the
// panelDetail special case: when state.focused == panelDetail (the
// operator pressed '0'), the entity key must come from state.detailFocus
// — otherwise renderViews would think Detail is showing "panelDetail"
// (which has no entity) and the key would jam at "" while the body
// actually swaps between task / job / workflow as the operator
// presses '0' from different side panels.
func TestDetailEntityKey_PanelDetailFocusUsesDetailFocus(t *testing.T) {
	st := newState()
	st.focused = panelDetail
	st.detailFocus = panelJobs
	st.jobs = []datasource.Job{{JobResponse: api.JobResponse{JobID: "job-Z"}}}
	st.jobCursor = 0
	if got := detailEntityKey(st); got != "job:job-Z" {
		t.Errorf("detailEntityKey(focused=panelDetail, detailFocus=panelJobs) = %q, want %q", got, "job:job-Z")
	}
	if !detailShowsJob(st) {
		t.Errorf("detailShowsJob should be true when detailFocus normalises to panelJobs")
	}
}

// TestDetailEntityKey_PanelJobInputNormalises pins the
// normalizeForDetail collapse for the synthetic panelJobInput: the
// operator typing in the input still has Job Detail above, so the
// key must read as a job entry.
func TestDetailEntityKey_PanelJobInputNormalises(t *testing.T) {
	st := newState()
	st.focused = panelJobInput
	st.jobs = []datasource.Job{{JobResponse: api.JobResponse{JobID: "job-Y"}}}
	st.jobCursor = 0
	if got := detailEntityKey(st); got != "job:job-Y" {
		t.Errorf("detailEntityKey(focused=panelJobInput) = %q, want %q", got, "job:job-Y")
	}
}

// TestDetailBottomBorderVisible_LastBox pins the bottom-anchor
// off-by-one the operator reported: "у последнего блока в панеле
// Detail не видно нижней границы блока". The Detail pane stacks
// drawLabeledBox blocks; the last block's bottom-border row (╯──╮)
// is the last line of the buffer. After G (or sticky-tail anchoring),
// the viewport origin must put that line INSIDE the visible region
// [oy, oy+effectiveH). The buggy version used `strings.Count(buf,
// "\n")` for the line count — one less than the actual line count,
// because gocui's linesToString omits the trailing separator —
// which left the last line one row past the visible region.
func TestDetailBottomBorderVisible_LastBox(t *testing.T) {
	gu := detailScrollFixture(t)
	// Seed a job with enough transcript boxes that the rendered
	// body overflows winDetail's inner height. The exact content
	// doesn't matter — we just need the body to exceed the viewport
	// so sticky-tail / G have to actually clamp.
	now := time.Now()
	events := make([]datasource.MessageEvent, 30)
	for i := range events {
		events[i] = datasource.MessageEvent{
			Kind: "assistant_text",
			Text: "event " + strings.Repeat("X", 40),
			TS:   now.Add(time.Duration(i) * time.Second),
		}
	}
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-X", Status: "done"},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobTranscript["job-X"] = &jobTranscriptEntry{
			events:   events,
			loadedAt: now,
		}
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, _ := gu.g.View(winDetail)
	if v == nil {
		t.Fatalf("winDetail missing")
	}
	// Don't depend on the sticky-tail path — explicitly drive G so
	// detailScrollTo / scrollViewByLines / viewBufferLineCount all
	// participate.
	if err := gu.detailScrollTo(true)(nil, nil); err != nil {
		t.Fatalf("detailScrollTo: %v", err)
	}
	_, oy := v.Origin()
	effH := gu.detailEffectiveInnerH()
	lineCount := viewBufferLineCount(v)
	if lineCount <= effH {
		t.Skipf("fixture preconditions not met: lineCount=%d effH=%d (body fits in viewport, regression can't manifest)", lineCount, effH)
	}
	// The last buffer line must be inside [oy, oy+effH). With the
	// off-by-one fix, oy = lineCount - effH; the bottom of the
	// visible region is oy + effH = lineCount, so the last line
	// index (lineCount-1) is inside [oy, oy+effH). The buggy
	// formula had oy + effH = lineCount-1 and the last line was
	// one row past the visible edge.
	if oy+effH < lineCount {
		t.Errorf("last buffer line clipped: oy=%d effH=%d lineCount=%d (need oy+effH >= lineCount)", oy, effH, lineCount)
	}
}

// TestRenderViews_DetailRepaintWithSameKeyPreservesScroll is the
// positive companion to TestRenderViews_DetailOriginResetsOnEntityChange:
// repeated renderViews calls on the same entity must NOT touch
// the operator's scroll position. The body cache short-circuit
// covers the no-change case; this test exercises the case where
// the body DID change (e.g. a new comment lands) but the entity
// key is stable.
func TestRenderViews_DetailRepaintWithSameKeyPreservesScroll(t *testing.T) {
	gu := detailScrollFixture(t)
	gu.st.withLock(func() {
		gu.st.tasks = []datasource.Task{{
			ID: "ask-aaaaaa", Title: "t",
			Description: strings.Repeat("paragraph\n\n", 20),
			Status:      store.StatusWork,
		}}
		gu.st.taskCursor = 0
		gu.st.focused = panelTasks
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (initial): %v", err)
	}
	v, _ := gu.g.View(winDetail)
	if v == nil {
		t.Fatalf("winDetail missing")
	}
	v.SetOrigin(0, 7)

	// Same task, but with a new comment appended (so the body
	// string changes). The cache miss path must still preserve oy.
	gu.st.withLock(func() {
		gu.st.comments["ask-aaaaaa"] = []datasource.Comment{{
			ID: 1, AuthorName: "tester", Text: "fresh comment", CreatedAt: time.Now(),
		}}
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (after comment): %v", err)
	}
	if _, oy := v.Origin(); oy != 7 {
		t.Errorf("same-entity repaint with body change clobbered scroll: oy=%d, want 7", oy)
	}
}

