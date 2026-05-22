package tui

import (
	"context"
	"strings"
	"testing"

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
