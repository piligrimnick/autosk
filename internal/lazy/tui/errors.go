package tui

import (
	"github.com/jesseduffield/gocui"
)

// isUnknownView reports whether err is gocui.ErrUnknownView.
//
// gocui wraps its sentinel errors via github.com/go-errors/errors,
// which (at the pinned version) does NOT implement the stdlib Unwrap
// interface. That means stdlib errors.Is(err, gocui.ErrUnknownView)
// returns false against a wrapped value. So we compare via Error()
// here \u2014 cheap and robust.
func isUnknownView(err error) bool {
	if err == nil {
		return false
	}
	return err == gocui.ErrUnknownView || err.Error() == gocui.ErrUnknownView.Error()
}
