package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"autosk/internal/lazy/datasource"
)

// TestRenderJobDetail_UsesCachedJoinedBody pins acceptance
// criterion 1 from ask-beab99 as a STRUCTURAL test (not a timing
// assertion): when the transcript is stable (joinedDirty==false,
// renderedWidth matches the pane width, len(renderedBoxes) matches
// len(events)), renderJobDetail must emit te.joinedBody VERBATIM
// instead of re-joining te.renderedBoxes on every call.
//
// We inject a unique sentinel into te.joinedBody and DIFFERENT
// contents into te.renderedBoxes. If the renderer takes the
// supposedly-deleted per-event loop path, the sentinel won't
// appear in the output (and the unrelated box content will);
// the cached path emits the sentinel. The asymmetry is the
// invariant — a future regression to the per-event loop fails
// this test deterministically, independent of CPU speed / CI
// hardware / b.N tuning.
func TestRenderJobDetail_UsesCachedJoinedBody(t *testing.T) {
	j := makeRunningJob("job-cached")
	const sentinel = "SENTINEL-CACHED-JOINED-BODY-XYZ"
	const boxSentinel = "BOXES-LOOP-SHOULD-NOT-FIRE-ABC"
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{{
			Kind: "user_text",
			TS:   jobDetailFixedTS,
			Text: "noop",
		}},
		renderedBoxes: []string{boxSentinel},
		// joinedBody intentionally differs from the rendered boxes
		// to detect a regression to the per-event loop.
		joinedBody:    "\n" + sentinel + "\n",
		joinedDirty:   false,
		renderedWidth: 96, // matches contentW = width(100) - 4
		loadedAt:      time.Now(),
	}
	out := renderJobDetail(j, te, 100)
	if !strings.Contains(out, sentinel) {
		t.Errorf("renderJobDetail did not emit the cached joinedBody sentinel — per-event loop must have fired. output=%q", out)
	}
	if strings.Contains(out, boxSentinel) {
		t.Errorf("renderJobDetail emitted a box from renderedBoxes — the per-event-loop path is still live. output=%q", out)
	}
}

// TestRenderJobDetail_RebuildsOnDirty pins the inverse of the
// cached-emit contract: when joinedDirty is true,
// renderJobDetail must rebuild joinedBody from renderedBoxes
// (clearing the dirty flag in the process) and emit the freshly
// rebuilt content. Without this path, an SSE-pump-driven append
// would never become visible to the user — the renderer would
// keep emitting the pre-append joinedBody.
func TestRenderJobDetail_RebuildsOnDirty(t *testing.T) {
	j := makeRunningJob("job-dirty")
	const newBoxText = "NEWLY-APPENDED-BOX-MARK-QED"
	te := &jobTranscriptEntry{
		events: []datasource.MessageEvent{{
			Kind: "user_text",
			TS:   jobDetailFixedTS,
			Text: "noop",
		}},
		renderedBoxes: []string{newBoxText},
		joinedBody:    "STALE-PRE-APPEND-CONTENT", // intentionally stale
		joinedDirty:   true,
		renderedWidth: 96,
		loadedAt:      time.Now(),
	}
	out := renderJobDetail(j, te, 100)
	if !strings.Contains(out, newBoxText) {
		t.Errorf("dirty rebuild did not pick up the new box content. output=%q", out)
	}
	if strings.Contains(out, "STALE-PRE-APPEND-CONTENT") {
		t.Errorf("renderJobDetail emitted the stale joinedBody instead of rebuilding. output=%q", out)
	}
	// The dirty flag must be cleared so subsequent frames take the
	// cached path.
	if te.joinedDirty {
		t.Errorf("joinedDirty still true after rebuild — next frame will rebuild again, defeating the cache")
	}
}

// TestAppendTranscriptEvent_NoLinearGrowth pins acceptance
// criterion 2 as a STRUCTURAL test: appendTranscriptEvent must not
// regrow joinedBody on each append (the regression would be an
// O(N²) "joinedBody = joinedBody + boxstr" pattern). Append a few
// thousand events and verify that joinedBody is empty (never
// rebuilt by the append path) and joinedDirty is true (deferred
// rebuild owned by the renderer). Together these prove the per-
// append cost is independent of the session length.
func TestAppendTranscriptEvent_NoLinearGrowth(t *testing.T) {
	te := &jobTranscriptEntry{}
	const n = 2000
	contentW := 96
	for i := 0; i < n; i++ {
		appendTranscriptEvent(te, datasource.MessageEvent{
			Kind: "user_text",
			TS:   jobDetailFixedTS.Add(time.Duration(i) * time.Second),
			Text: "lorem ipsum dolor sit amet consectetur adipiscing elit",
		}, contentW)
	}
	if te.joinedBody != "" {
		t.Errorf("appendTranscriptEvent rebuilt joinedBody during append (got %d bytes); the per-append path must defer the rebuild to the renderer", len(te.joinedBody))
	}
	if !te.joinedDirty {
		t.Errorf("joinedDirty not set after append; renderer won't know to rebuild on next frame")
	}
	if got, want := len(te.events), n; got != want {
		t.Errorf("events len = %d, want %d", got, want)
	}
	if got := len(te.renderedBoxes); got != n {
		t.Errorf("renderedBoxes len = %d, want %d", got, n)
	}
}

// makeTranscriptFixture produces a jobTranscriptEntry with n
// transcript events, all already rendered into per-event boxes at
// contentW. Used by the renderJobDetail / appendTranscriptEvent
// micro-benchmarks pinned by ask-beab99.
//
// The event payload is intentionally ~500 bytes per box — that
// matches the typical assistant_text / user_text shape observed
// in a live pi session, where the dominant per-frame allocation
// hit identified in the original investigation was the string
// concatenation in renderJobDetail's box-join loop. Re-running
// these benchmarks on a long-session-sized fixture demonstrates
// the O(N)-to-O(1) win the joinedBody cache delivers.
func makeTranscriptFixture(n, contentW int) *jobTranscriptEntry {
	te := &jobTranscriptEntry{}
	payload := ""
	for i := 0; i < 12; i++ {
		// ~40 chars × 12 = ~480 chars of payload before box framing.
		payload += "lorem ipsum dolor sit amet ev "
	}
	for i := 0; i < n; i++ {
		ev := datasource.MessageEvent{
			Kind: "user_text",
			TS:   jobDetailFixedTS.Add(time.Duration(i) * time.Second),
			Text: fmt.Sprintf("event %d %s", i, payload),
		}
		appendTranscriptEvent(te, ev, contentW)
	}
	// Force the joined-body cache to be primed so the benchmark
	// measures the steady-state read path, not the first-frame
	// rebuild.
	rebuildTranscriptBoxes(te, contentW)
	return te
}

// BenchmarkRenderJobDetail_StableTranscript pins acceptance
// criterion 1 from ask-beab99: with the per-event-loop replaced by
// the cached joinedBody, a render call against a stable transcript
// (no new events, no resize) must stay ≤ ~50µs even at 5000 events.
//
// Reports per-call ns/op + allocs/op. The CI threshold isn't
// enforced here (different hardware) — the regression guard is the
// benchmark's existence + the order-of-magnitude readout in
// `go test -bench` output.
func BenchmarkRenderJobDetail_StableTranscript(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			j := makeRunningJob("job-bench")
			te := makeTranscriptFixture(n, 96)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = renderJobDetail(j, te, 100)
			}
		})
	}
}

// BenchmarkAppendTranscriptEvent_5000 pins acceptance criterion 2:
// the per-call cost of appendTranscriptEvent over a session of
// 5000 events must NOT grow with the session length (no
// joinedBody = joinedBody + box-style O(N²) regrowth). The metric
// to watch is ns/op + allocs/op staying ~constant across N — if
// a future change reintroduces the O(N²) shape, the per-call
// cost balloons proportionally.
func BenchmarkAppendTranscriptEvent_5000(b *testing.B) {
	contentW := 96
	ev := datasource.MessageEvent{
		Kind: "user_text",
		TS:   jobDetailFixedTS,
		Text: "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Allocate one fresh entry per b.N batch of 5000 so the
		// allocation count is dominated by the appendTranscriptEvent
		// body, not the prelude. The N is the per-iteration amortised
		// cost of one append on a buffer that grew to ~5000 events.
		b.StopTimer()
		te := &jobTranscriptEntry{}
		b.StartTimer()
		for k := 0; k < 5000; k++ {
			appendTranscriptEvent(te, ev, contentW)
		}
	}
}
