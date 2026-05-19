package tui

import (
	"context"
	"fmt"
	"strings"

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
			t, _ := gu.ds.GetJob(gu.ctx, jobID)
			sigs, _ := gu.ds.Signals(gu.ctx, t.TaskID)
			cms, _ := gu.ds.Comments(gu.ctx, t.TaskID)
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
// inspector's live buffer. Throttles UI updates to 30ms to keep fast
// streams from flickering — same knob lazygit uses for tasks.go.
func (gu *Gui) pumpLive(ctx context.Context, h *datasource.LiveHandle) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-h.Events:
			if !ok {
				return
			}
			line := renderLiveEvent(ev)
			gu.g.Update(func(_ *gocui.Gui) error {
				gu.st.withLock(func() {
					gu.st.insp.liveBuf = append(gu.st.insp.liveBuf, line)
					if len(gu.st.insp.liveBuf) > 2000 {
						gu.st.insp.liveBuf = gu.st.insp.liveBuf[len(gu.st.insp.liveBuf)-2000:]
					}
					if ev.Kind == "status" || ev.Kind == "done" {
						gu.st.insp.Streaming = ev.Status.Streaming
						gu.st.insp.Job.JobResponse = ev.Status
					}
				})
				return nil
			})
		}
	}
}

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

// inspectorCycleTab cycles inspector tab by step (±1).
func (gu *Gui) inspectorCycleTab(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		var newTab inspectorTab
		gu.st.withLock(func() {
			n := 4
			cur := int(gu.st.insp.Tab)
			cur = (cur + step + n) % n
			gu.st.insp.Tab = inspectorTab(cur)
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
			fmt.Fprintf(&b, "  %s  %s → %s  (%s)\n",
				s.CreatedAt.Format("15:04:05"), s.StepName, s.Target, s.AgentName)
		}
	}
	b.WriteString("\n" + styleHeader.Render("comments") + "\n")
	if len(comments) == 0 {
		b.WriteString(styleMuted.Render("(no comments)") + "\n")
	} else {
		for _, c := range comments {
			fmt.Fprintf(&b, "  %s  %s: %s\n",
				c.CreatedAt.Format("15:04:05"), c.AuthorName, truncate(c.Text, 80))
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
