// Package tui — sessiontranscript.go is the per-sessionID transcript cache
// + per-event box rendering. The live-subscription lifecycle that feeds
// events into here lives in sessiondetail.go; the renderer that joins the
// per-event boxes into the Detail pane body lives in render.go.

package tui

import (
	"strings"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/markdown"
	"autosk/internal/timeformat"
)

// parseWireTime parses an RFC3339 UTC wire timestamp. Returns the
// zero time on empty/malformed input so callers can route through
// internal/timeformat (which renders the zero time as "") without a
// panic-prone byte-slice. NEVER slice line.Timestamp by index — a
// custom/external agent can emit a short or malformed value.
func parseWireTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ensureTranscriptEntryLocked returns the cache entry for sessionID,
// creating one if needed. Eviction policy: when the map is at
// sessionTranscriptCacheMax AND sessionID is not already present, drop the
// least-recently-touched non-key entry (LRU). Every call — hit OR
// miss — stamps the entry's touchedAt with time.Now() so a return
// visit to a cached session refreshes its LRU rank.
//
// MUST be called with state.mu held (read+write — we mutate the
// map). The "Locked" suffix mirrors the selectedTaskLocked /
// selectedSessionLocked conventions for "I hold the lock; just give me
// the value".
func (gu *Gui) ensureTranscriptEntryLocked(sessionID string) *sessionTranscriptEntry {
	if gu.st.sessionTranscript == nil {
		gu.st.sessionTranscript = map[string]*sessionTranscriptEntry{}
	}
	now := time.Now()
	if te, ok := gu.st.sessionTranscript[sessionID]; ok {
		te.touchedAt = now
		return te
	}
	evictTranscriptIfNeeded(gu.st.sessionTranscript, sessionID, sessionTranscriptCacheMax)
	te := &sessionTranscriptEntry{touchedAt: now}
	gu.st.sessionTranscript[sessionID] = te
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
// The scan is O(N) but N is capped at sessionTranscriptCacheMax (32) so
// the cost is trivial — cheaper than maintaining an ordered list
// alongside the map. A zero touchedAt sorts as "oldest" so a freshly
// allocated entry whose stamp wasn't set (defensive path) evicts
// before genuinely-used entries.
func evictTranscriptIfNeeded(m map[string]*sessionTranscriptEntry, key string, maxLen int) {
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
// When te.events would exceed sessionLiveBufCap, the oldest 25% are
// dropped in one allocation and te.truncated is set.
//
// te.renderedWidth is INTENTIONALLY not touched here — it remains
// authoritative for the last rebuildTranscriptBoxes pass. If
// contentW differs from te.renderedWidth (mid-stream resize race),
// the existing boxes are at the OLD width while the new box would
// be at the NEW width; keeping renderedWidth pinned to the old
// value means renderSessionDetail's `renderedWidth != contentW` guard
// fires on the next frame and rebuilds the whole slice at the
// current width. In the steady state contentW always equals
// te.renderedWidth (pumpSessionLive reads gu.innerWidth at append time
// and that matches the value rebuildTranscriptBoxes was last fed),
// so the rebuild only fires when a real resize happened.
func appendTranscriptEvent(te *sessionTranscriptEntry, line api.TranscriptLine, contentW int) {
	if te == nil {
		return
	}
	te.events = append(te.events, datasource.LiveEvent{Kind: "message", Line: line})
	box := renderTranscriptLineBox(line, contentW) // renamed from renderTranscriptEventBox
	te.renderedBoxes = append(te.renderedBoxes, box)
	// joinedBody needs a rebuild — do NOT extend in place via
	// joinedBody = joinedBody + "\n" + box + "\n": that reallocates
	// the full prefix on every append and degrades the live-pump
	// path to O(N²) over the session. We mark dirty and let
	// renderSessionDetail rebuild the whole joined string in one
	// strings.Join pass on the next frame (which only fires when
	// the Detail pane is actually visible). The dirty flag set
	// here covers BOTH the plain append AND the cap-trim path
	// below — the trim mutates renderedBoxes too, but dirty is
	// already true so no second assignment is needed.
	te.joinedDirty = true
	if n := len(te.events); n > sessionLiveBufCap {
		keep := sessionLiveBufCap - sessionLiveBufCap/4 // drop oldest 25%
		newEvs := make([]datasource.LiveEvent, keep)
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
// after loadSessionArchive replaces te.events.
//
// Also rebuilds te.joinedBody in the same pass and clears the
// dirty flag — a resize / archive replace is the worst case for
// the join (every box is fresh string content), and folding it in
// here saves a second loop over te.renderedBoxes when
// renderSessionDetail next runs.
func rebuildTranscriptBoxes(te *sessionTranscriptEntry, contentW int) {
	if te == nil {
		return
	}
	te.renderedBoxes = te.renderedBoxes[:0]
	if cap(te.renderedBoxes) < len(te.events) {
		te.renderedBoxes = make([]string, 0, len(te.events))
	}
	for i := range te.events {
		te.renderedBoxes = append(te.renderedBoxes, renderTranscriptLineBox(te.events[i].Line, contentW))
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
func rebuildJoinedBody(te *sessionTranscriptEntry) {
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

// renderTranscriptLineBox renders one api.TranscriptLine as a labeled
// box. The label varies by Type:
//   - "message": timestamp + role + (tool info if applicable)
//   - "custom": timestamp + custom type
//   - "session": session header info
//
// Body rendering uses pi-format message content or custom data.
func renderTranscriptLineBox(line api.TranscriptLine, contentW int) string {
	// Width arithmetic — drawLabeledBox takes the OUTER width, not
	// the content width, so we add the 4 cells of frame+padding
	// back in. Floor at 6 so drawLabeledBox doesn't fall through
	// to its plain-body fallback.
	outerW := contentW + 4
	if outerW < 6 {
		outerW = 6
	}

	var label, body string

	switch line.Type {
	case "session":
		// Session header line. The wire timestamp is RFC3339 UTC;
		// render it in the operator's local zone via timeformat
		// (AGENTS.md: all user-facing times route through timeformat).
		label = styleHeader.Render("Session: " + line.Agent + " on " + line.Step)
		if started := timeformat.FormatDateTime(parseWireTime(line.Timestamp)); started != "" {
			body = "Started: " + started
		}

	case "message":
		// Pi-format message entry
		if line.Message == nil {
			return ""
		}
		// Render the RFC3339 UTC timestamp in the operator's local
		// zone via timeformat — never byte-slice line.Timestamp (a
		// short/malformed value would panic the render loop).
		stamp := ""
		if s := timeformat.FormatDateTimeSmart(parseWireTime(line.Timestamp)); s != "" {
			stamp = s + " "
		}
		label = styleHeader.Render(stamp + line.Message.Role)
		if line.Message.ToolName != "" {
			label += " " + styleAccent.Render(line.Message.ToolName)
		}
		// Render message content. Text() flattens text+thinking blocks,
		// so a pure tool-call turn has an empty Text() — surface the
		// toolCall block names too so a tool-only assistant turn isn't
		// rendered as an empty box.
		text := line.Message.Text()
		if text != "" {
			if line.Message.Role == "assistant" {
				body = markdown.Render(text, contentW)
			} else {
				body = wrapPlain(text, contentW)
			}
		}
		if calls := transcriptToolCalls(line.Message); calls != "" {
			if body != "" {
				body += "\n"
			}
			body += wrapPlain(calls, contentW)
		}

	case "custom":
		// Custom entry (e.g. autosk:* events)
		customType := strings.TrimPrefix(line.CustomType, "autosk:")
		label = styleHeader.Render(customType)
		// For now, just show raw JSON data
		if len(line.Data) > 0 {
			body = wrapPlain(string(line.Data), contentW)
		}

	default:
		label = styleHeader.Render("Unknown: " + line.Type)
	}

	return drawLabeledBox(label, body, outerW)
}

// transcriptToolCalls returns a compact one-tool-per-line summary of the
// toolCall blocks in m (e.g. "→ Read", "→ Bash"), or "" when the message
// carries no tool calls. Lets the Detail box show something for a
// tool-only assistant turn whose Text() is empty.
func transcriptToolCalls(m *api.TranscriptMessage) string {
	if m == nil {
		return ""
	}
	var names []string
	for _, b := range m.Blocks() {
		if b.Type == "toolCall" && b.Name != "" {
			names = append(names, "→ "+b.Name)
		}
	}
	return strings.Join(names, "\n")
}
