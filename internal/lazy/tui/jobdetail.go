// Package tui — jobdetail.go is the Detail-pane side of the
// post-Inspector world: it owns the per-job SSE subscription
// lifecycle, the input-textarea editor + dispatch handlers, and the
// scroll helpers shared by Job Detail and Task Detail. The
// per-event cache + box rendering lives in jobtranscript.go.

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

// isJobRunning reports whether j is currently in the running enum
// state. Shared by layout / scheduleJobLive / renderViews so the
// "Live input visible" predicate has exactly one definition.
func isJobRunning(j datasource.Job) bool {
	return runstore.RunStatus(j.Status) == runstore.StatusRunning
}

// ---- SSE lifecycle ------------------------------------------------------

// scheduleJobLive (re)arms the debounce timer for opening an SSE
// subscription on jobID. Calling this on every cursor move + every
// refresh-driven status update keeps the timer pinned to the latest
// selection, so a burst of j/k presses collapses into one StreamLive
// call against the final-resting cursor row jobLiveDebounce after
// the last keystroke.
//
// Behaviour:
//   - jobID == "" OR !running: cancels any pending timer AND any
//     active handle. Terminal jobs don't stream.
//   - jobID == st.jobLiveJobID AND a handle already exists: cancels
//     the timer and is a no-op otherwise.
//   - Anything else: cancels timer + (eventually) active handle,
//     arms a new timer pointing at openJobLive(jobID).
func (gu *Gui) scheduleJobLive(jobID string, running bool) {
	if jobID == "" || !running {
		gu.stopJobLive()
		return
	}
	// Stop any pending timer + arm the new one in ONE critical
	// section so a concurrent scheduleJobLive on a different job
	// can't slip its timer between our Stop and our Set —
	// time.AfterFunc is non-blocking, so it's safe to call under
	// the lock.
	gu.st.withLock(func() {
		if gu.st.jobLiveTimer != nil {
			gu.st.jobLiveTimer.Stop()
			gu.st.jobLiveTimer = nil
		}
		if gu.st.jobLiveJobID == jobID && gu.st.jobLiveHandle != nil {
			// Already streaming the right job — nothing to do.
			return
		}
		gu.st.jobLiveTimer = time.AfterFunc(jobLiveDebounce, func() { gu.openJobLive(jobID) })
	})
}

// openJobLive opens the SSE subscription for jobID after the debounce
// fires. Runs on the timer goroutine; re-checks selection so a cursor
// move that overlapped the timer doesn't open a stream for a
// no-longer-selected job.
func (gu *Gui) openJobLive(jobID string) {
	// Selection re-check: if the cursor no longer points at jobID OR
	// the job has gone terminal, abort.
	var keep bool
	var hadHandle bool
	gu.st.withRLock(func() {
		if j, ok := gu.st.selectedJob(); ok && j.JobID == jobID && isJobRunning(j) {
			keep = true
		}
		hadHandle = gu.st.jobLiveJobID == jobID && gu.st.jobLiveHandle != nil
	})
	if !keep {
		return
	}
	if hadHandle {
		return
	}

	// Cancel previous handle, then open the new one. We open BEFORE
	// taking the write lock so the StreamLive HTTP roundtrip
	// doesn't block other state writes.
	gu.stopJobLive()
	ctx, cancel := context.WithCancel(gu.ctx)
	handle, err := gu.ds.StreamLive(ctx, jobID)
	if err != nil {
		cancel()
		gu.flashf("warn", "live: %v", err)
		return
	}
	gu.st.withLock(func() {
		gu.st.jobLiveJobID = jobID
		gu.st.jobLiveHandle = handle
		gu.st.jobLiveCancel = cancel
	})
	go gu.pumpJobLive(ctx, jobID, handle)
}

// stopJobLive cancels the debounce timer + active SSE handle. Safe
// to call when nothing is active. Called from quit, from
// afterCursorMove when cursor leaves panelJobs / lands on a terminal
// job, and from applyRefreshLocked when the streamed job vanishes
// from the jobs slice.
func (gu *Gui) stopJobLive() {
	if gu == nil || gu.st == nil {
		return
	}
	var (
		h *datasource.LiveHandle
		c context.CancelFunc
		t *time.Timer
	)
	gu.st.withLock(func() {
		h = gu.st.jobLiveHandle
		c = gu.st.jobLiveCancel
		t = gu.st.jobLiveTimer
		gu.st.jobLiveHandle = nil
		gu.st.jobLiveCancel = nil
		gu.st.jobLiveJobID = ""
		gu.st.jobLiveTimer = nil
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

// pumpJobLive consumes SSE events for jobID and appends them to the
// per-job transcript cache. Mirrors the previous Inspector pumpLive
// but routes events into state.jobTranscript (a per-jobID cache of
// rendered boxes) instead of the inspector's flat liveBuf slice.
//
// Risk #4 mitigation: coalesce sub-30ms bursts into a single
// g.Update via pumpLiveLoop. Owner-check on flush guards against the
// transcript-cache being switched out from under us between throttle
// queue and apply.
func (gu *Gui) pumpJobLive(ctx context.Context, ownerJobID string, h *datasource.LiveHandle) {
	pumpLiveLoop(ctx, h.Events, livePumpThrottle, func(batch []datasource.LiveEvent) {
		gu.g.Update(func(_ *gocui.Gui) error {
			gu.st.withLock(func() {
				if gu.st.jobLiveJobID != ownerJobID {
					return
				}
				te := gu.ensureTranscriptEntryLocked(ownerJobID)
				contentW := gu.innerWidth(winDetail) - 4
				if contentW < 1 {
					contentW = 1
				}
				for _, ev := range batch {
					switch ev.Kind {
					case "message":
						appendTranscriptEvent(te, ev.Message, contentW)
					case "status", "done":
						// Status / done envelopes don't carry transcript
						// content; the running→terminal transition is
						// detected in applyRefreshLocked via the
						// prevJob/cur comparison. We just ignore them
						// here.
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
//   - Hit, terminal   → stale only if older than jobTranscriptTerminalTTL.
func (gu *Gui) jobArchiveStale(jobID string, running bool) bool {
	var stale bool
	gu.st.withLock(func() {
		te, ok := gu.st.jobTranscript[jobID]
		if !ok {
			stale = true
			return
		}
		if te.loadedAt.IsZero() {
			stale = true
			return
		}
		if !running && time.Since(te.loadedAt) > jobTranscriptTerminalTTL {
			stale = true
		}
	})
	return stale
}

// loadJobArchive fetches the archive for jobID and merges it into the
// per-job transcript cache. Always runs on a worker goroutine.
//
// Coalesce: re-checks jobArchiveStale at the top under the lock. If
// another worker just populated the entry between the scheduler's
// decision and this worker actually running, we bail without a
// duplicate Messages() call. The freshness check is the same
// predicate scheduleJobArchive uses, so the contract
// ("back-to-back schedules don't pile on requests") holds even
// when worker_2 is enqueued before worker_1 returns: worker_2 sees
// loadedAt set and short-circuits.
//
// Merge policy: the archive snapshot replaces the prefix; any live
// events whose TS strictly post-dates the archive's last TS are
// preserved so a slow Messages() response doesn't lose the live tail.
func (gu *Gui) loadJobArchive(jobID string) {
	if jobID == "" {
		return
	}
	// Determine the job's running state so the staleness predicate
	// can honour the running-vs-terminal-TTL split. A missing job
	// (e.g. cursor moved away during scheduling) treats running=false,
	// which keeps the TTL gate active.
	running := false
	gu.st.withRLock(func() {
		for i := range gu.st.jobs {
			if gu.st.jobs[i].JobID == jobID {
				running = isJobRunning(gu.st.jobs[i])
				return
			}
		}
	})
	if !gu.jobArchiveStale(jobID, running) {
		return
	}
	ctx, cancel := hydrateContext(gu.ctx, gu.opts.Refresh)
	defer cancel()
	evs, err := gu.ds.Messages(ctx, jobID, true, 0)
	if err != nil {
		gu.flashf("warn", "archive load: %v", err)
	}
	gu.g.Update(func(_ *gocui.Gui) error {
		gu.st.withLock(func() {
			gu.applyArchiveLoadLocked(jobID, evs, err)
		})
		return nil
	})
}

// applyArchiveLoadLocked merges a freshly-fetched archive snapshot
// into the per-job transcript cache and clears the per-load flags.
// MUST be called with state.mu held — the "Locked" suffix mirrors
// the rest of the helpers in this file.
//
// Pulled out of loadJobArchive so tests can drive the post-fetch
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
func (gu *Gui) applyArchiveLoadLocked(jobID string, evs []datasource.MessageEvent, fetchErr error) {
	te := gu.ensureTranscriptEntryLocked(jobID)
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
	// A fresh archive carries the FULL event set (we fetched with
	// full=true / limit=0), so any prior live-cap truncation no
	// longer applies — drop the stale flag so renderJobDetail
	// stops prepending the "(transcript truncated...)" note.
	te.truncated = false
	rebuildTranscriptBoxes(te, contentW)
}

// scheduleJobArchive queues loadJobArchive for jobID on the worker
// pool. loadJobArchive itself re-checks jobArchiveStale at the top
// (under the model lock) so back-to-back schedules that overlap a
// running worker collapse into one actual fetch.
func (gu *Gui) scheduleJobArchive(jobID string, running bool) {
	if jobID == "" {
		return
	}
	if !gu.jobArchiveStale(jobID, running) {
		return
	}
	if gu.g == nil {
		// Tests with no real gocui drive loadJobArchive directly; the
		// scheduler is a no-op without a worker pool.
		return
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		gu.loadJobArchive(jobID)
		return nil
	})
}

// mergeArchiveAndLive returns archive followed by any live events
// whose TS strictly post-dates archive's last event. When archive is
// empty the live slice is returned verbatim. When live is empty the
// archive is returned. Both inputs may be nil.
func mergeArchiveAndLive(archive, live []datasource.MessageEvent) []datasource.MessageEvent {
	if len(live) == 0 {
		return archive
	}
	if len(archive) == 0 {
		return live
	}
	lastTS := archive[len(archive)-1].TS
	tail := make([]datasource.MessageEvent, 0, len(live))
	for _, ev := range live {
		if ev.TS.After(lastTS) {
			tail = append(tail, ev)
		}
	}
	if len(tail) == 0 {
		return archive
	}
	out := make([]datasource.MessageEvent, 0, len(archive)+len(tail))
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
		v, err := gu.g.View(winDetail)
		if err != nil || v == nil {
			return nil
		}
		_, h := v.InnerSize()
		if h < 1 {
			h = 1
		}
		return gu.scrollViewByLines(winDetail, dir*h)
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
		lines := strings.Count(v.Buffer(), "\n")
		_, h := v.InnerSize()
		if h < 1 {
			h = 1
		}
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
	_, h := v.InnerSize()
	if h < 1 {
		h = 1
	}
	lines := strings.Count(v.Buffer(), "\n")
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

// currentJobID returns the currently-selected job id, or "".
func (gu *Gui) currentJobID() string {
	var id string
	gu.st.withRLock(func() {
		if j, ok := gu.st.selectedJob(); ok {
			id = j.JobID
		}
	})
	return id
}

// liveEditor is the gocui editor on the job-input view. Defers to
// the default editor for most keys; Ctrl-* shortcuts are caught by
// SetKeybinding before they reach this editor.
//
// Every keystroke also stamps jobInputOwner with the currently-
// selected job id, so the model has an authoritative record of
// whose draft this is for the next selection-change-driven clear.
func (gu *Gui) liveEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) bool {
	handled := gocui.DefaultEditor.Edit(v, key, ch, mod)
	owner := gu.currentJobID()
	gu.st.withLock(func() {
		gu.st.jobInput = v.Buffer()
		gu.st.jobInputOwner = owner
	})
	return handled
}

func (gu *Gui) liveSend(_ *gocui.Gui, v *gocui.View) error {
	return gu.liveDispatch(v, "")
}

func (gu *Gui) liveFollowUp(_ *gocui.Gui, v *gocui.View) error {
	return gu.liveDispatch(v, "follow_up")
}

func (gu *Gui) liveAbort(_ *gocui.Gui, _ *gocui.View) error {
	jobID := gu.currentJobID()
	if jobID == "" {
		return nil
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		if err := gu.ds.AbortJob(gu.ctx, jobID); err != nil {
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
// currentJobID() here would silently dispatch text typed against
// job-A to whatever job ended up under the cursor after the
// reshuffle. jobInputOwner is the authored target the operator
// actually typed against.
//
// Fallback: if jobInputOwner is empty (no draft yet — the textarea
// is empty so this Ctrl-D / Ctrl-F is a no-op anyway), we fall
// through to the current cursor as a defensive default.
func (gu *Gui) liveDispatch(v *gocui.View, behavior string) error {
	var jobID string
	gu.st.withRLock(func() {
		if gu.st.jobInputOwner != "" {
			jobID = gu.st.jobInputOwner
			return
		}
		if j, ok := gu.st.selectedJob(); ok {
			jobID = j.JobID
		}
	})
	if jobID == "" {
		return nil
	}
	msg := strings.TrimRight(v.Buffer(), "\n")
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	v.Clear()
	v.SetCursor(0, 0)
	gu.st.withLock(func() {
		gu.st.jobInput = ""
		gu.st.jobInputOwner = ""
	})
	gu.g.OnWorker(func(_ gocui.Task) error {
		disp, err := gu.ds.SendInput(gu.ctx, jobID, msg, behavior)
		if err != nil {
			gu.flashf("err", "send: %v", err)
			return nil
		}
		gu.flashf("info", "dispatched: %s", disp)
		return nil
	})
	return nil
}

// clearJobInputIfStale wipes the cached jobInput buffer when its
// recorded owner (jobInputOwner) no longer matches the currently-
// selected job.
//
// Called from afterCursorMove(panelJobs) on explicit j/k cursor
// moves — the only authored event we want anchoring the clear.
// Refresh-driven cursor shifts (e.g. a new job inserts at index 0
// and pushes the previously-cursored row to index 1) intentionally
// preserve the draft; see applyRefreshLocked's `Note:` block for
// the rationale.
//
// The view's Buffer() is also cleared so the next layout pass
// doesn't read the stale content back out of gocui's internal
// buffer (writeView seeds it from gu.st.jobInput on entry, but
// gocui keeps the View across layouts unless we DeleteView).
//
// MUST be called WITHOUT holding state.mu — we acquire it inside.
func (gu *Gui) clearJobInputIfStale(curJobID string) {
	var cleared bool
	gu.st.withLock(func() {
		if gu.st.jobInputOwner != "" && gu.st.jobInputOwner != curJobID {
			gu.st.jobInput = ""
			gu.st.jobInputOwner = ""
			cleared = true
		}
	})
	if !cleared || gu.g == nil {
		return
	}
	if v, err := gu.g.View(winJobInput); err == nil && v != nil {
		v.Clear()
		v.SetCursor(0, 0)
	}
}

// jobInputEscape is bound to Esc on winJobInput. It moves focus back
// to the Jobs panel and clears the cached buffer so a returning user
// doesn't see leftover text from a previous compose session.
func (gu *Gui) jobInputEscape(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() {
		gu.st.jobInput = ""
		gu.st.jobInputOwner = ""
		gu.st.focused = panelJobs
	})
	// Clear the view's TextArea / buffer too so the next layout pass
	// doesn't pull the previous content back out.
	if v, err := gu.g.View(winJobInput); err == nil && v != nil {
		v.Clear()
		v.SetCursor(0, 0)
	}
	return nil
}
