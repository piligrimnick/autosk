package tui

import (
	"strings"

	"github.com/jesseduffield/gocui"
)

// openMenu pushes a Menu popup with the given title, lines, and the
// onSelect handler. Esc cancels; Enter calls onSelect(cursor).
func (gu *Gui) openMenu(title string, lines []string, onSelect func(int) error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:    popupMenu,
			Title:   title,
			Lines:   lines,
			Cursor:  0,
			OnSelect: onSelect,
		}
	})
	gu.g.Update(func(_ *gocui.Gui) error { return nil })
}

// openConfirm pushes a Confirm popup; onAccept runs on y/Enter.
func (gu *Gui) openConfirm(prompt string, onAccept func() error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:    popupConfirm,
			Title:   prompt,
			OnAccept: func(string) error { return onAccept() },
		}
	})
	gu.g.Update(func(_ *gocui.Gui) error { return nil })
}

// openPrompt pushes a Prompt popup; onAccept gets the typed value.
func (gu *Gui) openPrompt(prompt, initial string, onAccept func(string) error) {
	gu.st.withLock(func() {
		gu.st.popup = popupState{
			Kind:    popupPrompt,
			Title:   prompt,
			Input:   initial,
			OnAccept: onAccept,
		}
	})
	gu.g.Update(func(_ *gocui.Gui) error { return nil })
}

// popupClose dismisses the active popup.
func (gu *Gui) popupClose(*gocui.Gui, *gocui.View) error {
	var cancel func() error
	gu.st.withLock(func() {
		cancel = gu.st.popup.OnCancel
		gu.st.popup = popupState{}
	})
	if cancel != nil {
		return cancel()
	}
	return nil
}

// popupConfirmYes treats y / Y / Enter on a confirm popup.
func (gu *Gui) popupConfirmYes(*gocui.Gui, *gocui.View) error {
	var fn func(string) error
	gu.st.withLock(func() {
		fn = gu.st.popup.OnAccept
		gu.st.popup = popupState{}
	})
	if fn != nil {
		return fn("yes")
	}
	return nil
}

// popupAccept handles Enter on Menu / Prompt.
func (gu *Gui) popupAccept(_ *gocui.Gui, v *gocui.View) error {
	var (
		kind  popupKind
		sel   func(int) error
		acc   func(string) error
		cur   int
		input string
	)
	if v != nil {
		input = strings.TrimRight(v.Buffer(), "\n")
	}
	gu.st.withLock(func() {
		kind = gu.st.popup.Kind
		sel = gu.st.popup.OnSelect
		acc = gu.st.popup.OnAccept
		cur = gu.st.popup.Cursor
		if kind == popupPrompt {
			gu.st.popup.Input = input
		}
		gu.st.popup = popupState{}
	})
	switch kind {
	case popupMenu:
		if sel != nil {
			return sel(cur)
		}
	case popupPrompt:
		if acc != nil {
			return acc(input)
		}
	}
	return nil
}

// popupCursor moves the menu selection.
func (gu *Gui) popupCursor(step int) func(*gocui.Gui, *gocui.View) error {
	return func(*gocui.Gui, *gocui.View) error {
		gu.st.withLock(func() {
			if gu.st.popup.Kind != popupMenu {
				return
			}
			n := len(gu.st.popup.Lines)
			if n == 0 {
				return
			}
			gu.st.popup.Cursor = (gu.st.popup.Cursor + step + n) % n
		})
		return nil
	}
}

// layoutPopup draws whatever popup is active. Centered, with overlap
// over the dashboard / inspector underneath.
func (gu *Gui) layoutPopup(g *gocui.Gui, w, h int) {
	var (
		kind  popupKind
		title string
		lines []string
		cur   int
		input string
	)
	gu.st.withRLock(func() {
		kind = gu.st.popup.Kind
		title = gu.st.popup.Title
		lines = gu.st.popup.Lines
		cur = gu.st.popup.Cursor
		input = gu.st.popup.Input
	})
	for _, w := range []string{winPopupMenu, winPopupConfirm, winPopupPrompt} {
		_ = g.DeleteView(w)
	}
	switch kind {
	case popupMenu, popupHelp:
		gu.drawPopup(g, winPopupMenu, w, h, title, renderMenuBody(lines, cur))
		if _, err := g.SetCurrentView(winPopupMenu); err != nil && !isUnknownView(err) {
			return
		}
	case popupConfirm:
		gu.drawPopup(g, winPopupConfirm, w, h, title, "[y]es / [n]o / [Esc] cancel")
		if _, err := g.SetCurrentView(winPopupConfirm); err != nil && !isUnknownView(err) {
			return
		}
	case popupPrompt:
		v := gu.drawPopup(g, winPopupPrompt, w, h, title, "")
		if v != nil {
			v.Editable = true
			v.Wrap = false
			v.Editor = gocui.DefaultEditor
			// Preserve initial.
			if v.Buffer() == "" && input != "" {
				_, _ = v.Write([]byte(input))
			}
		}
		if _, err := g.SetCurrentView(winPopupPrompt); err != nil && !isUnknownView(err) {
			return
		}
	}
}

func (gu *Gui) drawPopup(g *gocui.Gui, name string, w, h int, title, body string) *gocui.View {
	pw, ph := minPopup(w, h, body)
	x0 := (w - pw) / 2
	y0 := (h - ph) / 2
	v, err := g.SetView(name, x0, y0, x0+pw, y0+ph, 0)
	if err != nil && !isUnknownView(err) {
		return nil
	}
	if v == nil {
		return nil
	}
	v.Title = title
	v.Frame = true
	v.FrameColor = gocui.ColorMagenta
	if body != "" {
		v.Clear()
		_, _ = v.Write([]byte(body))
	}
	return v
}

func minPopup(w, h int, body string) (int, int) {
	pw := 60
	ph := 8 + strings.Count(body, "\n")
	if pw > w-4 {
		pw = w - 4
	}
	if ph > h-4 {
		ph = h - 4
	}
	if ph < 5 {
		ph = 5
	}
	return pw, ph
}

func renderMenuBody(lines []string, cur int) string {
	var b strings.Builder
	for i, l := range lines {
		if i == cur {
			b.WriteString("▶ " + styleAccent.Render(l) + "\n")
		} else {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}
