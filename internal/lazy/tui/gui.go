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
	"time"

	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/datasource"
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
}

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
		OutputMode: gocui.OutputNormal,
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
	g.Mouse = false

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

	// Kick an initial refresh + 2s ticker. Both go via g.OnWorker /
	// g.Update — no goroutine-per-panel.
	g.OnWorker(func(_ gocui.Task) error { gu.refreshAll(); return nil })
	go gu.tickLoop(opts.Refresh)

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

// tickLoop ticks the refresh every d. Runs in a goroutine; nothing
// touches the gocui screen here — Refresh is g.Update-driven.
func (gu *Gui) tickLoop(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-gu.stopRefresh:
			return
		case <-gu.ctx.Done():
			return
		case <-t.C:
			gu.g.OnWorker(func(_ gocui.Task) error {
				gu.refreshAll()
				return nil
			})
		}
	}
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
		gu.writeView(winTasks, "[1] Tasks", renderTasksPanel(gu.st.tasks, gu.st.taskCursor, gu.st.scope, gu.st.filter.Tasks))
		gu.writeView(winJobs, "[2] Jobs", renderJobsPanel(gu.st.jobs, gu.st.jobCursor, gu.st.scope, gu.st.filter.Jobs))
		gu.writeView(winWorkflows, "[3] Workflows", renderWorkflowsPanel(gu.st.workflows, gu.st.workflowCursor, gu.st.filter.Workflows))
		gu.writeView(winAgents, "[4] Agents", renderAgentsPanel(gu.st.agents, gu.st.agentCursor, gu.st.filter.Agents))
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
