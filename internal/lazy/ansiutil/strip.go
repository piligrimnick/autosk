// Package ansiutil holds tiny shared helpers for working with ANSI
// escape sequences in the lazy TUI. Today it carries the single
// Strip function used by every render-test package to compare the
// visible portion of a glamour / lipgloss render without having to
// reason about SGR escapes.
//
// Kept separate from internal/lazy/tui and internal/lazy/markdown so
// both can import it without depending on each other (a circular
// import the other way around — tui imports markdown — would block
// the obvious "co-locate the helper next to its first user" placement).
package ansiutil

import "strings"

// Strip removes ANSI CSI escape sequences (ESC [ … final-byte) from
// s. Pure-Go, allocation-light, and good enough for tests: we only
// need to compare visible content, not validate the escape grammar.
//
// The CSI parameter range is 0x30–0x3f (digits + ?, ;, etc.); a CSI
// terminator is any byte in 0x40–0x7e. We deliberately do NOT treat
// the opening '[' (0x5b, which falls inside the terminator range) as
// a terminator — an earlier in-repo version did, which made every
// 24-bit SGR sequence (\x1b[38;2;R;G;Bm) leak its parameter run
// into the output and broke substring assertions in render tests.
func Strip(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// Skip CSI sequence: ESC [ <params> <final byte>.
			j := i + 2
			for j < len(s) {
				c := s[j]
				if c >= 0x40 && c <= 0x7e {
					j++
					break
				}
				j++
			}
			i = j - 1
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
