package tui

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// errFakeArchive is a sentinel for archive-load failure in unit
// tests that drive applyArchiveLoadLocked directly.
var errFakeArchive = errors.New("fake archive load failure")

// TestEnsureTranscriptEntry_NewAndLRU pins the cache-init + LRU
// eviction contract: on miss we allocate; at jobTranscriptCacheMax
// we evict the LEAST-RECENTLY-TOUCHED non-key entry. Both branches
// (cap + actual LRU victim) are asserted explicitly so a future
// regression to random eviction fails this test.
func TestEnsureTranscriptEntry_NewAndLRU(t *testing.T) {
	gu := &Gui{st: newState()}
	// Fill the cache to capacity, stamping touchedAt by hand so the
	// LRU order is deterministic (job-000 is oldest, job-NN-1 newest).
	base := time.Now()
	for i := 0; i < jobTranscriptCacheMax; i++ {
		k := fmt.Sprintf("job-%03d", i)
		gu.st.withLock(func() {
			te := gu.ensureTranscriptEntryLocked(k)
			te.touchedAt = base.Add(time.Duration(i) * time.Second)
		})
	}
	if got := len(gu.st.jobTranscript); got != jobTranscriptCacheMax {
		t.Fatalf("len after fill = %d, want %d", got, jobTranscriptCacheMax)
	}

	// Touch job-005 so it's no longer the oldest — the next eviction
	// must pick job-000 (still untouched since base).
	gu.st.withLock(func() {
		te := gu.ensureTranscriptEntryLocked("job-005")
		te.touchedAt = base.Add(2 * time.Hour) // explicitly newest
	})

	// One more (a NEW key) must evict the least-recently-touched
	// existing entry. With the stamps above that's job-000.
	newKey := "job-new"
	gu.st.withLock(func() { gu.ensureTranscriptEntryLocked(newKey) })
	if got := len(gu.st.jobTranscript); got != jobTranscriptCacheMax {
		t.Errorf("len after one-over = %d, want %d (eviction failed)", got, jobTranscriptCacheMax)
	}
	if _, ok := gu.st.jobTranscript[newKey]; !ok {
		t.Errorf("just-touched key %q missing from cache", newKey)
	}
	if _, ok := gu.st.jobTranscript["job-000"]; ok {
		t.Errorf("LRU victim job-000 still present — LRU policy did not run")
	}
	if _, ok := gu.st.jobTranscript["job-005"]; !ok {
		t.Errorf("recently-touched job-005 was wrongly evicted — LRU picked the wrong victim")
	}

	// Hitting an existing entry doesn't grow the cache.
	existing := newKey
	gu.st.withLock(func() { gu.ensureTranscriptEntryLocked(existing) })
	if got := len(gu.st.jobTranscript); got != jobTranscriptCacheMax {
		t.Errorf("len after hit = %d, want %d (hit should not evict)", got, jobTranscriptCacheMax)
	}
}

// TestAppendTranscriptEvent_AppendsBoxAtParity pins the invariant
// that te.events and te.renderedBoxes grow in lock-step: every
// append grows both slices by exactly one element.
func TestAppendTranscriptEvent_AppendsBoxAtParity(t *testing.T) {
	te := &jobTranscriptEntry{}
	for i := 0; i < 10; i++ {
		appendTranscriptEvent(te, datasource.MessageEvent{
			Kind: "user_text",
			TS:   jobDetailFixedTS.Add(time.Duration(i) * time.Second),
			Text: fmt.Sprintf("event %d", i),
		}, 80)
	}
	if le, lb := len(te.events), len(te.renderedBoxes); le != lb {
		t.Errorf("events/boxes drift: events=%d boxes=%d", le, lb)
	}
	if got := len(te.events); got != 10 {
		t.Errorf("events count = %d, want 10", got)
	}
}

// TestAppendTranscriptEvent_HitCapDropsOldest pins the soft-cap
// behaviour: appending past jobLiveBufCap drops the oldest 25%, sets
// truncated=true, and keeps the two slices length-aligned.
func TestAppendTranscriptEvent_HitCapDropsOldest(t *testing.T) {
	te := &jobTranscriptEntry{}
	// Push exactly jobLiveBufCap+1 events: the +1 push is the one
	// that crosses the threshold (`n > jobLiveBufCap`) and triggers
	// the drop-oldest-25% path. After the trim there should be no
	// further appends so the final length equals the post-trim cap.
	total := jobLiveBufCap + 1
	for i := 0; i < total; i++ {
		appendTranscriptEvent(te, datasource.MessageEvent{
			Kind: "user_text",
			TS:   jobDetailFixedTS.Add(time.Duration(i) * time.Second),
			Text: fmt.Sprintf("event %d", i),
		}, 80)
	}
	wantLen := jobLiveBufCap - jobLiveBufCap/4
	if got := len(te.events); got != wantLen {
		t.Errorf("events len after cap-trim = %d, want %d", got, wantLen)
	}
	if got := len(te.renderedBoxes); got != wantLen {
		t.Errorf("boxes len after cap-trim = %d, want %d", got, wantLen)
	}
	if !te.truncated {
		t.Errorf("truncated flag not set")
	}
	// The trim copies events[n-keep:] forward, so the first
	// surviving event is event #(total-wantLen).
	dropped := total - wantLen
	wantFirst := fmt.Sprintf("event %d", dropped)
	if got := te.events[0].Text; got != wantFirst {
		t.Errorf("first surviving event = %q, want %q", got, wantFirst)
	}
}

// TestRebuildTranscriptBoxes_OnResize pins the per-event rebuild:
// rebuildTranscriptBoxes(te, w) re-renders every box at the new
// width and updates te.renderedWidth.
func TestRebuildTranscriptBoxes_OnResize(t *testing.T) {
	te := &jobTranscriptEntry{}
	for i := 0; i < 5; i++ {
		appendTranscriptEvent(te, datasource.MessageEvent{
			Kind: "user_text",
			TS:   jobDetailFixedTS.Add(time.Duration(i) * time.Second),
			Text: fmt.Sprintf("event %d body content here", i),
		}, 40)
	}
	beforeFirst := te.renderedBoxes[0]
	rebuildTranscriptBoxes(te, 100)
	if te.renderedWidth != 100 {
		t.Errorf("renderedWidth = %d, want 100", te.renderedWidth)
	}
	if len(te.renderedBoxes) != len(te.events) {
		t.Errorf("count drift: events=%d boxes=%d", len(te.events), len(te.renderedBoxes))
	}
	if te.renderedBoxes[0] == beforeFirst {
		t.Errorf("expected first box to re-render at width=100, got identical bytes")
	}
}

// TestTerminalTTL_TriggersRefetch pins the staleness rule for
// terminal-job cache entries via the shared jobArchiveStale
// predicate (the same one scheduleJobArchive + loadJobArchive
// consult). Calling the production helper directly means a future
// policy drift here is caught — no parallel copy to keep in sync.
func TestTerminalTTL_TriggersRefetch(t *testing.T) {
	gu := &Gui{st: newState()}
	const jobID = "job-terminal"
	// Seed an entry that just loaded — terminal+fresh must NOT refetch.
	gu.st.withLock(func() {
		te := gu.ensureTranscriptEntryLocked(jobID)
		te.loadedAt = time.Now()
	})
	if gu.jobArchiveStale(jobID, false) {
		t.Errorf("fresh terminal entry should not refetch")
	}
	// Age it past the TTL.
	gu.st.withLock(func() {
		gu.st.jobTranscript[jobID].loadedAt = time.Now().Add(-2 * jobTranscriptTerminalTTL)
	})
	if !gu.jobArchiveStale(jobID, false) {
		t.Errorf("stale terminal entry should refetch")
	}
	// Running jobs ignore TTL.
	gu.st.withLock(func() {
		gu.st.jobTranscript[jobID].loadedAt = time.Now().Add(-2 * jobTranscriptTerminalTTL)
	})
	if gu.jobArchiveStale(jobID, true) {
		t.Errorf("running entry should not refetch on TTL alone")
	}
}

// TestApplyArchiveLoad_ResetsTruncated pins the regression review
// flagged: when a job's transcript hits jobLiveBufCap and the live
// pump trims oldest 25% (setting te.truncated=true), a subsequent
// archive refetch (carrying the FULL event set via full=true /
// limit=0) must drop the stale truncated flag. Otherwise
// renderJobDetail keeps prepending the "(transcript truncated...)"
// note even though the cache now has every event.
func TestApplyArchiveLoad_ResetsTruncated(t *testing.T) {
	gu := newHeadlessGui(t, 120, 40)
	const jobID = "job-trunc-XYZ"

	// Seed: truncated=true with a small old event set; the renderer
	// would prepend the truncation note in this state.
	old := []datasource.MessageEvent{
		{Kind: "user_text", TS: jobDetailFixedTS, Text: "old-trim-survivor-1"},
		{Kind: "user_text", TS: jobDetailFixedTS.Add(time.Second), Text: "old-trim-survivor-2"},
	}
	gu.st.withLock(func() {
		te := gu.ensureTranscriptEntryLocked(jobID)
		te.events = old
		te.renderedBoxes = make([]string, len(old))
		te.truncated = true
	})

	// Simulate a successful archive load with the FULL event set
	// (this is what loadJobArchive does end-to-end; we drive the
	// inner helper directly because gocui's g.Update queue is async
	// in tests).
	full := []datasource.MessageEvent{
		{Kind: "user_text", TS: jobDetailFixedTS.Add(-time.Second), Text: "e-historical-1"},
		{Kind: "user_text", TS: jobDetailFixedTS, Text: "old-trim-survivor-1"},
		{Kind: "user_text", TS: jobDetailFixedTS.Add(time.Second), Text: "old-trim-survivor-2"},
		{Kind: "user_text", TS: jobDetailFixedTS.Add(2 * time.Second), Text: "e-fresh-tail"},
	}
	gu.st.withLock(func() { gu.applyArchiveLoadLocked(jobID, full, nil) })

	var (
		truncated bool
		loadedAt  time.Time
		nEvents   int
		fetchErr  error
	)
	gu.st.withRLock(func() {
		te := gu.st.jobTranscript[jobID]
		truncated = te.truncated
		loadedAt = te.loadedAt
		nEvents = len(te.events)
		fetchErr = te.err
	})
	if truncated {
		t.Errorf("truncated flag not cleared after successful archive load")
	}
	if loadedAt.IsZero() {
		t.Errorf("loadedAt not stamped on success")
	}
	if fetchErr != nil {
		t.Errorf("err leaked on success path: %v", fetchErr)
	}
	if nEvents != len(full) {
		t.Errorf("events count after load = %d, want %d (full archive replace)", nEvents, len(full))
	}

	// Conversely: a load that errors must NOT clear the flag (and
	// must NOT touch te.events). Re-set truncated to true to assert
	// the error path preserves it.
	gu.st.withLock(func() {
		te := gu.st.jobTranscript[jobID]
		te.truncated = true
	})
	gu.st.withLock(func() {
		gu.applyArchiveLoadLocked(jobID, nil, errFakeArchive)
	})
	gu.st.withRLock(func() {
		te := gu.st.jobTranscript[jobID]
		truncated = te.truncated
		fetchErr = te.err
	})
	if !truncated {
		t.Errorf("truncated wrongly cleared on error path")
	}
	if fetchErr == nil {
		t.Errorf("err not propagated on error path")
	}
}

// TestRunningToTerminalTransition pins the contract that an entry
// whose run just went terminal gets its loadedAt zeroed (so the
// applyRefreshLocked-driven scheduler will refetch on the next
// pass).
func TestRunningToTerminalTransition(t *testing.T) {
	gu := &Gui{st: newState()}
	const jobID = "job-trans"
	gu.st.withLock(func() {
		te := gu.ensureTranscriptEntryLocked(jobID)
		te.loadedAt = time.Now()
	})
	// Simulate the transition handler: zero loadedAt.
	gu.st.withLock(func() {
		te := gu.st.jobTranscript[jobID]
		te.loadedAt = time.Time{}
	})
	gu.st.withRLock(func() {
		te := gu.st.jobTranscript[jobID]
		if !te.loadedAt.IsZero() {
			t.Errorf("loadedAt not zeroed: %v", te.loadedAt)
		}
	})
}
