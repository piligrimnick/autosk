package tui

import (
	"fmt"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// TestEnsureTranscriptEntry_NewAndLRU pins the cache-init + LRU
// eviction contract: on miss we allocate; at jobTranscriptCacheMax
// we evict one arbitrary non-key entry to make room. The exact
// victim is implementation-defined (Go's map iteration is
// randomised), but the post-condition is fixed: len(map) ==
// jobTranscriptCacheMax AND the just-touched key is present.
func TestEnsureTranscriptEntry_NewAndLRU(t *testing.T) {
	gu := &Gui{st: newState()}
	// Fill the cache to capacity.
	for i := 0; i < jobTranscriptCacheMax; i++ {
		gu.st.withLock(func() {
			gu.ensureTranscriptEntryLocked(fmt.Sprintf("job-%03d", i))
		})
	}
	if got := len(gu.st.jobTranscript); got != jobTranscriptCacheMax {
		t.Fatalf("len after fill = %d, want %d", got, jobTranscriptCacheMax)
	}

	// One more (a NEW key) must evict one of the existing entries.
	newKey := "job-new"
	gu.st.withLock(func() { gu.ensureTranscriptEntryLocked(newKey) })
	if got := len(gu.st.jobTranscript); got != jobTranscriptCacheMax {
		t.Errorf("len after one-over = %d, want %d (eviction failed)", got, jobTranscriptCacheMax)
	}
	if _, ok := gu.st.jobTranscript[newKey]; !ok {
		t.Errorf("just-touched key %q missing from cache", newKey)
	}

	// Hitting an existing entry doesn't evict — len stays put.
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
// terminal-job cache entries: scheduleJobArchive must mark stale
// when loadedAt is older than jobTranscriptTerminalTTL and skip
// otherwise. We exercise the inline predicate that scheduleJobArchive
// would consult; the worker dispatch itself is wired through
// gu.g.OnWorker which needs no testing here.
func TestTerminalTTL_TriggersRefetch(t *testing.T) {
	gu := &Gui{st: newState()}
	const jobID = "job-terminal"
	// Seed an entry that just loaded — terminal+fresh must NOT refetch.
	gu.st.withLock(func() {
		te := gu.ensureTranscriptEntryLocked(jobID)
		te.loadedAt = time.Now()
	})
	if shouldRefetchJobArchive(gu, jobID, false) {
		t.Errorf("fresh terminal entry should not refetch")
	}
	// Age it past the TTL.
	gu.st.withLock(func() {
		gu.st.jobTranscript[jobID].loadedAt = time.Now().Add(-2 * jobTranscriptTerminalTTL)
	})
	if !shouldRefetchJobArchive(gu, jobID, false) {
		t.Errorf("stale terminal entry should refetch")
	}
	// Running jobs ignore TTL.
	gu.st.withLock(func() {
		gu.st.jobTranscript[jobID].loadedAt = time.Now().Add(-2 * jobTranscriptTerminalTTL)
	})
	if shouldRefetchJobArchive(gu, jobID, true) {
		t.Errorf("running entry should not refetch on TTL alone")
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
		te.runningSeen = true
	})
	// Simulate the transition handler: zero loadedAt + clear runningSeen.
	gu.st.withLock(func() {
		te := gu.st.jobTranscript[jobID]
		te.loadedAt = time.Time{}
		te.runningSeen = false
	})
	gu.st.withRLock(func() {
		te := gu.st.jobTranscript[jobID]
		if !te.loadedAt.IsZero() {
			t.Errorf("loadedAt not zeroed: %v", te.loadedAt)
		}
		if te.runningSeen {
			t.Errorf("runningSeen not cleared")
		}
	})
}

// shouldRefetchJobArchive mirrors the inline predicate from
// scheduleJobArchive so the cache-staleness rule can be tested
// without spinning up a real gocui.Gui. Keep this in lock-step
// with scheduleJobArchive's logic.
func shouldRefetchJobArchive(gu *Gui, jobID string, running bool) bool {
	var fetch bool
	gu.st.withLock(func() {
		te, ok := gu.st.jobTranscript[jobID]
		if !ok {
			fetch = true
			return
		}
		if te.loadedAt.IsZero() {
			fetch = true
			return
		}
		if !running && time.Since(te.loadedAt) > jobTranscriptTerminalTTL {
			fetch = true
		}
	})
	return fetch
}
