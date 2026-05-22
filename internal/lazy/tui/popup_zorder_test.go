package tui

import (
	"testing"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
)

// indexOf returns i where g.Views()[i].Name() == name, or -1 if
// the view isn't currently present. The popup-z-order assertion
// reads this on both candidates and compares the resulting
// indices.
func viewIndexInGui(gu *Gui, name string) int {
	for i, v := range gu.g.Views() {
		if v.Name() == name {
			return i
		}
	}
	return -1
}

// TestZOrder_JobInputCreatedDuringPopup_PopupStaysOnTop pins the
// SUBSTANTIVE regression review flagged: when winJobInput is
// created mid-popup (e.g. a queued job promotes to running while
// the operator has the palette open), the input view would end
// up appended AFTER the popup view in g.views — gocui draws in
// insertion order so the input would silently paint on top of
// the popup chrome.
//
// The fix is layoutPopup's pinOnTop / SetViewOnTop call: after
// drawing each active popup view, move it to the end of g.views
// so later-created dashboard views can't paint over it.
func TestZOrder_JobInputCreatedDuringPopup_PopupStaysOnTop(t *testing.T) {
	gu := newHeadlessGui(t, 120, 40)
	gu.ds = &refreshFakeDS{}

	// Step 1: seed a TERMINAL job (no input overlay yet) and
	// drive a layout pass to populate g.views with the dashboard
	// views.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{
			JobResponse: api.JobResponse{JobID: "job-A", Status: "done"},
		}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #1 (no overlay): %v", err)
	}
	if viewIndexInGui(gu, winJobInput) != -1 {
		t.Fatalf("setup: winJobInput should not exist before the promotion")
	}

	// Step 2: open a popup (palette / menu). Layout pass creates
	// winPopupMenu.
	gu.openMenu("p", []string{"a", "b"}, func(int) error { return nil })
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #2 (popup open): %v", err)
	}
	popupIdx := viewIndexInGui(gu, winPopupMenu)
	if popupIdx == -1 {
		t.Fatalf("setup: popup view should exist after openMenu+layout")
	}

	// Step 3: flip the job to non-terminal (e.g. it was queued
	// and just started running). The next layout pass creates
	// winJobInput because isJobLive now returns true.
	gu.st.withLock(func() {
		gu.st.jobs[0].Status = "running"
		gu.st.jobs[0].Streaming = true
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout #3 (overlay appears mid-popup): %v", err)
	}
	inputIdx := viewIndexInGui(gu, winJobInput)
	popupIdx = viewIndexInGui(gu, winPopupMenu)
	if inputIdx == -1 {
		t.Fatalf("winJobInput should exist after the job promotion")
	}
	if popupIdx == -1 {
		t.Fatalf("popup should still exist after layout #3")
	}
	// The crucial invariant: the popup must paint LAST (i.e. its
	// index in g.views must be greater than the input's), so it
	// draws ON TOP of the dashboard overlay.
	if popupIdx <= inputIdx {
		t.Errorf("popup draw order broken: popup index=%d, input index=%d (popup must be later in g.views so it paints last)",
			popupIdx, inputIdx)
	}
}
