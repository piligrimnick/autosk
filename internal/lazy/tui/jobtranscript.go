// Package tui — jobtranscript.go is the per-jobID transcript cache
// + per-event box rendering. The SSE lifecycle that feeds events
// into here lives in jobdetail.go; the renderer that joins the
// per-event boxes into the Detail pane body lives in render.go.

package tui

import (
	"strings"

	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/markdown"
	"autosk/internal/timeformat"
)

// ensureTranscriptEntryLocked returns the cache entry for jobID,
// creating one if needed. Eviction policy: when the map is at
// jobTranscriptCacheMax AND jobID is not already present, drop one
// arbitrary non-key entry to make room (same shape as
// evictCacheIfNeeded for the comments / signals caches).
//
// MUST be called with state.mu held (read+write — we mutate the
// map). The "Locked" suffix mirrors the selectedTaskLocked /
// selectedJobLocked conventions for "I hold the lock; just give me
// the value".
func (gu *Gui) ensureTranscriptEntryLocked(jobID string) *jobTranscriptEntry {
	if gu.st.jobTranscript == nil {
		gu.st.jobTranscript = map[string]*jobTranscriptEntry{}
	}
	if te, ok := gu.st.jobTranscript[jobID]; ok {
		return te
	}
	evictTranscriptIfNeeded(gu.st.jobTranscript, jobID, jobTranscriptCacheMax)
	te := &jobTranscriptEntry{}
	gu.st.jobTranscript[jobID] = te
	return te
}

// evictTranscriptIfNeeded drops one arbitrary non-key entry from m
// if the map is at maxLen AND key isn't already present. Mirrors
// evictCacheIfNeeded scoped to the transcript map.
func evictTranscriptIfNeeded(m map[string]*jobTranscriptEntry, key string, maxLen int) {
	if _, exists := m[key]; exists {
		return
	}
	if len(m) < maxLen {
		return
	}
	for k := range m {
		if k == key {
			continue
		}
		delete(m, k)
		return
	}
}

// appendTranscriptEvent appends ev to te.events and the
// corresponding pre-rendered drawLabeledBox to te.renderedBoxes.
// When te.events would exceed jobLiveBufCap, the oldest 25% are
// dropped in one allocation and te.truncated is set.
func appendTranscriptEvent(te *jobTranscriptEntry, ev datasource.MessageEvent, contentW int) {
	if te == nil {
		return
	}
	te.events = append(te.events, ev)
	box := renderTranscriptEventBox(ev, contentW)
	te.renderedBoxes = append(te.renderedBoxes, box)
	te.renderedWidth = contentW
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
