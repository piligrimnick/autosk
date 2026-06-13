package tui

import (
	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazycore/pkg/boxlayout"

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
		focusedWin       string
		focusedSide      string
		logHidden        bool
		showSessionInput bool
	)
	gu.st.withRLock(func() {
		focusedWin = gu.st.focused.window()
		// focusedSide drives the side-stack accordion (which side
		// panel grows). When the caret is in winSessionInput we still
		// want the Sessions column to be the wide one above, so
		// normalize panelSessionInput → winSessions for that calculation.
		focusedSide = gu.st.focused.normalizeForDetail().window()
		logHidden = gu.st.logHide
		// showSessionInput is gated on two conditions:
		//
		//   1. The Detail pane is currently showing a session
		//      (detailShowsSession: focused panel — or detailFocus when
		//      focused == panelDetail — normalises to panelSessions).
		//      Without this gate the input would appear whenever the
		//      Sessions panel's cursor happens to point at a live session
		//      EVEN IF the Detail pane is showing a task / workflow /
		//      agent. The input only makes sense as an attachment to
		//      the Session Detail body above it.
		//
		//   2. The selected session is non-terminal (isSessionLive, true for
		//      queued or running). The view's lifecycle matches the
		//      session's lifecycle — allocated once when the session becomes
		//      non-terminal, destroyed once when it terminates — so
		//      the view is never recreated mid-session and drafts can
		//      never be lost to gocui's NewView clearing the
		//      TextArea on creation.
		//
		// Once both gates pass, the input stays pinned even when the
		// operator focuses the Detail pane to read the transcript
		// above (panelDetail with detailFocus==panelSessions still
		// satisfies condition 1).
		if detailShowsSession(gu.st) {
			if s, ok := gu.st.selectedSession(); ok && isSessionLive(s) {
				showSessionInput = true
			}
		}
	})
	args := arrangeArgs{
		width:       w,
		height:      h,
		focusedSide: focusedSide,
		state:       StateDashboard,
		logHidden:   logHidden,
	}
	dims := arrange(args)

	// Overlay winSessionInput INSIDE winDetail's coordinates when the
	// selected session is live. Detail's rounded frame surrounds both the
	// transcript and the input box visually — the input has its own
	// rounded frame inset by 1 cell from detail's frame on left/right
	// and 1 cell above detail's bottom frame. The overlay's height is
	// fixed at sessionInputOverlayH rows (frame top + 2 content + frame
	// bottom). Sticky-tail and scroll handlers consult
	// detailEffectiveInnerH() so the visible region clamps to the rows
	// above the overlay.
	if showSessionInput {
		if detail, ok := dims[winDetail]; ok {
			overlay := boxlayout.Dimensions{
				X0: detail.X0 + 1,
				X1: detail.X1 - 1,
				Y0: detail.Y1 - sessionInputOverlayH,
				Y1: detail.Y1 - 1,
			}
			// Only add the overlay when there's enough room:
			//   - the box itself has sensible interior dimensions, AND
			//   - it sits at least 2 rows BELOW detail's top edge so
			//     a few rows of transcript stay visible above it AND
			//     overlay.Y0 never lands at a negative offset (at the
			//     dashboard min size detail's outer height collapses
			//     to ~1 row, which would push the overlay off-screen).
			if overlay.X1-overlay.X0 >= 4 &&
				overlay.Y1-overlay.Y0 >= 1 &&
				overlay.Y0 >= detail.Y0+2 {
				dims[winSessionInput] = overlay
			}
		}
	}

	// Garbage-collect any dashboard window that doesn't appear in the
	// current arrangement (e.g. winSessionInput when the selected session is
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

	// Create views for the active arrangement. Iterate in a fixed
	// order so the initial creation pass produces a deterministic
	// view stack — gocui renders views in creation order, and the
	// overlay winSessionInput must be created AFTER winDetail so it
	// draws ON TOP of detail's bottom rows. allDashboardWindows
	// already lists winDetail before winSessionInput, and the status
	// bar comes last.
	visitOrder := append([]string(nil), allDashboardWindows...)
	visitOrder = append(visitOrder, winStatusBar, winOptionsStrip)
	for _, win := range visitOrder {
		d, ok := dims[win]
		if !ok {
			continue
		}
		if d.X1-d.X0 < 1 || d.Y1-d.Y0 < 0 {
			continue
		}
		// gocui quirk: a Frame=false view's writeable area is its
		// outer rectangle shrunk by 1 cell on each side (InnerSize
		// = Size - 2; tcell paints content at v.x0+x+1, v.y0+y+1).
		// If we hand boxlayout's edges to SetView verbatim, a
		// Frame=false box of Size:1 collapses to InnerHeight=0 and
		// paints zero content rows — exactly what was happening to
		// the bottom statusbar + options strip after ask-ed8035
		// switched them from Size:3 to Size:1.
		//
		// Lazygit solves this by expanding Frame=false SetView
		// coordinates outward by 1 cell on each side (see
		// pkg/gui/layout.go:75-95 — `frameOffset := 1` when
		// !view.Frame), which makes the writeable area land exactly
		// on the boxlayout edges. We do the same here for the
		// dashboard's only Frame=false views, the bottom statusbar
		// and options strip.
		frameOffset := 0
		switch win {
		case winStatusBar, winOptionsStrip:
			frameOffset = 1
		}
		v, err := g.SetView(win, d.X0-frameOffset, d.Y0-frameOffset, d.X1+frameOffset, d.Y1+frameOffset, 0)
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
		case winStatusBar, winOptionsStrip, winFlash:
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
		mutedAttr := theme.Active().Muted.Gocui()
		switch win {
		case winSessionInput:
			// winSessionInput is overlaid INSIDE winDetail's frame; its own
			// frame + title stay muted (gray) at all times so it reads
			// as chrome that belongs to the detail pane rather than a
			// standalone panel competing for the operator's attention.
			// The hotkey hint in the title also wears muted.
			v.FrameColor = mutedAttr
			v.TitleColor = mutedAttr
		default:
			if win == focusedWin {
				v.FrameColor = focusAttr
				v.TitleColor = focusAttr
			} else {
				v.FrameColor = gocui.ColorDefault
				v.TitleColor = gocui.ColorDefault
			}
		}
		if win == winSessionInput {
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
