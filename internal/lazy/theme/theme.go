// Package theme owns the lazy TUI's colour palette. UI code never
// references raw colours directly — it asks the active Palette for a
// semantic slot (Focus, Accent, Err, ...). Swapping the palette is a
// one-line change in Default() / a future config hook; the consumer
// code stays put.
//
// Two adapters live on the Color type so the same palette feeds both
// gocui (View.FrameColor / View.TitleColor — true-colour Attribute)
// and lipgloss (Style.Foreground — ANSI-escape "#rrggbb" string).
// Holding both forms on one value rules out the dual-source drift
// where the panel frame and the cursor row would otherwise look
// "almost the same colour" because two literals diverged.
package theme

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/jesseduffield/gocui"
)

// Color is one swatch from a palette, expressed in both forms the TUI
// consumes: an RGB triple (gocui side) and a "#rrggbb" hex string
// (lipgloss side). Palette authors write a hex literal once via RGB();
// the consumer asks for whichever form they need via Lipgloss() / Gocui().
type Color struct {
	R, G, B uint8
	Hex     string
}

// RGB parses "#rrggbb" into a Color. Malformed input returns a Color
// whose Hex is preserved so a typo at palette-definition time is
// recoverable (lipgloss will still receive the string, gocui will
// receive black) instead of crashing the TUI on startup.
func RGB(hex string) Color {
	c := Color{Hex: hex}
	if len(hex) == 7 && hex[0] == '#' {
		var r, g, b uint8
		if n, err := fmt.Sscanf(hex[1:], "%02x%02x%02x", &r, &g, &b); err == nil && n == 3 {
			c.R, c.G, c.B = r, g, b
		}
	}
	return c
}

// Lipgloss returns the colour wrapped as a lipgloss.TerminalColor so
// it can be passed to Style.Foreground / Style.Background. An empty
// Hex degrades to lipgloss.NoColor{} so an under-populated palette
// renders as terminal-default rather than as a stray escape sequence.
func (c Color) Lipgloss() lipgloss.TerminalColor {
	if c.Hex == "" {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color(c.Hex)
}

// Gocui returns the colour as a gocui Attribute for View.FrameColor /
// View.TitleColor. Requires the Gui to be in OutputTrue mode for the
// full 24-bit fidelity; in OutputNormal / Output256 modes gocui silently
// masks down to 4-bit / 8-bit ANSI so the colour still renders, just
// less precisely.
func (c Color) Gocui() gocui.Attribute {
	return gocui.NewRGBColor(int32(c.R), int32(c.G), int32(c.B))
}

// Palette is the full set of named slots the TUI consumes. Every UI
// element pulls from a slot rather than from a raw colour — that
// indirection is the whole point of this package, and what makes a
// future "switch to gruvbox" PR a one-file change.
type Palette struct {
	// Name is a human-readable id for debugging / status-bar surfacing.
	Name string

	// Chrome (frames + titles, gocui-side).
	Focus    Color // focused side-panel frame + title
	PopupBox Color // popup frame (menu / confirm / prompt)

	// Body text (lipgloss-side).
	Header Color // bold section headers ("task X", "step signals", inspector "run X", transcript kind)
	Muted  Color // separators, placeholders, log lines, inactive inspector tab labels
	Accent Color // cursor row, active inspector tab, ⚡ info flash, transcript event name, popup ▶
	Warn   Color // warn flash, daemon=stale, flaky chip
	Err    Color // err flash, error lines, archive load failed, daemon=down, stream error
	OK     Color // daemon=ok, *live*, streaming chip
	Scope  Color // scope chip "task=X wf=Y agent=Z"
	Filter Color // "/ filter" prompt label in panel headers
}

// active holds the palette the TUI currently renders with. Exposed
// through Active() / SetActive() rather than as a global so tests
// (and a future runtime palette switcher) can swap it safely. The
// default is TokyoNight().
var active = TokyoNight()

// Active returns the currently-installed palette.
func Active() Palette { return active }

// SetActive installs p as the active palette. Returns the previous
// palette so the caller can restore it (test pattern: defer SetActive(prev)).
//
// Note: Lipgloss styles cached at package-load time in tui/render.go
// snapshot the palette they were built against. Callers wanting the
// switch to take effect on already-running renders need to rebuild
// those styles (tui exposes RebuildStyles for exactly that).
func SetActive(p Palette) Palette {
	prev := active
	active = p
	return prev
}
