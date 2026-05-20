// Package tui is the gocui-backed TUI for `autosk lazy`.
//
// The package is small on purpose. Lazygit's split (35+ contexts) is
// overkill for v1 — autosk lazy only needs a 4-panel dashboard, a
// 4-tab inspector, and a handful of popups. The architecture still
// mirrors the lazygit shape (gui + state + arrangement + helpers +
// render) but in fewer files.
//
// The only seam between the TUI and the rest of autosk is
// internal/lazy/datasource. Nothing in this package imports
// internal/store, internal/daemon/client, or internal/workflow
// directly.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
	"autosk/internal/lazy/theme"
)

// Options is the input to Run.
type Options struct {
	Datasource  datasource.Datasource
	ProjectRoot string
	Refresh     time.Duration
	// InitialJob, when non-empty, opens the inspector on that job
	// instead of the dashboard. The dashboard is reachable via Esc.
	InitialJob string
	// Headless makes the gocui screen a tcell simulation screen so
	// the TUI can be driven by tests without a real terminal.
	Headless bool
	Width    int
	Height   int
}

// Gui ties together the gocui main loop, the model, the datasource,
// and the helper goroutines.
type Gui struct {
	g     *gocui.Gui
	st    *state
	ds    datasource.Datasource
	opts  Options
	ctx   context.Context
	cancel context.CancelFunc
	stopRefresh chan struct{}

	// refreshInFlight coalesces redundant refresh requests. j-spam
	// (cursor moves that call applyScope → refresh) used to enqueue
	// one OnWorker job per keypress; now intermediate requests are
	// folded into a single trailing refresh via refreshPending so the
	// latest scope eventually drives the UI (lazygit's RefreshHelper
	// uses the same pattern).
	refreshInFlight atomic.Bool
	refreshPending  atomic.Bool

	// dispatch routes a body onto the gocui worker pool. Defaults to
	// gu.g.OnWorker(...) in production; tests replace it with a plain
	// goroutine spawn so scheduleRefresh's CAS dance can be exercised
	// without a real gocui.Gui.
	dispatch func(func())

	// lastFetchNS is the wall-clock duration of the most recent
	// fetchRefresh, in nanoseconds. The tick loop reads it to decide
	// how long to wait before the next refresh — if the datasource is
	// slow (e.g. doltlite's chunk-store WAL has grown and every query
	// pays a hundreds-of-milliseconds replay cost) we back off so the
	// dashboard never pegs a core trying to keep up. Reset to 0 on
	// scheduleRefresh from a user action so the next tick reverts to
	// the base cadence promptly when the slowdown clears.
	lastFetchNS atomic.Int64
}

// adaptiveDelay returns the next ticker delay given a base cadence and
// the most recent fetchRefresh duration.
//
// The rule is intentionally simple: keep the base cadence when reads
// finish in well under half the budget, double it when reads exceed
// half the budget, and cap at maxAdaptiveDelay so a wedged datasource
// doesn't stall the dashboard for minutes. The thresholds are picked
// so a healthy DB stays at the base 2s while a doltlite store mid-WAL
// stretch (each refresh ~500ms-3s) settles at 4-30s instead of
// firing back-to-back ticks and saturating a core.
func adaptiveDelay(base, elapsed time.Duration) time.Duration {
	if base <= 0 {
		base = 2 * time.Second
	}
	if elapsed <= base/2 {
		return base
	}
	// Back off to max(base*2, elapsed*2) so we always give the
	// datasource at least as much idle time as it just spent working
	// — otherwise a 4s refresh on a 2s base would still queue
	// back-to-back.
	delay := base * 2
	if elapsed*2 > delay {
		delay = elapsed * 2
	}
	if delay > maxAdaptiveDelay {
		delay = maxAdaptiveDelay
	}
	return delay
}

// maxAdaptiveDelay caps the adaptive ticker. 30s is long enough that a
// truly-wedged datasource (e.g. doltlite re-replaying a multi-MB WAL
// on every query) doesn't burn cycles, short enough that the operator
// still sees the dashboard reanimate within human-impatience range
// once compaction kicks in.
const maxAdaptiveDelay = 30 * time.Second

// Run constructs the gui, opens the alt-screen, and blocks on the
// main loop. Returns nil on a clean quit (ErrQuit), the underlying
// error otherwise.
func Run(ctx context.Context, opts Options) error {
	if opts.Datasource == nil {
		return errors.New("lazy tui: nil datasource")
	}
	if opts.Refresh <= 0 {
		opts.Refresh = 2 * time.Second
	}
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		// OutputTrue (24-bit) so the theme.Palette's RGB values render
		// faithfully on both sides of the rendering pipeline:
		//
		//   - View.FrameColor / View.TitleColor go through
		//     gocui.NewRGBColor → tcell.NewRGBColor without masking.
		//   - Lipgloss-generated "\x1b[38;2;R;G;Bm" escapes inside view
		//     buffers are accepted by gocui's escape interpreter
		//     (escape.go:327 explicitly rejects 38;2;… when mode<OutputTrue).
		//
		// Terminals without truecolor support degrade silently — tcell
		// downsamples to the closest 256-color slot.
		OutputMode: gocui.OutputTrue,
		Headless:   opts.Headless,
		Width:      opts.Width,
		Height:     opts.Height,
	})
	if err != nil {
		return fmt.Errorf("gocui init: %w", err)
	}
	defer g.Close()

	g.InputEsc = true
	g.SupportOverlaps = true
	// Mouse=true unlocks wheel-scroll bindings (MouseWheelUp/Down).
	// Without this gocui swallows wheel events at the tcell layer and
	// the per-view scroll handlers never fire — which is exactly the
	// bug operators hit on the Detail pane and inspector transcript:
	// content overflowed the viewport, j/k worked, the wheel didn't.
	g.Mouse = true

	cctx, cancel := context.WithCancel(ctx)
	gu := &Gui{
		g:           g,
		st:          newState(),
		ds:          opts.Datasource,
		opts:        opts,
		ctx:         cctx,
		cancel:      cancel,
		stopRefresh: make(chan struct{}),
	}

	g.SetManagerFunc(gu.layout)
	if err := gu.bindKeys(); err != nil {
		cancel()
		return fmt.Errorf("bind keys: %w", err)
	}

	// Kick an initial refresh + adaptive ticker. Both go via
	// g.OnWorker / g.Update — no goroutine-per-panel.
	gu.scheduleRefresh()
	go gu.adaptiveTickLoop(opts.Refresh)

	// --job <id> deep-link.
	if opts.InitialJob != "" {
		g.OnWorker(func(_ gocui.Task) error {
			gu.openInspector(opts.InitialJob)
			return nil
		})
	}

	// gocui's MainLoop is hostile to non-fatal mid-flush errors: a
	// SetView / SetCurrentView call against a still-being-rebuilt view
	// can surface ErrUnknownView, which we'd rather log-and-continue
	// than tear the whole TUI down for. Install an ErrorHandler that
	// rewrites those into ignored / drained-and-continue results.
	g.ErrorHandler = func(err error) error {
		if isUnknownView(err) {
			return nil
		}
		return err
	}
	mainErr := g.MainLoop()
	if mainErr != nil && mainErr != gocui.ErrQuit && !errors.Is(mainErr, gocui.ErrQuit) {
		cancel()
		close(gu.stopRefresh)
		return mainErr
	}
	cancel()
	close(gu.stopRefresh)
	return nil
}

// adaptiveTickLoop schedules refreshes on a self-adjusting cadence.
//
// The classic time.NewTicker(base) loop has a nasty failure mode
// against doltlite: when the chunk-store WAL grows long enough that
// each fetchRefresh takes longer than the 2s tick, the worker queue
// stays permanently saturated. Each refresh re-reads ~6 panels, each
// panel cursor-open triggers btreeRefreshFromDisk → csReplayWal, and
// the whole thing pegs a core indefinitely (see the post-mortem in
// docs/daemon.md "100%-CPU lazy").
//
// The adaptive loop sleeps for max(base, elapsed*2) up to a 30s cap,
// so a slow datasource self-throttles instead of melting the CPU.
// Once compaction (autosk gc / the daemon's per-project compactor)
// shrinks the chunk-store again, the next refresh comes back fast,
// lastFetchNS drops, and the loop snaps back to the base cadence on
// the very next iteration.
//
// Runs in a goroutine; nothing touches the gocui screen here —
// Refresh is g.Update-driven.
func (gu *Gui) adaptiveTickLoop(base time.Duration) {
	if base <= 0 {
		base = 2 * time.Second
	}
	for {
		elapsed := time.Duration(gu.lastFetchNS.Load())
		delay := adaptiveDelay(base, elapsed)
		select {
		case <-gu.stopRefresh:
			return
		case <-gu.ctx.Done():
			return
		case <-time.After(delay):
			gu.scheduleRefresh()
		}
	}
}

// scheduleRefresh enqueues a refresh via the gocui worker. If one is
// already in flight we set refreshPending; the running worker will
// loop until pending is clear, so the LATEST scope change always
// drives a trailing refresh. (The previous implementation dropped
// the latest request, which could leave the Jobs panel showing
// results filtered by a stale cursor row after j-spam.)
//
// scheduleRefresh is a thin convenience over scheduleRefreshWith
// that wires the production work function (gu.refreshAll). Tests
// drive the CAS dance through scheduleRefreshWith with a stub work
// callback so a future refactor of the loop can't silently regress
// the trailing-pickup invariant.
func (gu *Gui) scheduleRefresh() {
	gu.scheduleRefreshWith(gu.refreshAll)
}

// scheduleRefreshWith is the testable shape of scheduleRefresh. The
// CAS loop + pending-flag invariants live here; the production
// entry passes gu.refreshAll, tests pass a stub that mirrors the
// timing (a sleep, a counter increment) without touching gocui.
func (gu *Gui) scheduleRefreshWith(work func()) {
	if !gu.refreshInFlight.CompareAndSwap(false, true) {
		gu.refreshPending.Store(true)
		dlog("scheduleRefresh: coalesced (pending set)")
		return
	}
	gu.runDispatch(func() {
		for {
			work()
			gu.refreshInFlight.Store(false)
			// If a request arrived during work(), run it now. The CAS
			// reclaims the in-flight slot so a concurrent
			// scheduleRefresh either parks in pending or wins the race
			// (in which case we exit here).
			if !gu.refreshPending.CompareAndSwap(true, false) {
				return
			}
			if !gu.refreshInFlight.CompareAndSwap(false, true) {
				return
			}
		}
	})
}

// runDispatch dispatches f onto the gocui worker pool (or, in tests,
// whatever gu.dispatch was set to). The indirection is so
// scheduleRefresh's CAS loop can be exercised without a real gocui.
func (gu *Gui) runDispatch(f func()) {
	if gu.dispatch != nil {
		gu.dispatch(f)
		return
	}
	gu.g.OnWorker(func(_ gocui.Task) error { f(); return nil })
}

// quit is the standard handler for q / Ctrl-C.
func (gu *Gui) quit(*gocui.Gui, *gocui.View) error {
	gu.cancel()
	return gocui.ErrQuit
}

// flashf appends to the command log and sets a transient toast.
func (gu *Gui) flashf(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	gu.g.Update(func(_ *gocui.Gui) error {
		gu.st.withLock(func() {
			gu.st.appendLog(msg)
			gu.st.setFlash(msg, level)
		})
		return nil
	})
}

// renderViews re-renders every visible view's content from the model.
// Called from the layout function (cheap; the model fields are the
// "rendered slice" we just project to lines).
func (gu *Gui) renderViews() {
	gu.st.withRLock(func() {
		focused := gu.st.focused

		tasksBody, tasksHdr := renderTasksPanel(gu.st.tasks, gu.st.taskCursor, gu.st.scope, gu.st.filter.Tasks)
		gu.writeView(winTasks, "[1] Tasks", tasksBody)
		gu.applyRowHighlight(winTasks, tasksHdr, gu.st.taskCursor, len(gu.st.tasks), focused == panelTasks)

		jobsBody, jobsHdr := renderJobsPanel(gu.st.jobs, gu.st.jobCursor, gu.st.scope, gu.st.filter.Jobs)
		gu.writeView(winJobs, "[2] Jobs", jobsBody)
		gu.applyRowHighlight(winJobs, jobsHdr, gu.st.jobCursor, len(gu.st.jobs), focused == panelJobs)

		wfBody, wfHdr := renderWorkflowsPanel(gu.st.workflows, gu.st.workflowCursor, gu.st.filter.Workflows)
		gu.writeView(winWorkflows, "[3] Workflows", wfBody)
		gu.applyRowHighlight(winWorkflows, wfHdr, gu.st.workflowCursor, len(gu.st.workflows), focused == panelWorkflows)

		agBody, agHdr := renderAgentsPanel(gu.st.agents, gu.st.agentCursor, gu.st.filter.Agents)
		gu.writeView(winAgents, "[4] Agents", agBody)
		gu.applyRowHighlight(winAgents, agHdr, gu.st.agentCursor, len(gu.st.agents), focused == panelAgents)

		gu.writeView(winDetail, "[0] Detail", renderDetail(gu.st))
		gu.writeView(winLog, "log", renderCommandLog(gu.st.logBuf, gu.st.flash))
		gu.writeView(winStatusBar, "", renderStatusBar(gu.st, gu.opts.ProjectRoot))
		if gu.st.view == StateInspector {
			gu.writeView(winInspectorHdr, "", renderInspectorHeader(gu.st.insp))
			gu.writeView(winInspector, "", gu.renderInspectorBody())
			if gu.st.insp.Tab == tabLive {
				gu.writeView(winInspectorIn, "input", gu.st.insp.liveInput)
			}
		}
	})
}

// applyRowHighlight wires gocui's per-line Highlight to the panel's
// model cursor: when the panel is focused AND has rows, we set
// SelBgColor to the palette's Selection swatch and move the view's
// cy to the cursor row so gocui paints that row's background.
//
// Non-focused panels and empty panels get Highlight=false so the
// cursor row is invisible — matching the lazygit / file-listed-view
// affordance: "only the active list shows where you are".
//
// cyAbs is the cursor's absolute y-index in the view buffer
// (headerLines + modelCursor). The viewport origin (v.oy) is updated
// here to keep cyAbs visible: keystroke-driven moves are handled
// proactively in scrollOffAdjust (lazygit's scroll-off-margin
// algorithm with the cursor's expected motion direction), but THIS
// function is the catch-all that snaps the cursor back into view
// when it leaves the viewport non-contiguously — a filter / scope
// change that shrinks the list, a refresh that lands the cursor on
// a different model index, etc. Without this snap a 100-item list
// scrolled to oy=80 that suddenly becomes 5 items would render
// nothing in the visible window.
//
// rowCount lets us suppress the highlight on the "(no tasks)" / etc.
// placeholder line; without that check the cursor would visibly land
// on the placeholder and look like the empty state is selectable.
func (gu *Gui) applyRowHighlight(name string, headerLines, modelCursor, rowCount int, focused bool) {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return
	}
	if !focused || rowCount == 0 {
		v.Highlight = false
		return
	}
	ox, oy := v.Origin()
	vpH := viewportInnerHeight(v)
	cyAbs := headerLines + modelCursor
	totalLines := headerLines + rowCount

	// If the cursor landed outside the current viewport non-contiguously,
	// snap origin so it's roughly centred. Keystroke moves never enter
	// this branch (scrollOffAdjust handled them); this is the fallback
	// for filter / scope / refresh-induced jumps.
	if cyAbs < oy || cyAbs >= oy+vpH {
		oy = cyAbs - vpH/2
	}
	// Clamp so we don't expose blank rows past the end of the buffer.
	maxOY := totalLines - vpH
	if maxOY < 0 {
		maxOY = 0
	}
	if oy > maxOY {
		oy = maxOY
	}
	if oy < 0 {
		oy = 0
	}
	v.SetOrigin(ox, oy)

	v.Highlight = true
	v.SelBgColor = theme.Active().Selection.Gocui()
	// SelFgColor=Default so the per-column foregrounds we set in
	// renderXxxPanel survive the highlight overlay (gocui only forces
	// the cell's bg to SelBgColor and bolds the fg; it leaves the fg
	// hue alone for RGB Attributes).
	v.SelFgColor = gocui.ColorDefault
	// v.cy is interpreted by gocui as VISIBLE y (relative to v.oy), so
	// the absolute index has to be re-relativised here — SetCursor
	// writes v.cy verbatim and the highlight logic in gocui's
	// setCharacter compares it against the loop's visible y.
	v.SetCursor(0, cyAbs-oy)
}

// viewportInnerHeight returns the number of usable list rows in v
// (its outer height minus the 2-cell frame, clamped to >=1 so a
// degenerate height doesn't make divisions blow up).
func viewportInnerHeight(v *gocui.View) int {
	_, h := v.Size()
	vpH := h - 2
	if vpH < 1 {
		vpH = 1
	}
	return vpH
}

// scrollOffMargin is the look-ahead distance (in rows) the cursor
// keeps from the top/bottom of the viewport during keystroke moves.
// Matches lazygit's `gui.scrollOffMargin` default — the user
// reported expectation is two extra rows of context past the cursor
// in the direction of motion.
const scrollOffMargin = 2

// scrollOffAdjust scrolls the named view's origin so the cursor (whose
// absolute y went from cyAbsBefore to cyAbsAfter via one keystroke)
// stays inside the viewport with `scrollOffMargin` rows of look-ahead
// in the direction of motion. Mirrors lazygit's
// pkg/gui/controllers/scroll_off_margin.go formulas verbatim; the
// margin is capped to half the viewport so on a 3-row panel the
// cursor still moves at all.
func (gu *Gui) scrollOffAdjust(name string, cyAbsBefore, cyAbsAfter int) {
	v, err := gu.g.View(name)
	if err != nil || v == nil {
		return
	}
	ox, oy := v.Origin()
	vpH := viewportInnerHeight(v)
	delta := scrollOffDelta(cyAbsBefore, cyAbsAfter, oy, vpH, scrollOffMargin)
	if delta == 0 {
		return
	}
	newOY := oy + delta
	if newOY < 0 {
		newOY = 0
	}
	v.SetOrigin(ox, newOY)
}

// scrollOffDelta is the pure-Go core of scrollOffAdjust — returns the
// number of lines to add to the viewport origin (signed) so the
// cursor sits at least `margin` rows from the leading edge after the
// move. Extracted so the scroll-off math is testable without a real
// gocui.View.
//
// The two branches mirror lazygit's calculateLinesToScroll{Up,Down}:
//
//   - The (vpH+1)/2 and (vpH-1)/2 caps ensure that on even-height
//     viewports the top margin is one row taller than the bottom,
//     matching lazygit's centring bias on j-spam.
//   - The `before` visibility guard avoids scrolling when the user
//     dragged the cursor out-of-view with the mouse (or via a
//     non-keystroke event) — there's no "before" position inside
//     the viewport to anchor the scroll to.
func scrollOffDelta(before, after, oy, vpH, margin int) int {
	switch {
	case after < before: // moving up
		capped := margin
		if c := (vpH + 1) / 2; capped > c {
			capped = c
		}
		if before >= oy && before < oy+vpH {
			marginEnd := oy + capped
			if after < marginEnd {
				return after - marginEnd // negative
			}
		}
	case after > before: // moving down
		capped := margin
		if c := (vpH - 1) / 2; capped > c {
			capped = c
		}
		if before >= oy && before < oy+vpH {
			marginStart := oy + vpH - capped - 1
			if after > marginStart {
				return after - marginStart // positive
			}
		}
	}
	return 0
}

// headerLinesForLocked returns the number of leading meta lines
// (scope notes + filter pill) that renderXxxPanel will emit before
// the data rows for panel p, given the current state. Must be called
// with the model lock held — it reads gu.st.scope / gu.st.filter.
//
// Kept in lock-step with the matching `header++` paths in
// renderTasksPanel / renderJobsPanel / renderWorkflowsPanel /
// renderAgentsPanel. If a render path adds a new meta line, mirror
// it here or the scroll-off math will be off by that many rows.
func headerLinesForLocked(p panelID, st *state) int {
	switch p {
	case panelTasks:
		h := 0
		if st.scope.WorkflowName != "" {
			h++
		}
		if st.scope.Agent != "" {
			h++
		}
		if st.filter.Tasks != "" {
			h++
		}
		return h
	case panelJobs:
		h := 0
		if st.scope.TaskID != "" {
			h++
		}
		if st.filter.Jobs != "" {
			h++
		}
		return h
	case panelWorkflows:
		if st.filter.Workflows != "" {
			return 1
		}
	case panelAgents:
		if st.filter.Agents != "" {
			return 1
		}
	}
	return 0
}

// writeView writes content into a view, clearing first. Tolerates the
// view not existing yet (the layout function may not have created it
// on the very first frame, and the current viewState may not include
// the requested window).
func (gu *Gui) writeView(name, title, body string) {
	v, err := gu.g.View(name)
	if err != nil {
		return
	}
	v.Clear()
	if title != "" {
		v.Title = title
	}
	v.Wrap = false
	_, _ = v.Write([]byte(body))
}

// truncateLines crops a string to at most n lines (helps small views).
func truncateLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) <= n {
		return s
	}
	return strings.Join(parts[:n], "\n")
}
