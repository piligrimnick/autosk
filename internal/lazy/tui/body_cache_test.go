package tui

import (
	"strings"
	"testing"

	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
)

// newHeadlessGui wires up a Headless gocui.Gui big enough to host the
// dashboard layout (matches what other popup_*_test.go files do) and
// returns it together with a minimal Gui shell. Callers can call
// gu.layout(g) directly to exercise the manager pass without going
// through MainLoop.
func newHeadlessGui(t *testing.T, w, h int) *Gui {
	t.Helper()
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputNormal,
		Headless:   true,
		Width:      w,
		Height:     h,
	})
	if err != nil {
		t.Fatalf("gocui new: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return &Gui{g: g, st: newState()}
}

// TestWriteView_Cache_SkipsRedundantWrite pins the writeView
// short-circuit: a second call with identical (name, title, body)
// and unchanged view dimensions must NOT touch the view's buffer
// again. We prove the skip by mutating the buffer between the two
// writeView calls — if the second call had run v.Clear()+v.Write()
// the buffer would have been restored to `body`; because it's a
// no-op the mutated content stays visible.
func TestWriteView_Cache_SkipsRedundantWrite(t *testing.T) {
	gu := newHeadlessGui(t, 80, 24)
	// Manually create a view directly (bypassing the full layout
	// machinery so the test stays focused on writeView semantics).
	v, err := gu.g.SetView("probe", 0, 0, 40, 5, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}
	if v == nil {
		t.Fatal("SetView returned nil view")
	}

	const body = "first body"
	gu.writeView("probe", "Probe", body)
	if !strings.Contains(v.Buffer(), body) {
		t.Fatalf("first writeView did not populate buffer: %q", v.Buffer())
	}

	// Mutate the buffer to a sentinel that writeView would overwrite
	// IF it took the fast path.
	v.Clear()
	const sentinel = "MUTATED-XYZ"
	if _, err := v.Write([]byte(sentinel)); err != nil {
		t.Fatalf("v.Write sentinel: %v", err)
	}

	// Second writeView with identical body must short-circuit, so
	// the sentinel survives.
	gu.writeView("probe", "Probe", body)
	if !strings.Contains(v.Buffer(), sentinel) {
		t.Errorf("writeView did not short-circuit on identical body; sentinel was overwritten. buffer=%q", v.Buffer())
	}
	if strings.Contains(v.Buffer(), body) {
		t.Errorf("writeView appears to have rewritten the body despite no changes: %q", v.Buffer())
	}
}

// TestWriteView_Cache_RewritesOnBodyChange pins the inverse half of
// the short-circuit contract: when the body string differs from the
// cached entry, writeView must run v.Clear()+v.Write() so the new
// content lands.
func TestWriteView_Cache_RewritesOnBodyChange(t *testing.T) {
	gu := newHeadlessGui(t, 80, 24)
	v, err := gu.g.SetView("probe", 0, 0, 40, 5, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}
	if v == nil {
		t.Fatal("SetView returned nil view")
	}

	gu.writeView("probe", "Probe", "alpha")
	if !strings.Contains(v.Buffer(), "alpha") {
		t.Fatalf("first writeView did not write alpha: %q", v.Buffer())
	}

	gu.writeView("probe", "Probe", "beta")
	if !strings.Contains(v.Buffer(), "beta") {
		t.Errorf("writeView did not pick up body change; buffer=%q", v.Buffer())
	}
	if strings.Contains(v.Buffer(), "alpha") {
		t.Errorf("stale body leaked through body cache: %q", v.Buffer())
	}
}

// TestWriteView_Cache_InvalidatedOnRecreate pins acceptance criterion
// 6: after DeleteView + SetView for the same name, the next
// writeView with the same body string must still actually write,
// because the freshly-recreated view has an empty internal buffer.
//
// We model the layout-driven invalidation by calling
// gu.invalidateBodyCache(name) between DeleteView and the second
// writeView — that mirrors what layout.go does in production.
func TestWriteView_Cache_InvalidatedOnRecreate(t *testing.T) {
	gu := newHeadlessGui(t, 80, 24)
	if _, err := gu.g.SetView("probe", 0, 0, 40, 5, 0); err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}

	const body = "shared body"
	gu.writeView("probe", "Probe", body)

	// Tear the view down and recreate it. The layout's DeleteView
	// loop drops the cache entry; mirror that here.
	if err := gu.g.DeleteView("probe"); err != nil {
		t.Fatalf("DeleteView: %v", err)
	}
	gu.invalidateBodyCache("probe")

	v2, err := gu.g.SetView("probe", 0, 0, 40, 5, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView recreate: %v", err)
	}
	if v2 == nil {
		t.Fatal("SetView recreate returned nil view")
	}
	if v2.Buffer() != "" {
		t.Fatalf("recreated view should be empty; got %q", v2.Buffer())
	}

	gu.writeView("probe", "Probe", body)
	if !strings.Contains(v2.Buffer(), body) {
		t.Errorf("writeView after recreate did not rewrite body; buffer=%q (cache invalidation missing)", v2.Buffer())
	}
}

// TestWriteView_Cache_InvalidatedOnResize pins acceptance criterion
// 7: a SetView call with new coordinates triggers a rewrite even
// when the body string is byte-identical. gocui's SetView calls
// clearViewLines() internally on resize, so the body must be
// re-written or the pane goes blank.
func TestWriteView_Cache_InvalidatedOnResize(t *testing.T) {
	gu := newHeadlessGui(t, 80, 24)
	if _, err := gu.g.SetView("probe", 0, 0, 40, 5, 0); err != nil && !isUnknownView(err) {
		t.Fatalf("SetView: %v", err)
	}

	const body = "resize-invariant body"
	gu.writeView("probe", "Probe", body)

	// Resize the view. SetView clears the lines buffer internally.
	v, err := gu.g.SetView("probe", 0, 0, 50, 6, 0)
	if err != nil && !isUnknownView(err) {
		t.Fatalf("SetView resize: %v", err)
	}
	if v == nil {
		t.Fatal("SetView resize returned nil view")
	}

	// Drop a sentinel to detect that writeView actually re-wrote the
	// view (the body-match check by itself isn't enough because
	// SetView could leave the buffer intact under some
	// configurations — what we really care about is that writeView
	// noticed the dimension change and took the slow path).
	v.Clear()
	if _, err := v.Write([]byte("SENTINEL")); err != nil {
		t.Fatalf("v.Write sentinel: %v", err)
	}

	gu.writeView("probe", "Probe", body)
	if !strings.Contains(v.Buffer(), body) {
		t.Errorf("writeView did not rewrite body after resize; buffer=%q (sentinel still in place means dim-change was not noticed)", v.Buffer())
	}
	if strings.Contains(v.Buffer(), "SENTINEL") {
		t.Errorf("post-resize writeView left the sentinel in place; expected v.Clear()+v.Write() path. buffer=%q", v.Buffer())
	}
}

// TestWriteView_Cache_InvalidatedByLayoutDeleteView is the
// integration-style counterpart to the recreate test: drive the real
// layout function through a transition that adds + removes a view
// (winJobInput, which only exists when the selected job is running)
// and verify writeView still works on the panel that was torn down.
// The bug we're guarding against is the regression where layout
// drops the view but forgets to drop the cache entry — in which case
// the writeView at the end of the next frame short-circuits against
// the stale cache and the pane stays blank.
func TestWriteView_Cache_InvalidatedByLayoutDeleteView(t *testing.T) {
	gu := newHeadlessGui(t, 120, 40)
	gu.ds = &refreshFakeDS{}

	// Step 1: dashboard with no running job — winJobInput must NOT
	// be allocated.
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout dashboard #1: %v", err)
	}
	if _, err := gu.g.View(winJobInput); err == nil {
		t.Fatalf("layout without running job should not create winJobInput")
	}

	// Step 2: seed a running job and re-layout — winJobInput appears.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{JobResponse: api.JobResponse{JobID: "j-1", Status: "running", Streaming: true}}}
		gu.st.jobCursor = 0
		gu.st.focused = panelJobs
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout dashboard #2: %v", err)
	}
	if _, err := gu.g.View(winJobInput); err != nil {
		t.Fatalf("layout should have created winJobInput for running job: %v", err)
	}

	// Step 3: flip job to terminal — winJobInput must be deleted on
	// the next layout pass AND the body cache for that view must be
	// invalidated so a future re-creation doesn't get short-circuited.
	// The input view is gated on Streaming==true; flip both Status
	// and Streaming to simulate the running→done transition.
	gu.st.withLock(func() {
		gu.st.jobs[0].Status = "done"
		gu.st.jobs[0].Streaming = false
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout dashboard #3: %v", err)
	}
	if _, err := gu.g.View(winJobInput); err == nil {
		t.Fatalf("layout should have deleted winJobInput for terminal job")
	}

	// Step 4: flip back to running — winJobInput must come back
	// populated (not stuck blank from a stale body-cache entry).
	gu.st.withLock(func() {
		gu.st.jobs[0].Status = "running"
		gu.st.jobs[0].Streaming = true
	})
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout dashboard #4: %v", err)
	}
	v, err := gu.g.View(winJobInput)
	if err != nil {
		t.Fatalf("layout dashboard #4 should have re-created winJobInput: %v", err)
	}
	// jobInput buffer can be empty (no typed text yet); the assertion
	// is that writeView wasn't skipped due to stale cache. We force
	// a non-empty buffer by seeding st.jobInput and laying out once
	// more.
	gu.st.withLock(func() { gu.st.jobInput = "typed text" })
	if err := gu.layout(gu.g); err != nil {
		t.Fatalf("layout dashboard #5: %v", err)
	}
	if !strings.Contains(v.Buffer(), "typed text") {
		t.Errorf("winJobInput body missing after re-creation: %q", v.Buffer())
	}
}
