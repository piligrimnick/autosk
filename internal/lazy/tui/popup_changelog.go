package tui

import (
	"github.com/jesseduffield/gocui"

	"autosk/internal/changelog"
)

// handleWhatsNew is the global ctrl+w handler: opens the changelog
// modal with the FULL embedded CHANGELOG.md body (every parsed
// entry, newest-first) regardless of last_seen_changelog, and does
// NOT mutate state.json on dismiss.
//
// When the embedded changelog is empty (no parseable entries) we
// flash an info toast instead of opening an empty popup — the
// operator pressed ctrl+w expecting content, and a silent no-op
// looks like a broken keybinding.
//
// First-run-loop guard (review R1): when a popupChangelog is
// ALREADY active (the auto-popup fired on startup), pressing
// ctrl+w upgrades the body to the full history but PRESERVES the
// pre-existing OnDismissChangelog. Otherwise a curious operator
// who presses ctrl+w on top of the auto-popup and then dismisses
// would have silently dropped the state.json save — leaving them
// in a popup loop where the auto-popup fires again on the next
// start. AC #13 ("ctrl+w does not mutate state.json on dismiss")
// is still satisfied for the stand-alone re-opener path: with no
// pre-existing popup, existing stays nil and dismissal is a pure
// read.
func (gu *Gui) handleWhatsNew(*gocui.Gui, *gocui.View) error {
	entries := changelog.Embedded()
	if len(entries) == 0 {
		gu.flashf("info", "what's new: changelog is empty")
		return nil
	}
	var existing func() error
	gu.st.withRLock(func() {
		if gu.st.popup.Kind == popupChangelog {
			existing = gu.st.popup.OnDismissChangelog
		}
	})
	body := changelog.RenderMarkdown(entries)
	gu.openChangelog("What's new — CHANGELOG.md", body, existing)
	return nil
}

// changelogScroll moves the changelog popup's viewport one line at
// a time. Mirror of detailScroll but scoped to winPopupChangelog
// so the same key shape (j/k/arrows) doesn't accidentally scroll
// the Detail pane underneath.
func (gu *Gui) changelogScroll(step int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		return gu.scrollViewByLines(winPopupChangelog, step)
	}
}

// changelogScrollPage scrolls the changelog popup by one screen.
// Reuses scrollViewByLines (which honours the view's InnerSize)
// so the page distance matches the visible content height.
func (gu *Gui) changelogScrollPage(dir int) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winPopupChangelog)
		if err != nil || v == nil {
			return nil
		}
		_, h := v.InnerSize()
		if h < 1 {
			h = 1
		}
		return gu.scrollViewByLines(winPopupChangelog, dir*h)
	}
}

// changelogScrollTo jumps to the top (g) or bottom (G) of the
// changelog popup.
func (gu *Gui) changelogScrollTo(bottom bool) func(*gocui.Gui, *gocui.View) error {
	return func(_ *gocui.Gui, _ *gocui.View) error {
		v, err := gu.g.View(winPopupChangelog)
		if err != nil || v == nil {
			return nil
		}
		ox, _ := v.Origin()
		if !bottom {
			v.SetOrigin(ox, 0)
			return nil
		}
		lines := viewBufferLineCount(v)
		_, h := v.InnerSize()
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
