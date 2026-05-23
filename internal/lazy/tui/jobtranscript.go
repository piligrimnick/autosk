// Package tui — jobtranscript.go is the per-jobID transcript cache
// + per-event box rendering. The SSE lifecycle that feeds events
// into here lives in jobdetail.go; the renderer that joins the
// per-event boxes into the Detail pane body lives in render.go.

package tui

import (
	"strings"
	"time"

	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/markdown"
	"autosk/internal/timeformat"
)

// ensureTranscriptEntryLocked returns the cache entry for jobID,
// creating one if needed. Eviction policy: when the map is at
// jobTranscriptCacheMax AND jobID is not already present, drop the
// least-recently-touched non-key entry (LRU). Every call — hit OR
// miss — stamps the entry's touchedAt with time.Now() so a return
// visit to a cached job refreshes its LRU rank.
//
// MUST be called with state.mu held (read+write — we mutate the
// map). The "Locked" suffix mirrors the selectedTaskLocked /
// selectedJobLocked conventions for "I hold the lock; just give me
// the value".
func (gu *Gui) ensureTranscriptEntryLocked(jobID string) *jobTranscriptEntry {
	if gu.st.jobTranscript == nil {
		gu.st.jobTranscript = map[string]*jobTranscriptEntry{}
	}
	now := time.Now()
	if te, ok := gu.st.jobTranscript[jobID]; ok {
		te.touchedAt = now
		return te
	}
	evictTranscriptIfNeeded(gu.st.jobTranscript, jobID, jobTranscriptCacheMax)
	te := &jobTranscriptEntry{touchedAt: now}
	gu.st.jobTranscript[jobID] = te
	return te
}

// evictTranscriptIfNeeded drops the least-recently-touched entry
// from m if the map is at maxLen AND key isn't already present.
//
// Contract: the caller (ensureTranscriptEntryLocked) only invokes
// this on the cache-miss path, so `key` is GUARANTEED not to be in
// `m`. The outer `_, exists := m[key]` guard is a paranoia hatch
// against a future caller misusing the helper; the per-iteration
// `k == key` check the previous version carried was redundant
// (the key is absent by contract) and has been removed.
//
// The scan is O(N) but N is capped at jobTranscriptCacheMax (32) so
// the cost is trivial — cheaper than maintaining an ordered list
// alongside the map. A zero touchedAt sorts as "oldest" so a freshly
// allocated entry whose stamp wasn't set (defensive path) evicts
// before genuinely-used entries.
func evictTranscriptIfNeeded(m map[string]*jobTranscriptEntry, key string, maxLen int) {
	if _, exists := m[key]; exists {
		return
	}
	if len(m) < maxLen {
		return
	}
	var victim string
	var victimAt time.Time
	first := true
	for k, te := range m {
		if first || te.touchedAt.Before(victimAt) {
			victim = k
			victimAt = te.touchedAt
			first = false
		}
	}
	if !first {
		delete(m, victim)
	}
}

// appendTranscriptEvent appends ev to te.events and the
// corresponding pre-rendered drawLabeledBox to te.renderedBoxes.
// When te.events would exceed jobLiveBufCap, the oldest 25% are
// dropped in one allocation and te.truncated is set.
//
// te.renderedWidth is INTENTIONALLY not touched here — it remains
// authoritative for the last rebuildTranscriptBoxes pass. If
// contentW differs from te.renderedWidth (mid-stream resize race),
// the existing boxes are at the OLD width while the new box would
// be at the NEW width; keeping renderedWidth pinned to the old
// value means renderJobDetail's `renderedWidth != contentW` guard
// fires on the next frame and rebuilds the whole slice at the
// current width. In the steady state contentW always equals
// te.renderedWidth (pumpJobLive reads gu.innerWidth at append time
// and that matches the value rebuildTranscriptBoxes was last fed),
// so the rebuild only fires when a real resize happened.
func appendTranscriptEvent(te *jobTranscriptEntry, ev datasource.MessageEvent, contentW int) {
	if te == nil {
		return
	}
	te.events = append(te.events, ev)
	box := renderTranscriptEventBox(ev, contentW)
	te.renderedBoxes = append(te.renderedBoxes, box)
	// joinedBody needs a rebuild — do NOT extend in place via
	// joinedBody = joinedBody + "\n" + box + "\n": that reallocates
	// the full prefix on every append and degrades the live-pump
	// path to O(N²) over the session. We mark dirty and let
	// renderJobDetail rebuild the whole joined string in one
	// strings.Join pass on the next frame (which only fires when
	// the Detail pane is actually visible). The dirty flag set
	// here covers BOTH the plain append AND the cap-trim path
	// below — the trim mutates renderedBoxes too, but dirty is
	// already true so no second assignment is needed.
	te.joinedDirty = true
	if n := len(te.events); n > jobLiveBufCap {
		keep := jobLiveBufCap - jobLiveBufCap/4 // drop oldest 25%
		newEvs := make([]datasource.MessageEvent, keep)
		copy(newEvs, te.events[n-keep:])
		te.events = newEvs
		newBoxes := make([]string, keep)
		copy(newBoxes, te.renderedBoxes[n-keep:])
		te.renderedBoxes = newBoxes
		te.truncated = true
	}
}

// rebuildTranscriptBoxes re-renders every per-event box at the
// given contentW. Called when the pane width changes (resize) and
// after loadJobArchive replaces te.events.
//
// Also rebuilds te.joinedBody in the same pass and clears the
// dirty flag — a resize / archive replace is the worst case for
// the join (every box is fresh string content), and folding it in
// here saves a second loop over te.renderedBoxes when
// renderJobDetail next runs.
func rebuildTranscriptBoxes(te *jobTranscriptEntry, contentW int) {
	if te == nil {
		return
	}
	te.renderedBoxes = te.renderedBoxes[:0]
	if cap(te.renderedBoxes) < len(te.events) {
		te.renderedBoxes = make([]string, 0, len(te.events))
	}
	for i := range te.events {
		te.renderedBoxes = append(te.renderedBoxes, renderTranscriptEventBox(te.events[i], contentW))
	}
	te.renderedWidth = contentW
	rebuildJoinedBody(te)
}

// rebuildJoinedBody refreshes te.joinedBody from te.renderedBoxes in
// a single strings.Builder pass and clears the dirty flag. The
// emitted shape mirrors renderJobDetail's legacy per-event loop —
//
//	"\n" + box0 + "\n" + "\n" + box1 + "\n" + ...
//
// — so a resize / append / archive-replace produces a string the
// renderer can flush with a single WriteString. Empty boxes slice
// → empty body (no leading "\n" so the caller's preceding header
// section keeps its own trailing newline budget).
//
// Pre-sizes the Builder via Grow(sum + 2*N) so the typical 5000-
// event session avoids the usual doubling reallocs as the body
// grows.
func rebuildJoinedBody(te *jobTranscriptEntry) {
	if te == nil {
		return
	}
	te.joinedDirty = false
	if len(te.renderedBoxes) == 0 {
		te.joinedBody = ""
		return
	}
	total := 0
	for i := range te.renderedBoxes {
		total += len(te.renderedBoxes[i]) + 2 // leading "\n" + trailing "\n"
	}
	var b strings.Builder
	b.Grow(total)
	for i := range te.renderedBoxes {
		b.WriteByte('\n')
		b.WriteString(te.renderedBoxes[i])
		b.WriteByte('\n')
	}
	te.joinedBody = b.String()
}

// renderTranscriptEventBox renders one MessageEvent as a labeled
// box. The label is "<smart-datetime> <kind-styled> [<name-accent>]".
// Body rendering:
//
//   - kind starts with "assistant" (assistant_text / assistant_thinking):
//     markdown.Render at contentW.
//   - empty Text: empty body (drawLabeledBox accepts this and renders
//     a frame with just the label).
//   - everything else: plain text wrapped at contentW.
//
// The assistant check uses strings.HasPrefix so future assistant_*
// kinds (e.g. assistant_tool_use) automatically pick up markdown
// rendering.
func renderTranscriptEventBox(ev datasource.MessageEvent, contentW int) string {
	// Width arithmetic — drawLabeledBox takes the OUTER width, not
	// the content width, so we add the 4 cells of frame+padding
	// back in. Floor at 6 so drawLabeledBox doesn't fall through
	// to its plain-body fallback.
	outerW := contentW + 4
	if outerW < 6 {
		outerW = 6
	}

	stamp := ""
	if !ev.TS.IsZero() {
		stamp = timeformat.FormatDateTimeSmart(ev.TS) + " "
	}
	label := styleHeader.Render(stamp + ev.Kind)
	if ev.Name != "" {
		label += " " + styleAccent.Render(ev.Name)
	}

	var body string
	switch {
	case ev.Text == "":
		body = ""
	case strings.HasPrefix(ev.Kind, "assistant"):
		body = markdown.Render(ev.Text, contentW)
	default:
		body = wrapPlain(ev.Text, contentW)
	}
	return drawLabeledBox(label, body, outerW)
}
