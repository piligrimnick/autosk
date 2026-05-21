package theme

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestRGB_ParsesHex pins the "#rrggbb" → uint8 parsing for the
// hex literals palette authors will write. Loose parsing is fine
// (malformed input keeps Hex so lipgloss still gets the string),
// but the happy path MUST be accurate to the byte — a future
// refactor that swaps Sscanf for a custom parser can't silently
// drop low bits.
func TestRGB_ParsesHex(t *testing.T) {
	cases := []struct {
		hex     string
		r, g, b uint8
	}{
		{"#7aa2f7", 0x7a, 0xa2, 0xf7}, // TN blue
		{"#bb9af7", 0xbb, 0x9a, 0xf7}, // TN magenta
		{"#000000", 0, 0, 0},
		{"#ffffff", 255, 255, 255},
		{"#9ece6a", 0x9e, 0xce, 0x6a}, // TN green
	}
	for _, tc := range cases {
		t.Run(tc.hex, func(t *testing.T) {
			c := RGB(tc.hex)
			if c.R != tc.r || c.G != tc.g || c.B != tc.b {
				t.Fatalf("RGB(%s)=%v want (%d,%d,%d)", tc.hex, c, tc.r, tc.g, tc.b)
			}
			if c.Hex != tc.hex {
				t.Fatalf("RGB(%s).Hex=%q want %q", tc.hex, c.Hex, tc.hex)
			}
		})
	}
}

// TestRGB_Empty: an unparseable / empty hex must NOT panic and must
// degrade to lipgloss.NoColor{} so a typo in a palette literal can't
// blow up TUI startup.
func TestRGB_Empty(t *testing.T) {
	c := RGB("")
	if c.Lipgloss() != (lipgloss.NoColor{}) {
		t.Fatalf("empty hex: Lipgloss()=%v want NoColor{}", c.Lipgloss())
	}
}

// TestTokyoNight_AllSlotsFilled is the regression that catches a slot
// being added to Palette but forgotten in the palette constructor —
// otherwise the new slot would silently render as terminal-default
// and we'd ship a half-themed UI.
func TestTokyoNight_AllSlotsFilled(t *testing.T) {
	p := TokyoNight()
	if p.Name == "" {
		t.Fatal("Name is empty")
	}
	slots := map[string]Color{
		"Focus":     p.Focus,
		"PopupBox":  p.PopupBox,
		"Selection": p.Selection,
		"Header":    p.Header,
		"Muted":     p.Muted,
		"Accent":    p.Accent,
		"Warn":      p.Warn,
		"Err":       p.Err,
		"OK":        p.OK,
		"Scope":     p.Scope,
		"Filter":    p.Filter,
	}
	for name, c := range slots {
		if c.Hex == "" {
			t.Errorf("%s slot has empty Hex", name)
		}
		if c.R == 0 && c.G == 0 && c.B == 0 && c.Hex != "#000000" {
			t.Errorf("%s slot: Hex %q didn't parse into RGB", name, c.Hex)
		}
	}
}

// TestSetActive_RestoresPrev: pins the "returns previous palette so
// caller can defer-restore" contract; tests rely on this to swap
// palettes without leaking state across cases.
func TestSetActive_RestoresPrev(t *testing.T) {
	prev := Active()
	swapped := SetActive(Palette{Name: "test"})
	if swapped.Name != prev.Name {
		t.Fatalf("SetActive returned %q, want previous %q", swapped.Name, prev.Name)
	}
	SetActive(prev) // restore
	if Active().Name != prev.Name {
		t.Fatalf("restore failed: Active=%q want %q", Active().Name, prev.Name)
	}
}
