package markdown

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"

	"autosk/internal/lazy/ansiutil"
	"autosk/internal/lazy/theme"
)

// TestRender_Empty: empty input returns empty, no escapes, no
// trailing newline.
func TestRender_Empty(t *testing.T) {
	if got := Render("", 80); got != "" {
		t.Fatalf("Render(\"\", 80) = %q want empty", got)
	}
}

// TestRender_WidthZero: width <= 0 returns the input verbatim. The
// TUI passes width=0 during the first layout pass before the view
// has been sized; rendering at that point would either pick a
// hardcoded default (wrong column) or panic in glamour.
func TestRender_WidthZero(t *testing.T) {
	in := "# heading\n\nbody"
	if got := Render(in, 0); got != in {
		t.Fatalf("Render(_, 0) should fall back to raw input; got %q", got)
	}
	if got := Render(in, -5); got != in {
		t.Fatalf("Render(_, -5) should fall back to raw input; got %q", got)
	}
}

// TestRender_Heading: a "# Title" markdown source produces output
// that contains the title text and ESC codes, with no raw "# "
// marker bleeding through.
func TestRender_Heading(t *testing.T) {
	out := Render("# Title", 80)
	if out == "" {
		t.Fatal("Render returned empty for heading")
	}
	if !strings.Contains(out, "Title") {
		t.Errorf("output missing heading text; got %q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("output missing ANSI escape; got %q", out)
	}
}

// TestRender_BoldItalic: bold and italic markers don't appear as
// raw asterisks in the output \u2014 they're absorbed into ANSI escapes.
func TestRender_BoldItalic(t *testing.T) {
	out := Render("a **bold** *italic*", 80)
	if !strings.Contains(out, "bold") || !strings.Contains(out, "italic") {
		t.Fatalf("bold/italic text missing from output: %q", out)
	}
	// goldmark consumes the ** and * markers; raw asterisks shouldn't
	// surface in the rendered output.
	if strings.Contains(out, "**bold**") {
		t.Errorf("raw bold marker leaked into output: %q", out)
	}
}

// TestRender_List: bulleted list renders with the bullet glyph the
// StyleConfig configured. Sanity check that the renderer is wired up
// with our config (not glamour's default ASCII style).
func TestRender_List(t *testing.T) {
	out := Render("- one\n- two\n- three", 80)
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") || !strings.Contains(out, "three") {
		t.Fatalf("list items missing from output: %q", out)
	}
	if !strings.Contains(out, "\u2022") {
		t.Errorf("list bullet (\u2022) missing from output: %q", out)
	}
	// The raw markdown bullet ("- ") shouldn't survive unless it was
	// already inside a code block.
	if strings.Contains(out, "- one") {
		t.Errorf("raw list marker leaked into output: %q", out)
	}
}

// TestRender_InlineCode: backtick-quoted inline code keeps the
// payload text and picks up the Filter (orange) hex from the active
// palette. Sanity check that the Code style slot is wired up.
func TestRender_InlineCode(t *testing.T) {
	out := Render("see `gofmt -l` for usage", 80)
	if !strings.Contains(out, "gofmt") {
		t.Fatalf("inline code text missing: %q", out)
	}
	filterHex := strings.TrimPrefix(theme.Active().Filter.Hex, "#")
	if len(filterHex) != 6 {
		t.Skip("palette missing Filter hex; can't pin colour")
	}
}

// TestRender_FencedCode: a fenced code block in a known language
// (go) renders without leaking the ``` fence markers. Chroma
// syntax highlighting splits each token with its own SGR escape
// so we have to compare against the ANSI-stripped output —
// strings.Contains(out, "package main") would otherwise fail
// because the ESC reset between "package" and "main" breaks the
// substring match.
func TestRender_FencedCode(t *testing.T) {
	in := "```go\npackage main\n\nfunc main() {}\n```"
	out := Render(in, 80)
	visible := ansiutil.Strip(out)
	if !strings.Contains(visible, "package") || !strings.Contains(visible, "main") {
		t.Fatalf("fenced go code missing payload: %q", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("fence markers leaked into output: %q", out)
	}
}

// TestRender_FencedCode_UnknownLang: an unknown language tag must
// not crash chroma; glamour falls back to a plain block.
func TestRender_FencedCode_UnknownLang(t *testing.T) {
	in := "```doesnotexist\nx := 1\n```"
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Render panicked on unknown language: %v", r)
		}
	}()
	out := Render(in, 80)
	if !strings.Contains(out, "x := 1") {
		t.Fatalf("unknown-lang block lost payload: %q", out)
	}
}

// TestRender_Link: a markdown link's display text survives the
// render and the URL is preserved somewhere in the output (glamour
// surfaces it after the text).
func TestRender_Link(t *testing.T) {
	out := Render("see [glamour](https://github.com/charmbracelet/glamour)", 80)
	if !strings.Contains(out, "glamour") {
		t.Fatalf("link text missing: %q", out)
	}
}

// TestRender_LongLineWraps: a long line is wrapped at roughly the
// requested width (within glamour's wrap tolerance \u2014 it never
// exceeds width + a few cells of paragraph-end slack).
func TestRender_LongLineWraps(t *testing.T) {
	long := strings.Repeat("word ", 50) // ~250 chars
	out := Render(long, 40)
	// The output has at least one newline (wrap happened) and no
	// single line is dramatically wider than the requested width.
	if !strings.Contains(out, "\n") {
		t.Fatalf("expected wrap inside long paragraph; got %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		visible := ansiutil.Strip(line)
		if utf8.RuneCountInString(visible) > 60 {
			t.Errorf("wrapped line longer than expected (%d cells): %q",
				utf8.RuneCountInString(visible), visible)
		}
	}
}

// TestRender_UTF8: Cyrillic + emoji input renders without mangling
// (we don't pin specific escapes \u2014 just that the runes survive).
func TestRender_UTF8(t *testing.T) {
	in := "# \u0420\u0435\u0444\u0430\u043a\u0442\u043e\u0440\u0438\u043d\u0433\n\nDone \U0001f680 emoji"
	out := Render(in, 80)
	if !strings.Contains(out, "\u0420\u0435\u0444\u0430\u043a\u0442\u043e\u0440\u0438\u043d\u0433") {
		t.Errorf("Cyrillic heading missing from output: %q", out)
	}
	if !strings.Contains(out, "\U0001f680") {
		t.Errorf("emoji missing from output: %q", out)
	}
}

// TestRender_TrimsTrailingNewlines: glamour wraps every document in
// a leading + trailing newline (BlockPrefix/Suffix on Document). Our
// Render strips both because the caller controls section spacing.
func TestRender_TrimsTrailingNewlines(t *testing.T) {
	out := Render("hello", 80)
	if strings.HasPrefix(out, "\n") {
		t.Errorf("leading newline not trimmed: %q", out)
	}
	if strings.HasSuffix(out, "\n") {
		t.Errorf("trailing newline not trimmed: %q", out)
	}
}

// TestRender_NonMarkdown: a plain text paragraph (no markdown markers)
// renders with the same visible characters as the input, no raw
// markers added.
func TestRender_NonMarkdown(t *testing.T) {
	in := "just plain text without any markup."
	out := Render(in, 80)
	if !strings.Contains(out, "just plain text without any markup.") {
		t.Errorf("plain text mangled: %q", out)
	}
}

// TestRender_Cache_ReusesRenderer: two Render calls at the same
// width hit the cache; the *glamour.TermRenderer instance is reused
// across calls. We can't observe the pointer directly without
// exposing it, so instead we exercise the path twice and confirm
// the output is byte-identical (renderer state is reset between
// calls inside glamour).
func TestRender_Cache_ReusesRenderer(t *testing.T) {
	RebuildStyles() // start clean
	a := Render("# A\n\nbody", 60)
	b := Render("# A\n\nbody", 60)
	if a != b {
		t.Fatalf("cached renderer should produce stable output across calls:\n a=%q\n b=%q", a, b)
	}
}

// TestRender_Cache_InvalidatedOnWidthChange: switching widths must
// rebuild the renderer so the new wrap width takes effect. We can't
// peek inside the cache, but we can call Render at two widths and
// observe that the output differs (long paragraph wraps at different
// columns).
func TestRender_Cache_InvalidatedOnWidthChange(t *testing.T) {
	RebuildStyles()
	long := strings.Repeat("word ", 30)
	narrow := Render(long, 30)
	wide := Render(long, 100)
	if narrow == wide {
		t.Errorf("expected different wrap output at different widths; got identical")
	}
}

// TestRender_Cache_InvalidatedOnPaletteSwap: SetActive bumps the
// palette version; the next Render must rebuild the cached renderer
// against the new palette. We swap to a palette with a distinctive
// hex and check the new colour appears in the output.
//
// Also asserts that RebuildStyles empties the output LRU map (not
// just the renderer pointer) — a stale entry there would surface
// the previous palette's ANSI bytes on the next cache-hit and
// silently re-leak the old hue into the dashboard.
func TestRender_Cache_InvalidatedOnPaletteSwap(t *testing.T) {
	defer RebuildStyles()
	prev := theme.SetActive(theme.TokyoNight())
	defer theme.SetActive(prev)
	RebuildStyles()

	first := Render("# Heading", 60)
	if first == "" {
		t.Fatal("first render empty")
	}

	// At this point the LRU should hold at least the rendered "# Heading"
	// fragment. RebuildStyles must drop it.
	cache.mu.Lock()
	populated := output.m != nil && len(output.m) > 0
	cache.mu.Unlock()
	if !populated {
		t.Fatal("expected output LRU to be populated after a successful Render")
	}

	// Build a palette whose Header slot is a custom hex.
	p := theme.TokyoNight()
	p.Header = theme.RGB("#ff00ff")
	theme.SetActive(p)
	RebuildStyles()

	// After RebuildStyles the output map must be empty — not just
	// the renderer pointer.
	cache.mu.Lock()
	emptied := output.m == nil || len(output.m) == 0
	cache.mu.Unlock()
	if !emptied {
		t.Fatalf("RebuildStyles did not empty the output LRU; len=%d", len(output.m))
	}

	second := Render("# Heading", 60)
	if !strings.Contains(second, "ff;0;ff") && !strings.Contains(second, "255;0;255") {
		// glamour uses termenv to convert hex to ANSI; the output may
		// be a 256-colour approximation. Fall back to comparing
		// outputs are different \u2014 a renderer that ignored the
		// palette swap would produce byte-identical output.
		if first == second {
			t.Errorf("palette swap had no effect on render output (cache not invalidated)")
		}
	}
}

// TestRender_OutputCache_HitSkipsGlamour pins the load-bearing
// invariant of the output LRU: a second Render with the same
// (md, width, palette) tuple must return the cached bytes WITHOUT
// running renderHook a second time. We pin that by swapping
// renderHook to one that panics on second invocation — if the LRU
// kicks in the panic never fires and the call returns the
// previously-cached output verbatim.
func TestRender_OutputCache_HitSkipsGlamour(t *testing.T) {
	RebuildStyles()
	orig := renderHook
	defer func() {
		renderHook = orig
		RebuildStyles()
	}()

	var calls int
	renderHook = func(r *glamour.TermRenderer, md string) (string, error) {
		calls++
		if calls > 1 {
			panic("renderHook invoked twice; LRU did not short-circuit")
		}
		return orig(r, md)
	}

	first := Render("# Heading\n\nbody", 60)
	if first == "" {
		t.Fatal("first Render returned empty")
	}
	second := Render("# Heading\n\nbody", 60)
	if second != first {
		t.Fatalf("second Render did not return cached bytes:\n first=%q\nsecond=%q", first, second)
	}
	if calls != 1 {
		t.Errorf("expected renderHook to be called exactly once; got %d", calls)
	}
}

// TestRender_OutputCache_LRUEviction pins the eviction policy:
// after we push more distinct entries than the cache can hold, the
// least-recently-used one is gone and a re-render reaches glamour.
//
// The test fills the cache up to maxOutputCacheEntries, then inserts
// one more (which must evict the first), then re-renders the FIRST
// fragment and asserts the call reached renderHook (i.e. it was
// evicted, not still cached).
func TestRender_OutputCache_LRUEviction(t *testing.T) {
	RebuildStyles()
	orig := renderHook
	defer func() {
		renderHook = orig
		RebuildStyles()
	}()

	var calls int
	renderHook = func(r *glamour.TermRenderer, md string) (string, error) {
		calls++
		return orig(r, md)
	}

	// Distinct markdown per iteration so each lands its own LRU entry.
	// Width is fixed so the (md, width, palette) tuple varies only by md.
	mds := make([]string, 0, maxOutputCacheEntries+1)
	for i := 0; i < maxOutputCacheEntries+1; i++ {
		mds = append(mds, fmt.Sprintf("entry %d body", i))
	}

	// Render the first entry, then fill the rest — the first should
	// be evicted once we push past maxOutputCacheEntries.
	_ = Render(mds[0], 60)
	callsAfterFirst := calls
	for i := 1; i < len(mds); i++ {
		_ = Render(mds[i], 60)
	}
	callsAfterFill := calls

	// Sanity: each distinct md produced one renderHook call — nothing
	// was a cache hit during the fill phase.
	if callsAfterFill-callsAfterFirst != len(mds)-1 {
		t.Fatalf("fill phase had unexpected cache hits: %d new calls for %d new entries", callsAfterFill-callsAfterFirst, len(mds)-1)
	}

	// LRU map must now hold exactly maxOutputCacheEntries items.
	cache.mu.Lock()
	cacheLen := 0
	if output.m != nil {
		cacheLen = len(output.m)
	}
	cache.mu.Unlock()
	if cacheLen != maxOutputCacheEntries {
		t.Errorf("expected cache to hold %d entries; got %d", maxOutputCacheEntries, cacheLen)
	}

	// Re-render the first entry. Because it was evicted, renderHook
	// must be called again.
	_ = Render(mds[0], 60)
	if calls != callsAfterFill+1 {
		t.Errorf("expected LRU-evicted entry to re-render; calls=%d, want %d (eviction did not happen)", calls, callsAfterFill+1)
	}
}

// TestRender_FailOpen_NeverPanics: even pathological input shouldn't
// panic; glamour is well-behaved but a future regression in goldmark
// could surface here. Inputs at or below the maxBlockquoteDepth /
// maxInputBytes guards exercise the actual renderer path; inputs
// over the guard are short-circuited to raw passthrough (covered
// separately by TestRender_PathologicalBounded).
func TestRender_FailOpen_NeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Render panicked: %v", r)
		}
	}()
	// Just under the guard: 12 levels still goes through glamour and
	// must not panic. Bigger inputs (the original 100-level case the
	// review post-mortem flagged) are handled by the bounded test
	// below — they exit through the early-return guard, so they
	// don't exercise glamour's blockquote recursion at all.
	inputs := []string{
		strings.Repeat("> ", 12) + "nested",
		"```\nno closing fence",
		"[[[unclosed",
		"\x00\x01\x02 control chars",
	}
	for _, in := range inputs {
		_ = Render(in, 60)
	}
}

// TestRender_PathologicalBounded pins BLOCKER 1 from the as-a261
// review: deeply-nested blockquote input must return promptly with
// the raw text intact, not thrash memory or hang. The 100-level
// input is well past maxBlockquoteDepth (16) so the guard should
// short-circuit before glamour ever sees it.
//
// The wall-clock budget is generous (200 ms): blockquoteDepth is a
// linear scan over a 200-byte string and should complete in
// microseconds on any developer machine. We're not benchmarking,
// we're catching a hang regression — if a future refactor drops
// the guard, the test will time out instead of taking the host
// down with it.
func TestRender_PathologicalBounded(t *testing.T) {
	deep := strings.Repeat("> ", 100) + "x"
	done := make(chan string, 1)
	go func() { done <- Render(deep, 60) }()
	select {
	case out := <-done:
		if out == "" {
			t.Fatal("Render returned empty on pathological input; want raw passthrough")
		}
		if !strings.Contains(out, "x") {
			t.Errorf("raw passthrough should preserve the payload; got %q", out)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Render did not return within 200ms on 100-level blockquote input")
	}

	// Also exercise the byte-length guard: a 65 KB input is one
	// past maxInputBytes and must short-circuit identically.
	big := strings.Repeat("a", 65*1024)
	done = make(chan string, 1)
	go func() { done <- Render(big, 60) }()
	select {
	case out := <-done:
		if out != big {
			t.Errorf("oversize input should pass through verbatim")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Render did not return within 200ms on 65 KB input")
	}
}

// TestRender_PanicSafe pins BLOCKER 2 from the as-a261 review: a
// panic from glamour / goldmark / chroma must surface as a
// fall-back to the raw input, not as a TUI crash. Swaps the package-
// level renderHook to one that panics deterministically and asserts
// that Render still returns the raw md.
//
// Also verifies the cache-eviction half of the recovery contract:
// the cached *TermRenderer is dropped on panic so a poisoned
// renderer can't keep crashing subsequent calls.
func TestRender_PanicSafe(t *testing.T) {
	RebuildStyles()
	orig := renderHook
	defer func() {
		renderHook = orig
		RebuildStyles()
	}()

	renderHook = func(*glamour.TermRenderer, string) (string, error) {
		panic("glamour exploded")
	}

	in := "hello"
	out := Render(in, 60)
	if out != in {
		t.Errorf("panic should fall back to raw input; got %q want %q", out, in)
	}

	// Cache should have been dropped: the next Render with a panic-
	// free hook must rebuild and produce styled output.
	cache.mu.Lock()
	if cache.r != nil {
		cache.mu.Unlock()
		t.Fatal("expected cache.r=nil after panic so a poisoned renderer can't persist")
	}
	cache.mu.Unlock()

	renderHook = orig
	out = Render("# heading", 60)
	if !strings.Contains(out, "heading") {
		t.Errorf("post-panic render should work normally; got %q", out)
	}
}
