package tui

import (
	"github.com/jesseduffield/gocui"

	"autosk/internal/lazy/theme"
)

// roundedFrameRunes is the lazygit-style "rounded" border preset: the
// horizontal/vertical edges stay the standard light-box runes, and the
// four corners are swapped for their rounded variants. The slot order
// matches gocui's expected layout (see gocui/gui.go:cornerCustomRune):
//
//	0: horizontal '─'
//	1: vertical   '│'
//	2: top-left   '╭'
//	3: top-right  '╮'
//	4: bottom-left  '╰'
//	5: bottom-right '╯'
//
// gocui falls back to its hard-edge defaults for any rune past index 5
// (the T-junctions used when views meet at edges), which is fine for
// the lazy dashboard — our panels are placed side by side with their
// own frames rather than welded into a single grid, so those indices
// are never consulted.
var roundedFrameRunes = []rune{'─', '│', '╭', '╮', '╰', '╯'}

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

	// Snapshot every state field we touch in this layout pass under
	// the RLock. After the snapshot we don't read st.* directly:
	// concurrent g.Update closures + worker-side mutations (inspector
	// SSE) would otherwise race against these reads under -race.
	var (
		state      ViewState
		focused    string
		logHidden  bool
		currentTab inspectorTab
		focusedWin string
	)
	gu.st.withRLock(func() {
		state = gu.st.view
		focusedWin = gu.st.focused.window()
		logHidden = gu.st.logHide
		currentTab = gu.st.insp.Tab
	})
	focused = focusedWin
	if state == StateInspector {
		focused = ""
	}
	args := arrangeArgs{
		width:       w,
		height:      h,
		focusedSide: focused,
		state:       state,
		logHidden:   logHidden,
	}
	showLiveInput := state == StateInspector && currentTab == tabLive
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
			v.FrameRunes = roundedFrameRunes
		}
		// Highlight the focused side panel's frame AND title. gocui
		// retains the last-set FrameColor/TitleColor on the View object
		// across layout passes (SetView returns the same view, it doesn't
		// reset attrs), so the else branches are load-bearing: without
		// them a panel that lost focus would stay tinted forever — same
		// goes for any panel that was focused before the inspector
		// opened and then dashboard came back.
		//
		// Title color is kept in sync with the frame on purpose: the
		// "[N] Title" string is the visual marker for which panel owns
		// the cursor right now. Per-panel tab labels rendered INSIDE the
		// body (when we add them) live in the view buffer and are not
		// touched by TitleColor — they stay default-coloured by design.
		focusAttr := theme.Active().Focus.Gocui()
		if state == StateDashboard && win == focusedWin {
			v.FrameColor = focusAttr
			v.TitleColor = focusAttr
		} else {
			v.FrameColor = gocui.ColorDefault
			v.TitleColor = gocui.ColorDefault
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
		if _, err := g.SetCurrentView(focusedWin); err != nil && !isUnknownView(err) {
			// best-effort; views may not exist yet on first frame
		}
	}

	// Popups, when active, sit on top with overlap.
	gu.layoutPopup(g, w, h)

	// Sync the terminal-cursor visibility with whatever view ended
	// up current after all the SetCurrentView calls above. lazygit
	// does the same in pkg/gui/context.go (Activate):
	//
	//	g.Cursor = v.Editable && v.Mask == ""
	//
	// gocui's drawing function only paints a terminal cursor when
	// g.Cursor=true (gocui/gui.go::draw), and it defaults to false.
	// Without this sync the text-input fields — compose summary /
	// description, prompt popup, inspector live input — all looked
	// like static panes; the user typed and saw text appear but no
	// blinking cursor, no caret position. Re-syncing here (instead
	// of at every SetCurrentView site) makes it impossible for a
	// future Editable view to forget the cursor.
	if cv := g.CurrentView(); cv != nil {
		g.Cursor = cv.Editable && cv.Mask == ""
	} else {
		g.Cursor = false
	}

	gu.renderViews()
	return nil
}
