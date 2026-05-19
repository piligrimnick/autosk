package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/runstore"
	"autosk/internal/lazy/datasource"
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
	// Hydrate the chosen tab's data.
	gu.hydrateInspectorTab(tab)
}

// hydrateInspectorTab fetches the data the current tab needs.
func (gu *Gui) hydrateInspectorTab(tab inspectorTab) {
	gu.g.OnWorker(func(_ gocui.Task) error {
		jobID := gu.currentJobID()
		if jobID == "" {
			return nil
		}
		switch tab {
		case tabArchive:
			evs, err := gu.ds.Messages(gu.ctx, jobID, true, 0)
			if err != nil {
				gu.flashf("warn", "archive load: %v", err)
				return nil
			}
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.st.withLock(func() {
					gu.st.insp.archive = evs
					gu.st.insp.archiveLoaded = true
				})
				return nil
			})
		case tabSignals:
			t, err := gu.ds.GetJob(gu.ctx, jobID)
			if err != nil {
				gu.flashf("err", "signals: get job %s: %v", jobID, err)
				return nil
			}
			// Design plan §5.5: Signals are scoped to THIS run; passing
			// jobID (not taskID) ensures rows from earlier kickback runs
			// of the same task don't bleed in.
			sigs, serr := gu.ds.Signals(gu.ctx, jobID)
			if serr != nil {
				gu.flashf("warn", "signals: %v", serr)
			}
			cms, cerr := gu.ds.Comments(gu.ctx, t.TaskID)
			if cerr != nil {
				gu.flashf("warn", "comments: %v", cerr)
			}
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.st.withLock(func() {
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

// startLiveStream opens an SSE subscription against jobID.
func (gu *Gui) startLiveStream(jobID string) {
	gu.stopLiveStream()
	ctx, cancel := context.WithCancel(gu.ctx)
	handle, err := gu.ds.StreamLive(ctx, jobID)
	if err != nil {
		cancel()
		gu.flashf("warn", "live: %v (try Archive)", err)
		return
	}
	gu.st.withLock(func() {
		gu.st.insp.live = handle
		gu.st.insp.liveCancel = cancel
	})
	go gu.pumpLive(ctx, handle)
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
func (gu *Gui) pumpLive(ctx context.Context, h *datasource.LiveHandle) {
	pumpLiveLoop(ctx, h.Events, livePumpThrottle, func(batch []datasource.LiveEvent) {
		gu.g.Update(func(_ *gocui.Gui) error {
			gu.st.withLock(func() {
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
		var newTab inspectorTab
		gu.st.withLock(func() {
			gu.st.insp.Tab = nextTab(gu.st.insp.Tab, step)
			newTab = gu.st.insp.Tab
		})
		gu.hydrateInspectorTab(newTab)
		return nil
	}
}

func (gu *Gui) inspectorJumpTab(t inspectorTab) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() { gu.st.insp.Tab = t })
		gu.hydrateInspectorTab(t)
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
		tab    inspectorTab
		insp   inspectorState
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
			// RFC3339 carries the date so a kickback loop straddling
			// midnight (or a run from yesterday opened today) is
			// readable. 15:04:05-only timestamps lose that context.
			fmt.Fprintf(&b, "  %s  %s → %s  (%s)\n",
				s.CreatedAt.Format(time.RFC3339), s.StepName, s.Target, s.AgentName)
		}
	}
	b.WriteString("\n" + styleHeader.Render("comments") + "\n")
	if len(comments) == 0 {
		b.WriteString(styleMuted.Render("(no comments)") + "\n")
	} else {
		for _, c := range comments {
			fmt.Fprintf(&b, "  %s  %s: %s\n",
				c.CreatedAt.Format(time.RFC3339), c.AuthorName, truncate(c.Text, 80))
		}
	}
	return b.String()
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
