package tui

import (
	"github.com/jesseduffield/gocui"

	"autosk/internal/daemon/runstore"
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
	// concurrent g.Update closures + worker-side mutations (jobdetail
	// SSE pump) would otherwise race against these reads under -race.
	var (
		focusedWin   string
		logHidden    bool
		showJobInput bool
	)
	gu.st.withRLock(func() {
		focusedWin = gu.st.focused.window()
		logHidden = gu.st.logHide
		// showJobInput is "selected job is running" — does NOT depend
		// on which view is currently focused. The Jobs cursor sitting
		// on a running job is enough; the input stays pinned even
		// when the operator is reading the transcript above with the
		// Detail pane focused.
		if j, ok := gu.st.selectedJob(); ok {
			if runstore.RunStatus(j.Status) == runstore.StatusRunning {
				showJobInput = true
			}
		}
	})
	args := arrangeArgs{
		width:        w,
		height:       h,
		focusedSide:  focusedWin,
		state:        StateDashboard,
		logHidden:    logHidden,
		showJobInput: showJobInput,
	}
	dims := arrange(args)

	// Garbage-collect any dashboard window that doesn't appear in the
	// current arrangement (e.g. winJobInput when the selected job is
	// terminal). We only delete views that are NOT in the current
	// arrangement so we don't churn through a delete+create on every
	// flush (gocui doesn't react gracefully to that — a
	// SetCurrentView landing between the delete and the create
	// surfaces as ErrUnknownView, which MainLoop propagates).
	active := map[string]bool{}
	for name := range dims {
		active[name] = true
	}
	for _, name := range allDashboardWindows {
		if !active[name] {
			_ = g.DeleteView(name)
			// The view (and its internal buffer) is gone — the next
			// writeView that targets this name will land on a freshly
			// recreated, empty view, so any cached body must be
			// dropped or the short-circuit would leave the new view
			// visibly blank.
			gu.invalidateBodyCache(name)
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
		if isUnknownView(err) {
			// First-creation path: SetView returned the freshly-built,
			// empty view AND ErrUnknownView. Drop the cache entry so the
			// writeView at the end of the frame actually writes into it.
			gu.invalidateBodyCache(win)
		}
		if v == nil {
			continue
		}
		v.Wrap = false
		switch win {
		case winStatusBar, winFlash:
			v.Frame = false
		default:
			v.Frame = true
			v.FrameRunes = roundedFrameRunes
		}
		// Highlight the focused side panel's frame AND title. gocui
		// retains the last-set FrameColor/TitleColor on the View object
		// across layout passes (SetView returns the same view, it doesn't
		// reset attrs), so the else branches are load-bearing: without
		// them a panel that lost focus would stay tinted forever.
		//
		// Title color is kept in sync with the frame on purpose: the
		// "[N] Title" string is the visual marker for which panel owns
		// the cursor right now.
		focusAttr := theme.Active().Focus.Gocui()
		if win == focusedWin {
			v.FrameColor = focusAttr
			v.TitleColor = focusAttr
		} else {
			v.FrameColor = gocui.ColorDefault
			v.TitleColor = gocui.ColorDefault
		}
		if win == winJobInput {
			v.Editable = true
			v.Editor = gocui.EditorFunc(gu.liveEditor)
		}
	}

	// Set the active view (current focus) so key events route
	// correctly. Best-effort: views may not exist yet on first
	// frame, and isUnknownView is the only error we expect.
	if _, err := g.SetCurrentView(focusedWin); err != nil {
		if !isUnknownView(err) {
			dlog("SetCurrentView(%s): %v", focusedWin, err)
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
	// description, prompt popup, jobInput textarea — all look like
	// static panes; the user types and sees text appear but no
	// blinking cursor, no caret position.
	if cv := g.CurrentView(); cv != nil {
		g.Cursor = cv.Editable && cv.Mask == ""
	} else {
		g.Cursor = false
	}

	gu.renderViews()
	return nil
}
