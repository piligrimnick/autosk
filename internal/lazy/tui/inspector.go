package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/timeformat"
)

// openInspector switches viewState to Inspector for the given job.
// Default tab is computed from the job's terminal status (Archive for
// terminal runs, Live otherwise).
func (gu *Gui) openInspector(jobID string) {
	gu.g.OnWorker(func(_ gocui.Task) error {
		j, err := gu.ds.GetJob(gu.ctx, jobID)
		if err != nil {
			gu.flashf("err", "open inspector: %v", err)
			return nil
		}
		tab := tabLive
		terminal := runstore.RunStatus(j.Status).IsTerminal()
		if terminal {
			tab = tabArchive
		}
		gu.openInspectorAtTabWithJob(j, tab, terminal)
		return nil
	})
}

func (gu *Gui) openInspectorAtTab(jobID string, tab inspectorTab) {
	gu.g.OnWorker(func(_ gocui.Task) error {
		j, err := gu.ds.GetJob(gu.ctx, jobID)
		if err != nil {
			gu.flashf("err", "open inspector: %v", err)
			return nil
		}
		terminal := runstore.RunStatus(j.Status).IsTerminal()
		gu.openInspectorAtTabWithJob(j, tab, terminal)
		return nil
	})
}

func (gu *Gui) openInspectorAtTabWithJob(j datasource.Job, tab inspectorTab, terminalAtOpen bool) {
	// Tear down any prior live stream BEFORE we overwrite st.insp.
	// Without this the old liveCancel reference is lost when the
	// inspectorState is replaced and the old pumpLive goroutine keeps
	// running — worse, its g.Update closure reads `gu.st.insp.liveBuf`
	// fresh on every flush, so events from the previous job end up
	// appended to the new job's buffer.
	gu.stopLiveStream()
	gu.g.Update(func(_ *gocui.Gui) error {
		gu.st.withLock(func() {
			gu.st.view = StateInspector
			gu.st.insp = inspectorState{
				JobID:          j.JobID,
				Tab:            tab,
				Job:            j,
				Streaming:      j.Streaming,
				TerminalAtOpen: terminalAtOpen,
			}
		})
		return nil
	})
	// Hydrate the chosen tab's data. Pass j.JobID explicitly: g.Update
	// above is asynchronous (per gocui docs the userEvents queue order
	// is not guaranteed wrt OnWorker goroutines), so reading st.insp.JobID
	// inside the worker races against the inspectorState assignment and
	// loses often enough in practice that hydration silently bails —
	// the Live tab never opens its SSE subscription ("(waiting for
	// events…)" forever) and the Archive tab never flips archiveLoaded
	// ("(loading…)" forever).
	gu.hydrateInspectorTab(j.JobID, tab)
}

// hydrateInspectorTab fetches the data the current tab needs. jobID is
// passed explicitly so the caller's identity is decoupled from
// `st.insp.JobID` — see the race note on openInspectorAtTabWithJob.
func (gu *Gui) hydrateInspectorTab(jobID string, tab inspectorTab) {
	if jobID == "" {
		return
	}
	gu.g.OnWorker(func(_ gocui.Task) error {
		switch tab {
		case tabArchive:
			// Per-request deadline: the daemon's UDS HTTP client has
			// Timeout=0 (correct for /stream) and gu.ctx has no deadline
			// either, so a daemon stalled reading a fat session.jsonl
			// would otherwise wedge the Archive tab in "(loading…)"
			// indefinitely. Mirror refreshAll's 5×Refresh budget.
			ctx, cancel := hydrateContext(gu.ctx, gu.opts.Refresh)
			evs, err := gu.ds.Messages(ctx, jobID, true, 0)
			cancel()
			if err != nil {
				gu.flashf("warn", "archive load: %v", err)
			}
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.st.withLock(func() {
					if gu.st.insp.JobID != jobID {
						return // inspector switched out from under us
					}
					gu.st.insp.archive = evs
					gu.st.insp.archiveLoaded = true
					gu.st.insp.archiveErr = err
				})
				return nil
			})
		case tabSignals:
			ctx, cancel := hydrateContext(gu.ctx, gu.opts.Refresh)
			defer cancel()
			t, err := gu.ds.GetJob(ctx, jobID)
			if err != nil {
				gu.flashf("err", "signals: get job %s: %v", jobID, err)
				return nil
			}
			// Design plan §5.5: Signals are scoped to THIS run; passing
			// jobID (not taskID) ensures rows from earlier kickback runs
			// of the same task don't bleed in.
			sigs, serr := gu.ds.Signals(ctx, jobID)
			if serr != nil {
				gu.flashf("warn", "signals: %v", serr)
			}
			cms, cerr := gu.ds.Comments(ctx, t.TaskID)
			if cerr != nil {
				gu.flashf("warn", "comments: %v", cerr)
			}
			// Design plan §5.5: the Signals tab's comments sub-region is
			// scoped to those added "since started_at" so the operator
			// sees "comments observed during THIS run" — not the full
			// task ledger that spans every kickback. Filter client-side
			// (the offline base returns the full slice anyway, so this is
			// the cheapest place to cut). When the run hasn't started yet
			// (queued/dispatched) we don't filter — there's no cutoff to
			// apply, and the operator wants to see the comments that
			// pushed the work into the queue.
			cms = filterCommentsSinceJob(cms, t)
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.st.withLock(func() {
					if gu.st.insp.JobID != jobID {
						return
					}
					gu.st.insp.signals = sigs
					gu.st.insp.comments = cms
				})
				return nil
			})
		case tabLive:
			gu.startLiveStream(jobID)
		case tabMeta:
			// already populated from openInspector
		}
		return nil
	})
}

// hydrateContext returns a context derived from parent with a
// 5×refresh budget (clamped at 5s minimum so a Refresh=0 / very-small
// Refresh doesn't immediately cancel). Mirrors refreshAll's choice so
// inspector hydration honours the same deadline as the dashboard.
func hydrateContext(parent context.Context, refresh time.Duration) (context.Context, context.CancelFunc) {
	budget := 5 * refresh
	if budget < 5*time.Second {
		budget = 5 * time.Second
	}
	return context.WithTimeout(parent, budget)
}

// startLiveStream opens an SSE subscription against jobID. On failure
// we BOTH flash a toast AND append an error line to liveBuf, because
// in StateInspector the dashboard's command-log panel is hidden
// (see arrangement.go:inspectorArrangement) and the flash decays after
// ~4s — without the in-buffer line the user would see only
// "(waiting for events…)" forever and have no idea the stream failed.
func (gu *Gui) startLiveStream(jobID string) {
	gu.stopLiveStream()
	ctx, cancel := context.WithCancel(gu.ctx)
	handle, err := gu.ds.StreamLive(ctx, jobID)
	if err != nil {
		cancel()
		gu.flashf("warn", "live: %v (try Archive)", err)
		line := styleErr.Render(fmt.Sprintf("— stream error: %v (try Archive)", err))
		gu.g.Update(func(_ *gocui.Gui) error {
			gu.st.withLock(func() {
				if gu.st.insp.JobID != jobID {
					return
				}
				gu.st.insp.liveBuf = append(gu.st.insp.liveBuf, line)
			})
			return nil
		})
		return
	}
	gu.st.withLock(func() {
		gu.st.insp.live = handle
		gu.st.insp.liveCancel = cancel
	})
	go gu.pumpLive(ctx, jobID, handle)
}

// stopLiveStream cancels and clears the SSE subscription.
func (gu *Gui) stopLiveStream() {
	var (
		h *datasource.LiveHandle
		c context.CancelFunc
	)
	gu.st.withLock(func() {
		h = gu.st.insp.live
		c = gu.st.insp.liveCancel
		gu.st.insp.live = nil
		gu.st.insp.liveCancel = nil
	})
	if c != nil {
		c()
	}
	if h != nil {
		_ = h.Close()
	}
}

// pumpLive consumes SSE events and appends rendered blocks to the
// inspector's live buffer. Delegates to pumpLiveLoop so the throttle
// behaviour is testable without a real gocui.Gui.
//
// Risk #4 mitigation: coalesce sub-30ms bursts into a single
// g.Update. A chatty pi run can emit 20+ events per turn boundary;
// without throttling the layout fires 20+ times in a row and the
// terminal flickers. Same knob lazygit's pkg/tasks/tasks.go uses.
//
// Ring-buffer cap (remark C): liveBuf is capped at liveBufCap so a
// long pi run doesn't grow the slice without bound. When the cap is
// hit we drop the oldest 25% in one allocation instead of re-slicing
// per event.
func (gu *Gui) pumpLive(ctx context.Context, ownerJobID string, h *datasource.LiveHandle) {
	pumpLiveLoop(ctx, h.Events, livePumpThrottle, func(batch []datasource.LiveEvent) {
		gu.g.Update(func(_ *gocui.Gui) error {
			gu.st.withLock(func() {
				// Owner-check: if the inspector switched jobs between the
				// throttle flush queueing and the main loop applying it,
				// dropping the batch is safer than appending stale events
				// into someone else's transcript. Same defense as the
				// hydrate paths above.
				if gu.st.insp.JobID != ownerJobID {
					return
				}
				for _, ev := range batch {
					line := renderLiveEvent(ev)
					gu.st.insp.liveBuf = append(gu.st.insp.liveBuf, line)
					if ev.Kind == "status" || ev.Kind == "done" {
						gu.st.insp.Streaming = ev.Status.Streaming
						gu.st.insp.Job.JobResponse = ev.Status
					}
				}
				if n := len(gu.st.insp.liveBuf); n > liveBufCap {
					keep := liveBufCap - liveBufCap/4 // drop oldest 25%
					newBuf := make([]string, keep)
					copy(newBuf, gu.st.insp.liveBuf[n-keep:])
					gu.st.insp.liveBuf = newBuf
					gu.st.insp.liveTruncated = true
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

// liveBufCap is the soft cap on the inspector Live tab's pre-rendered
// transcript. ~2000 events is enough for an hour of pi traffic; over
// the cap we drop oldest 25% in one allocation.
const liveBufCap = 2000

func renderLiveEvent(ev datasource.LiveEvent) string {
	switch ev.Kind {
	case "message":
		return renderTranscript([]datasource.MessageEvent{ev.Message})
	case "status":
		return styleMuted.Render(fmt.Sprintf("— status: %s streaming=%v attached=%d",
			ev.Status.Status, ev.Status.Streaming, ev.Status.AttachCount))
	case "done":
		return styleAccent.Render(fmt.Sprintf("— done: %s", ev.Status.Status))
	case "error":
		return styleErr.Render(fmt.Sprintf("— error: %v", ev.Err))
	}
	return ""
}

// filterCommentsSinceJob returns the subset of cms whose CreatedAt
// is at or after j.StartedAt. When j.StartedAt is nil (the run
// hasn't started yet) the full slice is returned — there's no
// cutoff to apply and the design intent ("comments observed during
// this run") is degenerate.
func filterCommentsSinceJob(cms []datasource.Comment, j datasource.Job) []datasource.Comment {
	if j.StartedAt == nil || j.StartedAt.IsZero() {
		return cms
	}
	cutoff := *j.StartedAt
	out := make([]datasource.Comment, 0, len(cms))
	for _, c := range cms {
		if c.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// currentJobID returns the inspector's current job id.
func (gu *Gui) currentJobID() string {
	var id string
	gu.st.withRLock(func() { id = gu.st.insp.JobID })
	return id
}

// inspectorClose pops back to the dashboard and tears down Live.
func (gu *Gui) inspectorClose(*gocui.Gui, *gocui.View) error {
	gu.stopLiveStream()
	gu.st.withLock(func() {
		gu.st.view = StateDashboard
		gu.st.insp = inspectorState{}
	})
	return nil
}

// nextTab returns the inspector tab arrived at by walking `step`
// positions from `cur` (wraps modulo the number of tabs). Extracted
// from inspectorCycleTab so the modulo math is reachable from a
// test that doesn't need a real gocui.Gui.
//
// Step is normalised to (-n, n) before the wrap-add so values with
// |step| > n (e.g. someone passing a multi-page jump) still land in
// the right slot — Go's % preserves the sign of the dividend, so a
// naive (cur+step+n)%n loses for step <= -n.
func nextTab(cur inspectorTab, step int) inspectorTab {
	const n = 4
	s := step % n
	return inspectorTab((int(cur) + s + n) % n)
}

// inspectorCycleTab cycles inspector tab by step (±1).
func (gu *Gui) inspectorCycleTab(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var (
			newTab inspectorTab
			jobID  string
		)
		gu.st.withLock(func() {
			gu.st.insp.Tab = nextTab(gu.st.insp.Tab, step)
			newTab = gu.st.insp.Tab
			jobID = gu.st.insp.JobID
		})
		gu.hydrateInspectorTab(jobID, newTab)
		return nil
	}
}

func (gu *Gui) inspectorJumpTab(t inspectorTab) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var jobID string
		gu.st.withLock(func() {
			gu.st.insp.Tab = t
			jobID = gu.st.insp.JobID
		})
		gu.hydrateInspectorTab(jobID, t)
		return nil
	}
}

// inspectorScroll moves the inspector body view's origin by step
// lines. Lazygit's pattern: use the view's own origin so gocui draws
// the right window of the underlying content buffer. Clamped at
// both ends so j-spam past the last line doesn't push content off
// the top of an empty screen (the previous implementation only
// guarded ny < 0).
func (gu *Gui) inspectorScroll(step int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		return gu.scrollViewByLines(winInspector, step)
	}
}

// inspectorScrollPage scrolls by one screen of the body view.
func (gu *Gui) inspectorScrollPage(dir int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winInspector)
		if err != nil || v == nil {
			return nil
		}
		_, h := v.Size()
		if h < 1 {
			h = 1
		}
		return gu.scrollViewByLines(winInspector, dir*h)
	}
}

// scrollViewByLines moves the named view's origin by step lines,
// clamping to [0, max(0, totalLines-height)]. Shared by inspectorScroll
// and inspectorScrollPage so the upper-bound clamp lives in one place.
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

// inspectorScrollTo jumps to the top (g) or bottom (G) of the body.
func (gu *Gui) inspectorScrollTo(bottom bool) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winInspector)
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

// detailScroll moves the Tasks-detail viewport up/down one line at a
// time. Design plan: J/K on the detail pane.
func (gu *Gui) detailScroll(step int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winDetail)
		if err != nil || v == nil {
			return nil
		}
		ox, oy := v.Origin()
		ny := oy + step
		if ny < 0 {
			ny = 0
		}
		v.SetOrigin(ox, ny)
		return nil
	}
}

// renderInspectorBody renders the body for the active tab.
func (gu *Gui) renderInspectorBody() string {
	var (
		tab  inspectorTab
		insp inspectorState
	)
	gu.st.withRLock(func() {
		tab = gu.st.insp.Tab
		insp = gu.st.insp
	})
	switch tab {
	case tabLive:
		body := strings.Join(insp.liveBuf, "")
		if body == "" {
			body = styleMuted.Render("(waiting for events…  Ctrl-D send · Ctrl-F follow_up · Ctrl-A abort · Esc back)")
		}
		if insp.liveTruncated {
			body = styleMuted.Render("(transcript truncated; older events dropped to cap memory)") + "\n" + body
		}
		return body
	case tabArchive:
		if !insp.archiveLoaded {
			return styleMuted.Render("(loading…)")
		}
		if insp.archiveErr != nil {
			return styleErr.Render(fmt.Sprintf("(archive load failed: %v)", insp.archiveErr))
		}
		body := renderTranscript(insp.archive)
		if body == "" {
			body = styleMuted.Render("(no transcript events)")
		}
		return body
	case tabMeta:
		return renderJobDetail(insp.Job)
	case tabSignals:
		return renderInspectorSignals(insp.signals, insp.comments)
	}
	return ""
}

func renderInspectorSignals(sigs []datasource.Signal, comments []datasource.Comment) string {
	var b strings.Builder
	b.WriteString(styleHeader.Render("step signals") + "\n")
	if len(sigs) == 0 {
		b.WriteString(styleMuted.Render("(no signals)") + "\n")
	} else {
		for _, s := range sigs {
			// Full local DateTime so a kickback loop straddling midnight
			// (or a run from yesterday opened today) is readable. A
			// time-only stamp would lose that context. Entity-coloured
			// columns let the operator scan signals by hue: purple
			// step → purple-or-status target, cyan agent in parens.
			fmt.Fprintf(&b, "  %s  %s → %s  (%s)\n",
				timeformat.FormatDateTime(s.CreatedAt),
				renderStepName(s.StepName),
				renderSignalTarget(s.Target),
				renderAgentName(s.AgentName))
		}
	}
	b.WriteString("\n" + styleHeader.Render("comments") + "\n")
	if len(comments) == 0 {
		b.WriteString(styleMuted.Render("(no comments)") + "\n")
	} else {
		for _, c := range comments {
			fmt.Fprintf(&b, "  %s  %s: %s\n",
				timeformat.FormatDateTime(c.CreatedAt),
				renderAgentName(c.AuthorName),
				truncate(c.Text, 80))
		}
	}
	return b.String()
}

// renderSignalTarget colours a step_signals.target value: a target
// that names a sibling step gets the StepName hue (purple); a
// lifecycle terminal (done / cancel / human) gets its task-status
// hue so the kickback chain reads as "step → status" with each half
// visually grounded in its panel counterpart.
func renderSignalTarget(target string) string {
	if target == "" {
		return ""
	}
	switch store.Status(target) {
	case store.StatusDone, store.StatusCancel, store.StatusHuman:
		return styleForTaskStatus(store.Status(target)).Render(target)
	}
	return renderStepName(target)
}

// ---- Live tab textarea --------------------------------------------------

// liveEditor is the gocui editor on the live-input view. Defers to the
// default editor for most keys; Enter inserts \n; Ctrl-* are caught
// via SetKeybinding so they don't actually reach this editor.
func (gu *Gui) liveEditor(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) bool {
	handled := gocui.DefaultEditor.Edit(v, key, ch, mod)
	gu.st.withLock(func() { gu.st.insp.liveInput = v.Buffer() })
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
	gu.st.withLock(func() { gu.st.insp.liveInput = "" })
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
