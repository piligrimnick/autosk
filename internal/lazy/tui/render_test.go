package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncate_RuneSafe verifies that truncate doesn't slice inside a
// multibyte rune (which would crash for some boundary positions and
// produce mojibake for others). The previous byte-slicing version
// would panic on len("Рефакторинг") > 30 == false but on a longer
// Cyrillic title (e.g. 22 runes = 44 bytes) the byte slice would fall
// mid-codepoint.
func TestTruncate_RuneSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
	}{
		{"ascii_short", "abc", 10},
		{"ascii_long", strings.Repeat("a", 80), 30},
		{"cyrillic_long", strings.Repeat("Рефакторинг ", 4), 30},
		{"emoji_long", strings.Repeat("🚀 ", 20), 10},
		{"cjk_long", strings.Repeat("作業中", 10), 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncate panicked: %v", r)
				}
			}()
			got := truncate(tc.in, tc.n)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8 output: %q", got)
			}
			// utf8.RuneCountInString(input) <= n → unchanged.
			if utf8.RuneCountInString(tc.in) <= tc.n {
				if got != tc.in {
					t.Errorf("unchanged input was mutated: %q→%q", tc.in, got)
				}
				return
			}
			if utf8.RuneCountInString(got) != tc.n {
				t.Errorf("rune count: got %d want %d (output %q)",
					utf8.RuneCountInString(got), tc.n, got)
			}
		})
	}
}

// TestTruncate_NEdge: n < 1 → empty.
func TestTruncate_NEdge(t *testing.T) {
	if got := truncate("hello", 0); got != "" {
		t.Errorf("n=0: got %q want empty", got)
	}
	if got := truncate("hello", -3); got != "" {
		t.Errorf("n<0: got %q want empty", got)
	}
}
