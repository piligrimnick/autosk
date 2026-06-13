// Package tui — sessiondetail.go is the Detail-pane side of the
// post-Inspector world: it owns the per-session SSE subscription
// lifecycle, the input-textarea editor + dispatch handlers, and the
// scroll helpers shared by Session Detail and Task Detail. The
// per-event cache + box rendering lives in sessiontranscript.go.

package tui

import (
	"context"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
)

// hydrateContext returns a context derived from parent with a
// 5×refresh budget (clamped at 5s minimum so a Refresh=0 / very-small
// Refresh doesn't immediately cancel). Mirrors refreshAll's choice so
// archive hydration honours the same deadline as the dashboard.
func hydrateContext(parent context.Context, refresh time.Duration) (context.Context, context.CancelFunc) {
	budget := 5 * refresh
	if budget < 5*time.Second {
		budget = 5 * time.Second
	}
	return context.WithTimeout(parent, budget)
}

// isSessionRunning reports whether s is currently in the running enum
// state. Drives the SSE subscription (we stream transcript events
// for any running session, even when pi is idle between turns).
func isSessionRunning(s datasource.Session) bool {
	return s.Status == runstore.StatusRunning
}

// isSessionLive reports whether s is in a state where the operator
// can author input — i.e. whenever the daemon would accept a
// SessionInput call. Drives the visibility of winSessionInput.
//
// The daemon's input dispatch (session.input) accepts a `steer`
// or `followup` kind in ANY non-terminal status. Mirroring that here
// means the input view's lifecycle matches the session's lifecycle:
// allocated once when the session becomes non-terminal, destroyed
// once when it terminates. No churn on agent_start/agent_end
// boundaries → no draft-loss-on-view-recreation, no
// phantom-focus-after-reshuffle to a non-streaming row.
//
// The user's manual-test feedback ("input должно отображаться
// только при live-режиме работы агента, когда ему можно
// отправить сообщения") is satisfied by this predicate's
// clarifying clause: hide the input when the job is in archive
// view (= terminal), show it whenever messages can be sent.
func isSessionLive(s datasource.Session) bool {
	return !s.Status.IsTerminal()
}

// sessionInputOverlayH is the outer height (frame + content + frame) of
// the winSessionInput overlay that sits inside winDetail. Two content
// rows are enough for typing follow-up messages; longer drafts wrap
// inside the textarea (gocui's editor handles vertical scrolling).
const sessionInputOverlayH = 4 // renamed from jobInputOverlayH

// detailEffectiveInnerH returns the inner content height of winDetail
// adjusted for the winSessionInput overlay (if present this frame).
// When the input is overlaid at the bottom of detail, the rows it
// covers are not visible to the operator, so sticky-tail / scroll
// math should treat the visible region as ending just above the
// overlay. Returns 1 at minimum so the / 0 trap doesn't bite.
func (gu *Gui) detailEffectiveInnerH() int {
	v, err := gu.g.View(winDetail)
	if err != nil || v == nil {
		return 1
	}
	_, h := v.InnerSize()
	if _, err := gu.g.View(winSessionInput); err == nil { // renamed from winJobInput
		// The input overlay covers the bottom sessionInputOverlayH rows
		// of detail's inner area. Subtract that from the visible
		// content height.
		h -= sessionInputOverlayH
	}
	if h < 1 {
		h = 1
	}
	return h
}

// ---- SSE lifecycle ------------------------------------------------------

// scheduleSessionLive (re)arms the debounce timer for opening an SSE
// subscription on sessionID. Calling this on every cursor move + every
// refresh-driven status update keeps the timer pinned to the latest
// selection, so a burst of j/k presses collapses into one StreamSession
// call against the final-resting cursor row sessionLiveDebounce after
// the last keystroke.
//
// Behaviour:
//   - sessionID == "" OR !running: cancels any pending timer AND any
//     active handle. Terminal sessions don't stream.
//   - sessionID == st.sessionLiveSessionID AND a handle already exists: cancels
//     the timer and is a no-op otherwise.
//   - Anything else: cancels timer + (eventually) active handle,
//     arms a new timer pointing at openSessionLive(sessionID).
func (gu *Gui) scheduleSessionLive(sessionID string, running bool) {
	if sessionID == "" || !running {
		gu.stopSessionLive() // renamed from stopJobLive
		return
	}
	// Stop any pending timer + arm the new one in ONE critical
	// section so a concurrent scheduleJobLive on a different job
	// can't slip its timer between our Stop and our Set —
	// time.AfterFunc is non-blocking, so it's safe to call under
	// the lock.
	gu.st.withLock(func() {
		if gu.st.sessionLiveTimer != nil {
			gu.st.sessionLiveTimer.Stop()
			gu.st.sessionLiveTimer = nil
		}
		if gu.st.sessionLiveSessionID == sessionID && gu.st.sessionLiveHandle != nil {
			// Already streaming the right session — nothing to do.
			return
		}
		gu.st.sessionLiveTimer = time.AfterFunc(sessionLiveDebounce, func() { gu.openSessionLive(sessionID) })
	})
}

// openSessionLive opens the SSE subscription for sessionID after the debounce
// fires. Runs on the timer goroutine; re-checks selection so a cursor
// move that overlapped the timer doesn't open a stream for a
// no-longer-selected session.
func (gu *Gui) openSessionLive(sessionID string) {
	// Selection re-check: if the cursor no longer points at sessionID OR
	// the session has gone terminal, abort.
	var keep bool
	var hadHandle bool
	gu.st.withRLock(func() {
		if s, ok := gu.st.selectedSession(); ok && s.ID == sessionID && isSessionRunning(s) {
			keep = true
		}
		hadHandle = gu.st.sessionLiveSessionID == sessionID && gu.st.sessionLiveHandle != nil
	})
	if !keep {
		return
	}
	if hadHandle {
		return
	}

	// Cancel previous handle, then open the new one. We open BEFORE
	// taking the write lock so the StreamSession HTTP roundtrip
	// doesn't block other state writes.
	gu.stopSessionLive()
	ctx, cancel := context.WithCancel(gu.ctx)
	handle, err := gu.ds.StreamSession(ctx, sessionID) // renamed from StreamLive
	if err != nil {
		cancel()
		gu.flashf("warn", "live: %v", err)
		return
	}
	gu.st.withLock(func() {
		gu.st.sessionLiveSessionID = sessionID
		gu.st.sessionLiveHandle = handle
		gu.st.sessionLiveCancel = cancel
	})
	go gu.pumpSessionLive(ctx, sessionID, handle)
}

// stopSessionLive cancels the debounce timer + active SSE handle. Safe
// to call when nothing is active. Called from quit, from
// afterCursorMove when cursor leaves panelSessions / lands on a terminal
// session, and from applyRefreshLocked when the streamed session vanishes
// from the sessions slice.
func (gu *Gui) stopSessionLive() { // renamed from stopJobLive
	if gu == nil || gu.st == nil {
		return
	}
	var (
		h *datasource.LiveHandle
		c context.CancelFunc
		t *time.Timer
	)
	gu.st.withLock(func() {
		h = gu.st.sessionLiveHandle
		c = gu.st.sessionLiveCancel
		t = gu.st.sessionLiveTimer
		gu.st.sessionLiveHandle = nil
		gu.st.sessionLiveCancel = nil
		gu.st.sessionLiveSessionID = ""
		gu.st.sessionLiveTimer = nil
	})
	if t != nil {
		t.Stop()
	}
	if c != nil {
		c()
	}
	if h != nil {
		_ = h.Close()
	}
}

// pumpSessionLive consumes SSE events for sessionID and appends them to the
// per-session transcript cache. Mirrors the previous Inspector pumpLive
// but routes events into state.sessionTranscript (a per-sessionID cache of
// rendered boxes) instead of the inspector's flat liveBuf slice.
//
// Risk #4 mitigation: coalesce sub-30ms bursts into a single
// g.Update via pumpLiveLoop. Owner-check on flush guards against the
// transcript-cache being switched out from under us between throttle
// queue and apply.
func (gu *Gui) pumpSessionLive(ctx context.Context, ownerSessionID string, h *datasource.LiveHandle) {
	pumpLiveLoop(ctx, h.Events, livePumpThrottle, func(batch []datasource.LiveEvent) {
		gu.g.Update(func(_ *gocui.Gui) error {
			gu.st.withLock(func() {
				if gu.st.sessionLiveSessionID != ownerSessionID {
					return
				}
				te := gu.ensureTranscriptEntryLocked(ownerSessionID) // renamed
				contentW := gu.innerWidth(winDetail) - 4
				if contentW < 1 {
					contentW = 1
				}
				for _, ev := range batch {
					switch ev.Kind {
					case "message":
						appendTranscriptEvent(te, ev.Line, contentW) // v2: ev.Line instead of ev.Message
					case "status", "done":
						// Status / done envelopes don't carry transcript
						// content, so there is nothing to append here.
						// On `done` the datasource StreamSession tears the
						// subscription down (closes the LiveHandle), which
						// closes h.Events and makes pumpLiveLoop exit via
						// its `!ok` branch — so this loop self-terminates.
						// (The archive refetch for the running→terminal
						// transition is handled separately in
						// applyRefreshLocked via the prevSession/cur compare.)
					case "error":
						te.err = ev.Err
					}
				}
			})
			return nil
		})
	})
}

// livePumpThrottle is the coalescing window. ~30ms is what lazygit
// uses; small enough that the user never sees the lag, large enough
// that a burst of 20+ events collapses into a single redraw.
const livePumpThrottle = 30 * time.Millisecond

// pumpLiveLoop is the pure-Go throttle loop. Accepts the event
// channel, the throttle window, and a flush callback that's invoked
// with each non-empty batch. Tested directly so a future change to
// the coalesce logic can't silently regress the 'one g.Update per
// burst' invariant. Returns when ctx is done OR events is closed.
func pumpLiveLoop(ctx context.Context, events <-chan datasource.LiveEvent, throttle time.Duration, flush func([]datasource.LiveEvent)) {
	var (
		pending []datasource.LiveEvent
		timer   *time.Timer
	)
	doFlush := func() {
		if len(pending) == 0 {
			return
		}
		batch := pending
		pending = nil
		flush(batch)
	}
	for {
		select {
		case <-ctx.Done():
			doFlush()
			return
		case ev, ok := <-events:
			if !ok {
				doFlush()
				return
			}
			pending = append(pending, ev)
			dlog("pumpLive: queued %s (pending=%d)", ev.Kind, len(pending))
			if timer == nil {
				timer = time.NewTimer(throttle)
			} else {
				select {
				case <-timer.C:
				default:
				}
				timer.Reset(throttle)
			}
		case <-timerC(timer):
			doFlush()
		}
	}
}

// timerC returns t.C, or a nil channel when t is nil so the select
// case never fires. Lets us start the timer lazily on the first
// queued event.
func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// ---- archive hydration --------------------------------------------------

// jobArchiveStale reports whether jobID's transcript cache entry
// needs a fresh Messages() fetch. The single predicate consulted
// by both scheduleJobArchive (before enqueueing a worker) and
// loadJobArchive (at the top of the worker, as the in-flight
// coalesce guard) so cursor-bounce can't enqueue more than one
// fetch per cache-miss-window. MUST be called WITHOUT holding
// state.mu — we acquire it inside.
//
// Policy:
//   - Miss            → stale (need fetch).
//   - Hit, loadedAt zero (cleared by refresh.go's running→terminal
//     transition path, or never loaded yet) → stale.
//   - Hit, running    → fresh (SSE keeps it current; no TTL refetch).
//   - Hit, terminal   → stale only if older than sessionTranscriptTerminalTTL.
func (gu *Gui) sessionArchiveStale(sessionID string, running bool) bool {
	var stale bool
	gu.st.withLock(func() {
		te, ok := gu.st.sessionTranscript[sessionID]
		if !ok {
			stale = true
			return
		}
		if te.loadedAt.IsZero() {
			stale = true
			return
		}
		if !running && time.Since(te.loadedAt) > sessionTranscriptTerminalTTL {
			stale = true
		}
	})
	return stale
}

// loadSessionArchive fetches the archive for sessionID and merges it into the
// per-session transcript cache. Always runs on a worker goroutine.
//
// Coalesce: re-checks sessionArchiveStale at the top under the lock. If
// another worker just populated the entry between the scheduler's
// decision and this worker actually running, we bail without a
// duplicate fetch call. The freshness check is the same
// predicate scheduleSessionArchive uses, so the contract
// ("back-to-back schedules don't pile on requests") holds even
// when worker_2 is enqueued before worker_1 returns: worker_2 sees
// loadedAt set and short-circuits.
//
// Merge policy: the archive snapshot replaces the prefix; any live
// events whose LineNum strictly post-dates the archive's last LineNum are
// preserved so a slow fetch response doesn't lose the live tail.
func (gu *Gui) loadSessionArchive(sessionID string) {
	if sessionID == "" {
		return
	}
	// Determine the session's running state so the staleness predicate
	// can honour the running-vs-terminal-TTL split. A missing session
	// (e.g. cursor moved away during scheduling) treats running=false,
	// which keeps the TTL gate active.
	running := false
	gu.st.withRLock(func() {
		for i := range gu.st.sessions {
			if gu.st.sessions[i].ID == sessionID {
				running = isSessionRunning(gu.st.sessions[i])
				return
			}
		}
	})
	if !gu.sessionArchiveStale(sessionID, running) {
		return
	}
	ctx, cancel := hydrateContext(gu.ctx, gu.opts.Refresh)
	defer cancel()
	evs, err := gu.ds.SessionTranscript(ctx, sessionID)
	if err != nil {
		gu.flashf("warn", "archive load: %v", err)
	}
	gu.g.Update(func(_ *gocui.Gui) error {
		gu.st.withLock(func() {
			gu.applyArchiveLoadLocked(sessionID, evs, err)
		})
		return nil
	})
}

// applyArchiveLoadLocked merges a freshly-fetched archive snapshot
// into the per-session transcript cache and clears the per-load flags.
// MUST be called with state.mu held — the "Locked" suffix mirrors
// the rest of the helpers in this file.
//
// Pulled out of loadSessionArchive so tests can drive the post-fetch
// path directly (gocui's g.Update queue is async + only drained by
// MainLoop, which test harnesses don't run). The contract under
// test:
//
//   - te.loadedAt is stamped to time.Now() regardless of fetchErr.
//   - te.err mirrors the fetch error.
//   - On success: te.events is replaced by mergeArchiveAndLive
//     (full snapshot + any post-snapshot live tail), te.truncated
//     is reset to false (the full archive carries every event the
//     previous live-cap drop tossed), and te.renderedBoxes is
//     rebuilt at the current content width.
func (gu *Gui) applyArchiveLoadLocked(sessionID string, evs []datasource.LiveEvent, fetchErr error) {
	te := gu.ensureTranscriptEntryLocked(sessionID)
	te.err = fetchErr
	te.loadedAt = time.Now()
	if fetchErr != nil {
		return
	}
	contentW := gu.innerWidth(winDetail) - 4
	if contentW < 1 {
		contentW = 1
	}
	te.events = mergeArchiveAndLive(evs, te.events)
	// A fresh archive carries the FULL event set (SessionTranscript
	// returns the complete pi-format transcript), so any prior
	// live-cap truncation no
	// longer applies — drop the stale flag so renderSessionDetail
	// stops prepending the "(transcript truncated...)" note.
	te.truncated = false
	rebuildTranscriptBoxes(te, contentW)
}

// scheduleSessionArchive queues loadSessionArchive for sessionID on the worker
// pool. loadSessionArchive itself re-checks sessionArchiveStale at the top
// (under the model lock) so back-to-back schedules that overlap a
// running worker collapse into one actual fetch.
func (gu *Gui) scheduleSessionArchive(sessionID string, running bool) {
	if sessionID == "" {
		return
	}
	if !gu.sessionArchiveStale(sessionID, running) {
		return
	}
	if gu.g == nil {
		// Tests with no real gocui drive loadSessionArchive directly; the
		// scheduler is a no-op without a worker pool.
		return
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		gu.loadSessionArchive(sessionID)
		return nil
	})
}

// mergeArchiveAndLive returns archive followed by any live events
// whose TS strictly post-dates archive's last event. When archive is
// empty the live slice is returned verbatim. When live is empty the
// archive is returned. Both inputs may be nil.
func mergeArchiveAndLive(archive, live []datasource.LiveEvent) []datasource.LiveEvent {
	if len(live) == 0 {
		return archive
	}
	if len(archive) == 0 {
		return live
	}
	lastLineNum := archive[len(archive)-1].LineNum
	tail := make([]datasource.LiveEvent, 0, len(live))
	for _, ev := range live {
		if ev.LineNum > lastLineNum {
			tail = append(tail, ev)
		}
	}
	if len(tail) == 0 {
		return archive
	}
	out := make([]datasource.LiveEvent, 0, len(archive)+len(tail))
	out = append(out, archive...)
	out = append(out, tail...)
	return out
}

// ---- detail-pane scroll helpers ----------------------------------------

// detailScroll moves the Detail-pane viewport up/down one line at a
// time. Reused by both Task Detail and Job Detail.
func (gu *Gui) detailScroll(step int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		return gu.scrollViewByLines(winDetail, step)
	}
}

// detailScrollPage scrolls by one screen of the detail view.
//
// Uses InnerSize (not Size): gocui's origin oy indexes into the
// view's inner content area, so paging by the outer height would
// undershoot by the 2-cell frame each page. Mirrors the same fix
// writeViewSticky applies for the tail anchor.
func (gu *Gui) detailScrollPage(dir int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		if v, err := gu.g.View(winDetail); err != nil || v == nil {
			return nil
		}
		return gu.scrollViewByLines(winDetail, dir*gu.detailEffectiveInnerH())
	}
}

// detailScrollTo jumps to the top (g) or bottom (G) of the detail
// pane.
//
// Uses InnerSize for the bottom-anchor math (see detailScrollPage)
// so G lands on the actual last line, not 2 lines short.
func (gu *Gui) detailScrollTo(bottom bool) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winDetail)
		if err != nil || v == nil {
			return nil
		}
		ox, _ := v.Origin()
		if !bottom {
			v.SetOrigin(ox, 0)
			return nil
		}
		// viewBufferLineCount, not strings.Count("\n"): the latter
		// undercounts visible lines by 1 (gocui joins v.lines with
		// '\n' without a trailing separator), which would leave G
		// one row short of the actual bottom and hide the last
		// drawLabeledBox's bottom border.
		lines := viewBufferLineCount(v)
		h := gu.detailEffectiveInnerH()
		target := lines - h
		if target < 0 {
			target = 0
		}
		v.SetOrigin(ox, target)
		return nil
	}
}

// scrollViewByLines moves the named view's origin by step lines,
// clamping to [0, max(0, totalLines-innerHeight)]. Uses InnerSize
// for the same reason detailScrollPage / detailScrollTo do.
func (gu *Gui) scrollViewByLines(name string, step int) error {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return nil
	}
	ox, oy := v.Origin()
	ny := oy + step
	if ny < 0 {
		ny = 0
	}
	var h int
	if name == winDetail {
		// winDetail's effective visible height shrinks when the
		// winJobInput overlay sits over its bottom rows.
		h = gu.detailEffectiveInnerH()
	} else {
		_, h = v.InnerSize()
		if h < 1 {
			h = 1
		}
	}
	// viewBufferLineCount: see the comment in detailScrollTo for
	// the off-by-one this avoids vs. strings.Count("\n").
	lines := viewBufferLineCount(v)
	max := lines - h
	if max < 0 {
		max = 0
	}
	if ny > max {
		ny = max
	}
	v.SetOrigin(ox, ny)
	return nil
}

// ---- job-input textarea -------------------------------------------------

// (currentJobID helper removed: the three input-handler keys
// (Ctrl-D / Ctrl-F / Ctrl-A) all resolve their target via
// jobInputOwner (with a state.selectedJob fallback for the
// no-draft case), so no helper currently reads "just the
// selected job's id". The doc comments on liveDispatch /
// liveAbort still reference it by name to explain WHY the
// shorthand path was rejected.)

// liveEditor is the gocui editor on the job-input view. Defers to
// the default editor for most keys; Ctrl-* shortcuts are caught by
// SetKeybinding before they reach this editor.
//
// jobInputOwner is stamped on the FIRST keystroke only (when it's
// empty), so the draft's authored target is sticky for the lifetime
// of the draft. Stamping on EVERY keystroke would silently re-
// attribute the buffer to whatever job the cursor now points at —
// which after a refresh-driven reshuffle of the jobs slice can be
// a different job than the one the operator originally typed for.
// The ownership-clear paths (clearJobInputIfStale, liveDispatch,
// jobInputEscape) all reset jobInputOwner alongside jobInput, so
// empty-owner ≡ empty-buffer is the canonical "no draft" state
// the next keystroke can safely claim.
func (gu *Gui) liveEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) bool {
	handled := gocui.DefaultEditor.Edit(v, key, ch, mod)
	gu.st.withLock(func() {
		gu.st.sessionInput = v.Buffer()
		if gu.st.sessionInputOwner == "" {
			if s, ok := gu.st.selectedSession(); ok {
				gu.st.sessionInputOwner = s.ID
			}
		}
	})
	return handled
}

func (gu *Gui) liveSend(_ *gocui.Gui, v *gocui.View) error {
	return gu.liveDispatch(v, "steer")
}

func (gu *Gui) liveFollowUp(_ *gocui.Gui, v *gocui.View) error {
	return gu.liveDispatch(v, "followup")
}

// liveAbort routes Ctrl-A to the same target liveDispatch uses:
// jobInputOwner (the job that authored the draft), falling back
// to the current cursor when no draft is in flight.
//
// The three input-handler keys (Ctrl-D steer, Ctrl-F followup,
// Ctrl-A abort) MUST be symmetric on target selection: a refresh-
// driven reshuffle of the jobs slice silently shifts the cursor's
// underlying jobID without closing the input view, so reading
// currentJobID() here would let Ctrl-A abort the wrong job
// (typically a brand-new one inserted at index 0). jobInputOwner
// is the operator-authored target the textarea is dispatching
// against; abort follows the same anchor.
//
// The fallback covers the no-draft case (operator opens the input
// and immediately presses Ctrl-A): jobInputOwner is only stamped
// on the first keystroke, so an empty-owner state falls through
// to the current cursor unchanged.
func (gu *Gui) liveAbort(_ *gocui.Gui, _ *gocui.View) error {
	var sessionID string
	gu.st.withRLock(func() {
		if gu.st.sessionInputOwner != "" {
			sessionID = gu.st.sessionInputOwner
			return
		}
		if s, ok := gu.st.selectedSession(); ok {
			sessionID = s.ID
		}
	})
	if sessionID == "" {
		return nil
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		if err := gu.ds.AbortSession(gu.ctx, sessionID); err != nil {
			gu.flashf("err", "abort: %v", err)
			return nil
		}
		gu.flashf("info", "abort sent")
		return nil
	})
	return nil
}

// liveDispatch routes the buffered draft to the job that authored
// it (state.jobInputOwner), NOT the job under the cursor right now.
// The two can drift after a refresh-driven reshuffle of the jobs
// slice: applyRefreshLocked deliberately preserves the draft across
// such shifts (see refresh.go's Note: block), so reading
// currentSessionID() here would silently dispatch text typed against
// session-A to whatever session ended up under the cursor after the
// reshuffle. sessionInputOwner is the authored target the operator
// actually typed against.
//
// Fallback: if sessionInputOwner is empty (no draft yet — the textarea
// is empty so this Ctrl-D / Ctrl-F is a no-op anyway), we fall
// through to the current cursor as a defensive default.
func (gu *Gui) liveDispatch(v *gocui.View, behavior string) error {
	var sessionID string
	gu.st.withRLock(func() {
		if gu.st.sessionInputOwner != "" {
			sessionID = gu.st.sessionInputOwner
			return
		}
		if s, ok := gu.st.selectedSession(); ok {
			sessionID = s.ID
		}
	})
	if sessionID == "" {
		return nil
	}
	msg := strings.TrimRight(v.Buffer(), "\n")
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	// ClearTextArea (not Clear) scrubs BOTH v.lines and v.TextArea —
	// SimpleEditor's TypeCharacter writes into TextArea, then
	// RenderTextArea re-rasterises TextArea → v.lines on every
	// keystroke. v.Clear() alone leaves TextArea populated, so the
	// next keystroke would re-materialise the just-dispatched
	// message into the visible buffer.
	v.ClearTextArea()
	gu.st.withLock(func() {
		gu.st.sessionInput = ""
		gu.st.sessionInputOwner = ""
	})
	gu.g.OnWorker(func(_ gocui.Task) error {
		err := gu.ds.SessionInput(gu.ctx, sessionID, msg, behavior)
		if err != nil {
			gu.flashf("err", "send: %v", err)
			return nil
		}
		gu.flashf("info", "message sent")
		return nil
	})
	return nil
}

// clearSessionInputIfStale wipes the cached sessionInput buffer when its
// recorded owner (sessionInputOwner) no longer matches the currently-
// selected session.
//
// Called from afterCursorMove(panelSessions) on explicit j/k cursor
// moves — the only authored event we want anchoring the clear.
// Refresh-driven cursor shifts (e.g. a new session inserts at index 0
// and pushes the previously-cursored row to index 1) intentionally
// preserve the draft; see applyRefreshLocked's `Note:` block for
// the rationale.
//
// The view's Buffer() is also cleared so the next layout pass
// doesn't read the stale content back out of gocui's internal
// buffer (writeView seeds it from gu.st.sessionInput on entry, but
// gocui keeps the View across layouts unless we DeleteView).
//
// MUST be called WITHOUT holding state.mu — we acquire it inside.
func (gu *Gui) clearSessionInputIfStale(curSessionID string) {
	var cleared bool
	gu.st.withLock(func() {
		if gu.st.sessionInputOwner != "" && gu.st.sessionInputOwner != curSessionID {
			gu.st.sessionInput = ""
			gu.st.sessionInputOwner = ""
			cleared = true
		}
	})
	if !cleared || gu.g == nil {
		return
	}
	if v, err := gu.g.View(winSessionInput); err == nil && v != nil {
		// ClearTextArea (not Clear) — see liveDispatch for the
		// rationale. v.Clear() alone would leave the editor's
		// internal TextArea populated and the next keystroke on
		// the new session would re-materialise session-A's draft into the
		// visible buffer.
		v.ClearTextArea()
	}
}

// sessionInputEscape is bound to Esc on winSessionInput. It moves focus back
// to the Sessions panel and clears the cached buffer so a returning user
// doesn't see leftover text from a previous compose session.
func (gu *Gui) sessionInputEscape(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() {
		gu.st.sessionInput = ""
		gu.st.sessionInputOwner = ""
		gu.st.focused = panelSessions
	})
	// Clear the view's TextArea / buffer too so the next layout pass
	// doesn't pull the previous content back out. ClearTextArea
	// scrubs both v.lines and v.TextArea (see liveDispatch); v.Clear()
	// alone would leak the previous draft on the next keystroke when
	// the operator returns to the same running job.
	if v, err := gu.g.View(winSessionInput); err == nil && v != nil {
		v.ClearTextArea()
	}
	return nil
}
