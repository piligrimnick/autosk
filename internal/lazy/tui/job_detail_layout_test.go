package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
)

// jobDetailLayoutFixture wires a headless gui with a refreshFakeDS
// so the layout pass has somewhere to read from. A background
// context is attached so handlers that schedule refreshes can
// derive timeouts without panicking on a nil parent.
func jobDetailLayoutFixture(t *testing.T) *Gui {
	t.Helper()
	gu := newHeadlessGui(t, 120, 40)
	gu.ds = &refreshFakeDS{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gu.ctx = ctx
	return gu
}

// seedRunningJob seeds one running job in the model and focuses the
// Jobs panel.
func seedRunningJob(gu *Gui, jobID string) {
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "running"},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
}

// seedTerminalJob seeds one done job in the model.
func seedTerminalJob(gu *Gui, jobID string) {
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: jobID, Status: "done"},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
}

// TestJobInput_DoesNotLeakAcrossJobs pins the regression review
// flagged: drafting text against job-A and then moving the cursor
// to job-B must NOT leave job-A's text in winJobInput. The
// jobInputOwner field is the contract — clearJobInputIfStale
// drops the cached buffer when the owner doesn't match the new
// cursor row.
func TestJobInput_DoesNotLeakAcrossJobs(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	// Seed two running jobs.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running"}},
			{JobResponse: api.JobResponse{JobID: "job-B", Status: "running"}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		// Pretend the user has typed "hello to A" into job-A's input.
		gu.st.jobInput = "hello to A"
		gu.st.jobInputOwner = "job-A"
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (cursor on job-A): %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	if !strings.Contains(v.Buffer(), "hello to A") {
		t.Fatalf("setup: job-A draft missing from view buffer: %q", v.Buffer())
	}

	// Flip the cursor to job-B and call afterCursorMove (what cursorDown
	// would do). The draft must be discarded before the next layout.
	gu.st.withLock(func() { gu.st.jobCursor = 1 })
	gu.afterCursorMove(panelJobs)

	var input, owner string
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if input != "" {
		t.Errorf("state.jobInput not cleared on cursor move; got %q", input)
	}
	if owner != "" {
		t.Errorf("state.jobInputOwner not cleared; got %q", owner)
	}

	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (cursor on job-B): %v", err)
	}
	v2, _ := gu.g.View(winJobInput)
	if v2 == nil {
		t.Fatalf("winJobInput missing for job-B")
	}
	if strings.Contains(v2.Buffer(), "hello to A") {
		t.Errorf("job-A draft leaked into job-B's input view: %q", v2.Buffer())
	}
}

// TestJobInput_SurvivesRefreshDrivenReshuffle pins the regression
// the latest review flagged: a refresh tick that REORDERS the
// jobs slice without an operator-authored cursor move (e.g. a
// newly-created job inserts at index 0 and pushes the previously-
// cursored row to index 1) must NOT silently wipe the operator's
// in-flight draft.
//
// The fix is to drop the clearJobInputIfStale call from
// applyRefreshLocked: only afterCursorMove (which fires on explicit
// j/k keypresses) clears the draft on jobID-mismatch. Refresh-
// driven reshuffles preserve it.
func TestJobInput_SurvivesRefreshDrivenReshuffle(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	// Seed: cursor on job-A at index 0, draft typed against job-A.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running"}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobInput = "plan: refactor X"
		gu.st.jobInputOwner = "job-A"
	})

	// Refresh-driven reshuffle: a brand-new job-Z lands at index 0,
	// pushing job-A to index 1. jobCursor stays pinned at 0, so the
	// selected job is now job-Z. The operator did NOT press j/k.
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running"}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running"}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	var (
		input string
		owner string
	)
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if input != "plan: refactor X" {
		t.Errorf("refresh-driven reshuffle wiped the in-flight draft: jobInput=%q (want %q)", input, "plan: refactor X")
	}
	if owner != "job-A" {
		t.Errorf("jobInputOwner clobbered on refresh: %q (want job-A)", owner)
	}

	// Sanity: a subsequent EXPLICIT cursor move to job-A (now at
	// index 1) still wins — the owner matches so the draft survives.
	gu.st.withLock(func() { gu.st.jobCursor = 1 })
	gu.afterCursorMove(panelJobs)
	gu.st.withRLock(func() {
		input = gu.st.jobInput
		owner = gu.st.jobInputOwner
	})
	if input != "plan: refactor X" {
		t.Errorf("draft lost after re-cursoring back to job-A: jobInput=%q", input)
	}
	if owner != "job-A" {
		t.Errorf("owner lost after re-cursoring back to job-A: %q", owner)
	}
}

// TestLayout_JobInputAppears_WhenSelectedJobRunning pins acceptance
// criterion 5: a running selected job allocates winJobInput, AND
// winDetail's height shrinks by the input's slot.
func TestLayout_JobInputAppears_WhenSelectedJobRunning(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	if _, err := gu.g.View(winJobInput); err != nil {
		t.Fatalf("winJobInput missing for running selected job: %v", err)
	}
	detail, err := gu.g.View(winDetail)
	if err != nil {
		t.Fatalf("winDetail missing: %v", err)
	}
	if h := detail.Height(); h < 4 {
		t.Errorf("winDetail too thin after winJobInput took its slot: h=%d", h)
	}
}

// TestLayout_JobInputDisappears_WhenSelectedJobTerminal pins the
// running→terminal teardown: a terminal selected job means
// winJobInput is deleted.
func TestLayout_JobInputDisappears_WhenSelectedJobTerminal(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (running): %v", err)
	}
	if _, err := gu.g.View(winJobInput); err != nil {
		t.Fatalf("setup: winJobInput should exist for running job: %v", err)
	}
	// Flip to terminal.
	gu.st.withLock(func() { gu.st.jobs[0].Status = "done" })
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout (terminal): %v", err)
	}
	if _, err := gu.g.View(winJobInput); err == nil {
		t.Errorf("winJobInput should be deleted for terminal job")
	}
}

// TestFocusPanel_DetailStashesPreviousFocus pins the focusPanel
// write side that TestRenderDetail_PanelDetailFocus_KeepsJobBody
// only exercises on the read side: pressing '0' from one of the
// side panels must record that panel into state.detailFocus so
// renderDetail can keep emitting the matching entity body.
//
// Without this stash, the panelDetail switch arm in renderDetail
// would fall through to "(nothing selected)" — the original
// blocking regression from the first review pass.
func TestFocusPanel_DetailStashesPreviousFocus(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	// Set focused = panelWorkflows so the outgoing focus is
	// distinct from the default panelTasks seed.
	gu.st.withLock(func() { gu.st.focused = panelWorkflows })
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("focusPanel(panelDetail): %v", err)
	}
	var focused, detailFocus panelID
	gu.st.withRLock(func() {
		focused = gu.st.focused
		detailFocus = gu.st.detailFocus
	})
	if focused != panelDetail {
		t.Errorf("focused = %v, want panelDetail", focused)
	}
	if detailFocus != panelWorkflows {
		t.Errorf("detailFocus = %v, want panelWorkflows (the outgoing focus)", detailFocus)
	}
}

// TestFocusPanel_DetailRepeatedPressPreservesStash pins the guard
// in focusPanel: pressing '0' twice in a row must NOT re-stash
// `panelDetail` into detailFocus (which would later make
// renderDetail render itself recursively and fall through to
// "(nothing selected)"). The first press records the source
// panel; the second press finds focused == panelDetail already
// and skips the stash.
func TestFocusPanel_DetailRepeatedPressPreservesStash(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	gu.st.withLock(func() { gu.st.focused = panelJobs })
	// First press: stashes panelJobs.
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("first focusPanel(panelDetail): %v", err)
	}
	// Second press: focused is already panelDetail; guard must
	// prevent overwriting detailFocus to panelDetail.
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("second focusPanel(panelDetail): %v", err)
	}
	var detailFocus panelID
	gu.st.withRLock(func() { detailFocus = gu.st.detailFocus })
	if detailFocus != panelJobs {
		t.Errorf("detailFocus = %v after two presses, want panelJobs (original source preserved)", detailFocus)
	}
}

// TestFocusPanel_DetailFromJobInputNormalises pins the
// panelJobInput → panelJobs normalisation in focusPanel's stash
// path. If the operator is in the input textarea and presses '0',
// detailFocus must hold panelJobs (not panelJobInput) so the next
// renderDetail call can find its switch arm. The renderer has its
// own normalisation as a defence-in-depth, but storing the model
// field already normalised keeps the persisted state sensible.
func TestFocusPanel_DetailFromJobInputNormalises(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	gu.st.withLock(func() { gu.st.focused = panelJobInput })
	if err := gu.focusPanel(panelDetail)(nil, nil); err != nil {
		t.Fatalf("focusPanel(panelDetail): %v", err)
	}
	var detailFocus panelID
	gu.st.withRLock(func() { detailFocus = gu.st.detailFocus })
	if detailFocus != panelJobs {
		t.Errorf("detailFocus = %v, want panelJobs (panelJobInput must normalise)", detailFocus)
	}
}

// TestJobInput_FocusPersistsAcrossLayout pins the regression
// the third-round review flagged: after jobsEnter on a running
// job sets the current view to winJobInput, the next layout
// pass MUST NOT yank focus back to winJobs (the previous
// focused panel).
//
// The fix is the synthetic panelJobInput focus identity:
// jobsEnter sets state.focused = panelJobInput, and layout's
// `g.SetCurrentView(focused.window())` then re-asserts the
// input view as current on every redraw — even at the 100ms
// spinner-tick cadence — instead of fighting it back.
func TestJobInput_FocusPersistsAcrossLayout(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	// Layout #1: create the views (current view defaults to
	// winJobs because state.focused == panelJobs from the seed).
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #1: %v", err)
	}
	if err := gu.jobsEnter(nil, nil); err != nil {
		t.Fatalf("jobsEnter: %v", err)
	}
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winJobInput {
		name := "<nil>"
		if cv != nil {
			name = cv.Name()
		}
		t.Fatalf("after jobsEnter, current view = %q, want %q", name, winJobInput)
	}
	// Also assert the model field: state.focused must reflect
	// the synthetic input panel, not panelJobs (otherwise the
	// next layout would override SetCurrentView back to winJobs).
	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobInput {
		t.Fatalf("state.focused = %v after jobsEnter, want panelJobInput", focused)
	}

	// Layout #2: simulates the next spinner tick / refresh tick
	// redrawing the dashboard. SetCurrentView is called inside
	// layout unconditionally; the input view MUST still be
	// current afterwards.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #2: %v", err)
	}
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winJobInput {
		name := "<nil>"
		if cv != nil {
			name = cv.Name()
		}
		t.Errorf("after layout #2, current view = %q, want %q (focus did not persist across redraw)", name, winJobInput)
	}

	// One more layout pass for good measure.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #3: %v", err)
	}
	if cv := gu.g.CurrentView(); cv == nil || cv.Name() != winJobInput {
		name := "<nil>"
		if cv != nil {
			name = cv.Name()
		}
		t.Errorf("after layout #3, current view = %q, want %q", name, winJobInput)
	}
}

// TestJobInput_EscRestoresJobsFocus pins the Esc handler contract:
// jobInputEscape (bound to Esc on winJobInput) returns state.focused
// to panelJobs so the next layout pass moves the caret back to
// the Jobs panel — mirroring the operator's mental model of
// "close the textarea, back to navigating jobs".
func TestJobInput_EscRestoresJobsFocus(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	seedRunningJob(gu, "job-run")
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	// Enter the input.
	if err := gu.jobsEnter(nil, nil); err != nil {
		t.Fatalf("jobsEnter: %v", err)
	}
	var focused panelID
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobInput {
		t.Fatalf("setup: state.focused = %v, want panelJobInput", focused)
	}
	// Esc.
	if err := gu.jobInputEscape(nil, nil); err != nil {
		t.Fatalf("jobInputEscape: %v", err)
	}
	gu.st.withRLock(func() { focused = gu.st.focused })
	if focused != panelJobs {
		t.Errorf("state.focused after Esc = %v, want panelJobs", focused)
	}
}

// dispatchFakeDS captures SendInput calls so
// TestLiveDispatch_AfterReshuffle_DispatchesToOwner can assert
// the dispatch target.
type dispatchFakeDS struct {
	refreshFakeDS
	calls   atomic.Int32
	lastJob atomic.Value // string
	lastMsg atomic.Value // string
}

func (f *dispatchFakeDS) SendInput(_ context.Context, jobID, msg, _ string) (string, error) {
	f.calls.Add(1)
	f.lastJob.Store(jobID)
	f.lastMsg.Store(msg)
	return "ok", nil
}

// TestLiveDispatch_AfterReshuffle_DispatchesToOwner pins the
// regression the third-round review flagged: a refresh-driven
// reshuffle of the jobs slice preserves the draft text (per the
// previous review's option-(a)), but the dispatch must still
// target the AUTHORED job — not whatever job ended up under
// the cursor after the reshuffle.
//
// Scenario:
//
//  1. Operator drafts "plan: refactor X" against job-A.
//  2. Refresh tick reshuffles the jobs slice so job-Z is at
//     index 0 and job-A at index 1; jobCursor stays at 0, so
//     the selected job is now job-Z.
//  3. Operator presses Ctrl-D (liveSend / liveDispatch).
//  4. SendInput must be called with jobID == "job-A" (the
//     authored target, recorded in jobInputOwner), NOT
//     "job-Z" (the post-reshuffle current cursor).
func TestLiveDispatch_AfterReshuffle_DispatchesToOwner(t *testing.T) {
	gu := jobDetailLayoutFixture(t)
	ds := &dispatchFakeDS{}
	gu.ds = ds

	// Seed: cursor on job-A, draft typed against job-A.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running"}},
		}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
		gu.st.jobInput = "plan: refactor X"
		gu.st.jobInputOwner = "job-A"
	})

	// Refresh-driven reshuffle: job-Z lands at index 0, job-A at index 1.
	// jobCursor pinned at 0 → selected job is now job-Z.
	r := refreshResult{
		jobs: []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-Z", Status: "running"}},
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running"}},
		},
		taskJobIdx: taskJobIndex{Active: map[string]bool{}, Any: map[string]bool{}},
	}
	gu.applyRefreshLocked(r)

	// Layout so winJobInput exists with the draft text.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout: %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("winJobInput missing: %v", err)
	}
	if !strings.Contains(v.Buffer(), "plan: refactor X") {
		t.Fatalf("setup: draft text not in view buffer: %q", v.Buffer())
	}

	// Press Ctrl-D → liveSend → liveDispatch. We invoke directly to
	// avoid the keybinding plumbing.
	if err := gu.liveSend(gu.g, v); err != nil {
		t.Fatalf("liveSend: %v", err)
	}

	// liveDispatch dispatches inside g.OnWorker; the headless gui
	// doesn't drain workers automatically. Poll for up to a second.
	deadline := time.Now().Add(1 * time.Second)
	for ds.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ds.calls.Load(); got != 1 {
		t.Fatalf("SendInput calls = %d, want 1", got)
	}
	if got, _ := ds.lastJob.Load().(string); got != "job-A" {
		t.Errorf("SendInput target jobID = %q, want %q (the authored target, NOT the post-reshuffle cursor)", got, "job-A")
	}
	if got, _ := ds.lastMsg.Load().(string); got != "plan: refactor X" {
		t.Errorf("SendInput msg = %q, want %q", got, "plan: refactor X")
	}
}

// TestLayout_EnterFocusesJobInput pins acceptance criterion 6:
// jobsEnter on a running job moves current view to winJobInput;
// on a terminal job it moves logical focus to panelDetail.
func TestLayout_EnterFocusesJobInput(t *testing.T) {
	t.Run("running_focuses_input", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		seedRunningJob(gu, "job-run")
		// First layout to create the views.
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		// Move the current view somewhere else first so we can
		// observe jobsEnter's SetCurrentView actually firing
		// (rather than passing trivially because winJobInput was
		// already current after the layout pass).
		if _, err := gu.g.SetCurrentView(winJobs); err != nil {
			t.Fatalf("setup: SetCurrentView winJobs: %v", err)
		}

		if err := gu.jobsEnter(nil, nil); err != nil {
			t.Fatalf("jobsEnter: %v", err)
		}
		// jobsEnter applies SetCurrentView synchronously now — the
		// previous g.Update wrap was vestigial. The current view
		// must already be winJobInput by the time jobsEnter returns.
		cv := gu.g.CurrentView()
		if cv == nil || cv.Name() != winJobInput {
			name := "<nil>"
			if cv != nil {
				name = cv.Name()
			}
			t.Errorf("current view after jobsEnter = %q, want %q", name, winJobInput)
		}
	})
	t.Run("terminal_focuses_detail", func(t *testing.T) {
		gu := jobDetailLayoutFixture(t)
		seedTerminalJob(gu, "job-done")
		if err := gu.layout(gu.g); err != nil {
			t.Fatalf("layout: %v", err)
		}
		if err := gu.jobsEnter(nil, nil); err != nil {
			t.Fatalf("jobsEnter: %v", err)
		}
		var focused panelID
		gu.st.withRLock(func() { focused = gu.st.focused })
		if focused != panelDetail {
			t.Errorf("focused = %v, want panelDetail", focused)
		}
	})
}
