package tui

import (
	"github.com/jesseduffield/gocui"
)

// layout is the gocui Manager: it asks boxlayout for window
// dimensions, calls SetView for every named window, hides those that
// don't appear in the current arrangement, and finally re-renders
// every view's content.
func (gu *Gui) layout(g *gocui.Gui) error {
	w, h := g.Size()
	if w < 60 || h < 12 {
		// Too tiny for our layout: paint a courtesy message in a single
		// view and bail.
		if v, err := g.SetView("tiny", 0, 0, w-1, h-1, 0); err != nil && !isUnknownView(err) {
			return err
		} else if v != nil {
			v.Clear()
			_, _ = v.Write([]byte("Terminal too small (need ≥ 60x12). Resize or quit (q).\n"))
		}
		return nil
	}
	_ = g.DeleteView("tiny")

	gu.st.withRLock(func() {})
	state := gu.st.view
	focused := gu.st.focused.window()
	if state == StateInspector {
		focused = ""
	}
	args := arrangeArgs{
		width:       w,
		height:      h,
		focusedSide: focused,
		state:       state,
		logHidden:   gu.st.logHide,
	}
	showLiveInput := state == StateInspector && gu.st.insp.Tab == tabLive
	dims := arrange(args, showLiveInput)

	// In dashboard state, hide inspector views; in inspector state,
	// hide dashboard views. We only delete views that are NOT in the
	// current arrangement so we don't churn through a delete+create
	// on every flush (gocui doesn't react gracefully to that — a
	// SetCurrentView landing between the delete and the create
	// surfaces as ErrUnknownView, which MainLoop propagates).
	active := map[string]bool{}
	for name := range dims {
		active[name] = true
	}
	for _, name := range []string{winTasks, winJobs, winWorkflows, winAgents, winDetail, winLog, winInspectorHdr, winInspector, winInspectorIn} {
		if !active[name] {
			_ = g.DeleteView(name)
		}
	}

	// Create views for the active arrangement.
	for win, d := range dims {
		if d.X1-d.X0 < 1 || d.Y1-d.Y0 < 0 {
			continue
		}
		v, err := g.SetView(win, d.X0, d.Y0, d.X1, d.Y1, 0)
		if err != nil && !isUnknownView(err) {
			return err
		}
		if v == nil {
			continue
		}
		v.Wrap = false
		switch win {
		case winStatusBar, winFlash:
			v.Frame = false
		case winInspectorHdr:
			v.Frame = false
		default:
			v.Frame = true
		}
		// Highlight the focused side panel's frame.
		if state == StateDashboard && win == gu.st.focused.window() {
			v.FrameColor = gocui.ColorCyan
		}
		if win == winInspectorIn {
			v.Editable = true
			v.Editor = gocui.EditorFunc(gu.liveEditor)
		}
	}

	// Set the active view (current focus) so key events route correctly.
	switch state {
	case StateInspector:
		if showLiveInput {
			if _, err := g.SetCurrentView(winInspectorIn); err != nil && !isUnknownView(err) {
				return err
			}
		} else {
			if _, err := g.SetCurrentView(winInspector); err != nil && !isUnknownView(err) {
				return err
			}
		}
	default:
		if _, err := g.SetCurrentView(gu.st.focused.window()); err != nil && !isUnknownView(err) {
			// best-effort; views may not exist yet on first frame
		}
	}

	// Popups, when active, sit on top with overlap.
	gu.layoutPopup(g, w, h)

	gu.renderViews()
	return nil
}
