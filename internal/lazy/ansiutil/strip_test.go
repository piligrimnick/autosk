package ansiutil

import (
	"strings"
	"testing"
)

func TestStrip_NoEscapes(t *testing.T) {
	in := "plain text without escapes"
	if got := Strip(in); got != in {
		t.Errorf("Strip(%q) = %q, want unchanged", in, got)
	}
}

func TestStrip_BasicSGR(t *testing.T) {
	in := "\x1b[31mred\x1b[0m"
	want := "red"
	if got := Strip(in); got != want {
		t.Errorf("Strip = %q, want %q", got, want)
	}
}

// TestStrip_TrueColorSGR pins the original regression: an earlier
// implementation treated the opening '[' of a CSI as a terminator,
// which made every 24-bit SGR (\x1b[38;2;R;G;Bm) leak its
// "38;2;R;G;B" parameter run into the visible output.
func TestStrip_TrueColorSGR(t *testing.T) {
	in := "\x1b[38;2;255;128;0morange\x1b[0m"
	got := Strip(in)
	if got != "orange" {
		t.Errorf("Strip = %q, want %q", got, "orange")
	}
	if strings.Contains(got, "38") || strings.Contains(got, "255") {
		t.Errorf("Strip leaked SGR parameters: %q", got)
	}
}

func TestStrip_MultipleSequences(t *testing.T) {
	in := "\x1b[1;34mhello\x1b[0m \x1b[32mworld\x1b[0m"
	want := "hello world"
	if got := Strip(in); got != want {
		t.Errorf("Strip = %q, want %q", got, want)
	}
}

// TestStrip_EmptyAndStrayESC: degenerate inputs don't crash.
func TestStrip_EmptyAndStrayESC(t *testing.T) {
	if got := Strip(""); got != "" {
		t.Errorf("Strip(\"\") = %q, want empty", got)
	}
	// Lone ESC with no '[' should pass through.
	if got := Strip("\x1bfoo"); got != "\x1bfoo" {
		t.Errorf("Strip(stray ESC) = %q, want pass-through", got)
	}
	// ESC [ with no terminator — should consume to end of string and
	// not produce visible bytes from the params.
	if got := Strip("\x1b[31;"); got != "" {
		t.Errorf("Strip(unterminated CSI) = %q, want empty", got)
	}
}
