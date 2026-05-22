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
	var (
		sameJob   bool
		hasHandle bool
	)
	gu.st.withLock(func() {
		if gu.st.jobLiveTimer != nil {
			gu.st.jobLiveTimer.Stop()
			gu.st.jobLiveTimer = nil
		}
		sameJob = gu.st.jobLiveJobID == jobID
		hasHandle = gu.st.jobLiveHandle != nil
	})
	if sameJob && hasHandle {
		// Already streaming the right job — nothing to do.
		return
	}
	// Arm the debounce. AfterFunc runs the callback on its own
	// goroutine when the timer fires.
	t := time.AfterFunc(jobLiveDebounce, func() { gu.openJobLive(jobID) })
	gu.st.withLock(func() { gu.st.jobLiveTimer = t })
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
						te.runningSeen = ev.Status.Streaming ||
							runstore.RunStatus(ev.Status.Status) == runstore.StatusRunning
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

// loadJobArchive fetches the archive for jobID and merges it into the
// per-job transcript cache. Always runs on a worker goroutine.
//
// Cache policy:
//   - Hit + fresh (terminal job within TTL OR running job): no-op.
//   - Hit + stale (terminal job past TTL): refetch.
//   - Miss: fetch and seed the entry.
//
// Merge policy: the archive snapshot replaces the prefix; any live
// events whose TS strictly post-dates the archive's last TS are
// preserved so a slow Messages() response doesn't lose the live tail.
func (gu *Gui) loadJobArchive(jobID string) {
	if jobID == "" {
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
			te := gu.ensureTranscriptEntryLocked(jobID)
			te.err = err
			te.loadedAt = time.Now()
			if err == nil {
				contentW := gu.innerWidth(winDetail) - 4
				if contentW < 1 {
					contentW = 1
				}
				te.events = mergeArchiveAndLive(evs, te.events)
				rebuildTranscriptBoxes(te, contentW)
			}
		})
		return nil
	})
}

// scheduleJobArchive queues loadJobArchive for jobID on the worker
// pool, coalesced through a per-jobID inflight set so back-to-back
// cursor moves on the same job don't pile on requests.
//
// Coalesce behaviour: while a load is in flight for jobID, additional
// calls flip the "pending" bit but don't spawn a new worker. When the
// active worker finishes it checks the pending bit and re-runs once
// more — same trailing-edge pattern scheduleRefreshWith uses. The
// pending set lives in state.jobArchiveInFlight (initialised lazily).
func (gu *Gui) scheduleJobArchive(jobID string, running bool) {
	if jobID == "" {
		return
	}
	// Decide up-front whether to schedule based on cache freshness.
	var fetch bool
	gu.st.withLock(func() {
		te, ok := gu.st.jobTranscript[jobID]
		if !ok {
			fetch = true
			return
		}
		// Stale entry: re-fetch.
		if te.loadedAt.IsZero() {
			fetch = true
			return
		}
		if !running {
			// Terminal: honour TTL.
			if time.Since(te.loadedAt) > jobTranscriptTerminalTTL {
				fetch = true
			}
		}
	})
	if !fetch {
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
func (gu *Gui) detailScrollPage(dir int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winDetail)
		if err != nil || v == nil {
			return nil
		}
		_, h := v.Size()
		if h < 1 {
			h = 1
		}
		return gu.scrollViewByLines(winDetail, dir*h)
	}
}

// detailScrollTo jumps to the top (g) or bottom (G) of the detail
// pane.
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
		_, h := v.Size()
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
// clamping to [0, max(0, totalLines-height)].
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
	_, h := v.Size()
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
func (gu *Gui) liveEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) bool {
	handled := gocui.DefaultEditor.Edit(v, key, ch, mod)
	gu.st.withLock(func() { gu.st.jobInput = v.Buffer() })
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

func (gu *Gui) liveDispatch(v *gocui.View, behavior string) error {
	jobID := gu.currentJobID()
	if jobID == "" {
		return nil
	}
	msg := strings.TrimRight(v.Buffer(), "\n")
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	v.Clear()
	v.SetCursor(0, 0)
	gu.st.withLock(func() { gu.st.jobInput = "" })
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

// jobInputEscape is bound to Esc on winJobInput. It moves focus back
// to the Jobs panel and clears the cached buffer so a returning user
// doesn't see leftover text from a previous compose session.
func (gu *Gui) jobInputEscape(*gocui.Gui, *gocui.View) error {
	gu.st.withLock(func() {
		gu.st.jobInput = ""
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
