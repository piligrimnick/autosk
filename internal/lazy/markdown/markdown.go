// Package markdown renders user-supplied markdown to ANSI for the
// lazy TUI detail pane.
//
// The render path is glamour (goldmark + chroma + bluemonday) wired
// to a StyleConfig derived from theme.Active(), so a palette swap
// at runtime keeps headings / links / code blocks in the same hues
// as the rest of the TUI.
//
// This package is the TUI-only seam: machine-facing wire formats
// (the `--json` CLI output, the proto-v2 JSON-RPC wire types, and the
// pi-format session transcript) stay on raw UTF-8 plain text and never
// call into here.
//
// The renderer is cached by (width, paletteVersion) and rebuilt on
// the next Render call when either changes. The cache lives in this
// package so callers (TUI render code, tests) don't need to plumb a
// renderer handle around — they just call Render and get an ANSI
// string back.
//
// Fail-open contract: if the input is obviously pathological, if
// glamour cannot construct a renderer, if it errors mid-render, or
// if it panics, Render returns the input markdown verbatim instead
// of erroring or panicking. A raw markdown string in the detail
// pane is always preferable to a blank, crashed, or frozen TUI.
package markdown

import (
	"container/list"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"

	"autosk/internal/lazy/theme"
)

// maxRenderWidth is the absolute cap on the wrap width passed to
// glamour, regardless of how wide the detail pane is. Lifted from the
// original 120 to 500 so wide terminals (ultrawide / 2K+) actually
// fill the labeled-box body to the right border instead of leaving a
// dead column of padding inside the frame. The cap is still here to
// protect glamour from being asked to lay out at silly widths
// (megapixel sized panes from a misconfigured Gui); 500 cells is
// well above any real terminal layout while staying inside glamour's
// tested-sane envelope.
const maxRenderWidth = 500

// Pathological-input guards. Glamour's blockquote renderer recurses
// per nesting level and allocates a fresh wrap buffer at every layer,
// so a few dozen "> " prefixes balloon RSS into the tens of
// gigabytes before any error surfaces — the operator's machine
// thrashes long before our defer recover() gets a chance to fire.
// (The original investigation report on as-a261 measured 27 GB peak
// RSS for 100 levels.) Length cap covers a similar shape for inputs
// that aren't blockquote-deep but are just bulk text the agent
// produced in a runaway loop.
//
// These are intentionally low: the typical task description is
// hundreds of bytes and nests blockquotes one or two levels at most
// (an agent quoting a previous comment, say). Anything past these
// thresholds is overwhelmingly more likely to be pathological than
// legitimate, and the fail-open path still surfaces the raw text so
// the operator can read it.
const (
	maxBlockquoteDepth = 16
	maxInputBytes      = 64 * 1024
)

// rendererCache holds the cached *glamour.TermRenderer and the
// (width, paletteVersion) tuple it was built against. Guarded by mu
// so concurrent renders from the spinner / refresh goroutines don't
// race; the renderer itself is not goroutine-safe (it holds a shared
// goldmark buffer), so Render takes mu for the whole conversion.
//
// The same mu also guards the output LRU below: a palette swap that
// drops the renderer must drop the cached output bytes atomically,
// otherwise the next Render of a previously-cached fragment could
// return ANSI escapes built against the OLD palette while the rest
// of the TUI has flipped to the new one.
type rendererCache struct {
	mu             sync.Mutex
	r              *glamour.TermRenderer
	width          int
	paletteVersion uint64
}

var cache rendererCache

// maxOutputCacheEntries caps the rendered-output LRU. A typical
// `autosk lazy` session browses a few dozen tasks; each contributes
// 1 + N fragments to the cache (description + every non-empty
// comment in the thread — the Detail pane has no display cap, see
// renderTaskDetail). 64 is enough to hold the working set on a
// short-thread workload (a handful of tasks × a few comments each)
// without spending memory on rarely-revisited entries; on a
// long-thread workload a single 60+ comment task can saturate the
// LRU on its own and cause re-renders when revisiting after
// browsing other tasks. If that becomes a measurable problem,
// bumping this cap (e.g. 256) is the simplest mitigation.
//
// Setting this to 0 effectively disables the LRU — every Render
// re-runs glamour. Lifted out as a const so a future regression in
// a specific corner case can be mitigated without code changes.
const maxOutputCacheEntries = 64

// outputKey is the LRU lookup tuple. paletteVersion is included so a
// runtime palette swap can't return ANSI bytes baked against the
// previous hues (RebuildStyles drops the whole cache anyway, but
// keying on the version is a belt-and-braces second line of
// defence — and lets us drop the explicit reset in the future if
// callers ever forget to call RebuildStyles).
type outputKey struct {
	md             string
	width          int
	paletteVersion uint64
}

// outputCacheEntry is the list element payload: we keep the key
// alongside the value so eviction can delete the map entry without
// a reverse lookup.
type outputCacheEntry struct {
	key outputKey
	val string
}

// outputLRU is a bounded LRU of rendered ANSI fragments, keyed by
// (md, width, paletteVersion). On hit we return the cached bytes
// verbatim, skipping the entire goldmark → chroma → glamour
// pipeline; on miss we render normally and stash the result. The
// list orders entries MRU-front so eviction picks the back.
//
// All access is guarded by cache.mu (shared with the renderer
// cache); the LRU itself is not goroutine-safe.
type outputLRU struct {
	m  map[outputKey]*list.Element
	ll *list.List // front = most-recently-used
}

var output outputLRU

// outputCacheGet returns the cached fragment for k and promotes it
// to MRU on hit. Returns ("", false) on miss. Must be called with
// cache.mu held.
func outputCacheGet(k outputKey) (string, bool) {
	if output.m == nil {
		return "", false
	}
	e, ok := output.m[k]
	if !ok {
		return "", false
	}
	output.ll.MoveToFront(e)
	return e.Value.(outputCacheEntry).val, true
}

// outputCachePut stores v under k and evicts the LRU entry when the
// cache grows past maxOutputCacheEntries. A repeated put for the
// same key updates the value in place and re-promotes to MRU. Must
// be called with cache.mu held.
//
// No-op when maxOutputCacheEntries == 0 so the cache can be
// disabled by a one-line change to the const.
func outputCachePut(k outputKey, v string) {
	if maxOutputCacheEntries <= 0 {
		return
	}
	if output.m == nil {
		output.m = make(map[outputKey]*list.Element, maxOutputCacheEntries)
		output.ll = list.New()
	}
	if e, ok := output.m[k]; ok {
		e.Value = outputCacheEntry{key: k, val: v}
		output.ll.MoveToFront(e)
		return
	}
	e := output.ll.PushFront(outputCacheEntry{key: k, val: v})
	output.m[k] = e
	for output.ll.Len() > maxOutputCacheEntries {
		old := output.ll.Back()
		if old == nil {
			break
		}
		output.ll.Remove(old)
		delete(output.m, old.Value.(outputCacheEntry).key)
	}
}

// outputCacheReset drops every cached fragment. Called from
// RebuildStyles (so a palette swap doesn't leak old hues) and from
// the panic-recovery path (a poisoned renderer may have stored a
// half-baked entry). Must be called with cache.mu held.
func outputCacheReset() {
	output.m = nil
	output.ll = nil
}

// renderHook is the actual glamour render call, split out so the
// panic-safe wrapper in Render can be exercised in tests by swapping
// the hook to one that panics deterministically. Production code
// never reassigns it.
var renderHook = func(r *glamour.TermRenderer, md string) (string, error) {
	return r.Render(md)
}

// Render returns md rendered as ANSI for a detail-pane viewport of
// width cells. Width is clamped to the global cap (maxRenderWidth)
// before being passed to glamour, and a width of 0 (which the TUI
// passes during the first layout pass, before the view has been
// sized) returns md verbatim so the pane still renders something.
//
// Fail-open on three layers: oversize / deeply-nested input is
// rejected up front and returned verbatim; a glamour build / render
// error returns the raw md; a panic anywhere inside glamour is
// recovered, the (possibly poisoned) cached renderer is dropped, and
// the raw md is returned. The TUI is never crashed by user-supplied
// markdown.
func Render(md string, width int) (out string) {
	if md == "" {
		return ""
	}
	if width <= 0 {
		return md
	}
	if width > maxRenderWidth {
		width = maxRenderWidth
	}

	// Cheap pathological-input guards — see the const block above.
	// Done before we take the cache lock so a misbehaving comment
	// doesn't even briefly block other render calls.
	if len(md) > maxInputBytes {
		return md
	}
	if blockquoteDepth(md) > maxBlockquoteDepth {
		return md
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Panic-safe wrapper: any panic from glamour / goldmark / chroma
	// surfaces as a fall-back to raw md instead of crashing the TUI
	// manager loop. The cached renderer is dropped because a panic
	// mid-render may have left its internal buffers in an
	// inconsistent state; the next call rebuilds it. The output LRU
	// is dropped too — a poisoned renderer can't have written into
	// it on THIS call (we cache after a successful render), but the
	// cheapest robust contract is "on any panic anywhere in the
	// render path, both halves of cache go." Both writes run while
	// we still hold cache.mu (Unlock was deferred first and runs
	// after this recover).
	defer func() {
		if r := recover(); r != nil {
			cache.r = nil
			outputCacheReset()
			out = md
		}
	}()

	pv := theme.Version()

	// Output LRU hit: skip the entire goldmark → chroma → glamour
	// pipeline and return the cached bytes verbatim. paletteVersion
	// is part of the key so a stale-palette read is impossible even
	// if RebuildStyles is somehow skipped on a palette swap.
	key := outputKey{md: md, width: width, paletteVersion: pv}
	if v, ok := outputCacheGet(key); ok {
		return v
	}

	if cache.r == nil || cache.width != width || cache.paletteVersion != pv {
		r, err := newRenderer(width)
		if err != nil {
			return md
		}
		cache.r = r
		cache.width = width
		cache.paletteVersion = pv
	}

	rendered, err := renderHook(cache.r, md)
	if err != nil {
		return md
	}
	// Glamour wraps every render in a Document BlockPrefix/Suffix of
	// "\n", which produces a leading blank line and a trailing one.
	// In the detail pane the section header ("─ description ─")
	// already provides the leading break, and the trailing newline
	// double-spaces against the following section ("─ comments ─")
	// so it has to go. Trim both ends and let the caller decide on
	// spacing.
	rendered = strings.TrimRight(rendered, "\n")
	rendered = strings.TrimLeft(rendered, "\n")
	outputCachePut(key, rendered)
	return rendered
}

// RebuildStyles invalidates the cached renderer AND the output LRU
// so the next Render call picks up the active palette. The TUI
// calls this after theme.SetActive + tui.RebuildStyles so all three
// caches (lipgloss styles, the markdown renderer, gocui frame
// colours) flip in lockstep on a palette swap.
//
// The LRU clear is load-bearing: cached fragments encode the
// previous palette's hex colours as inline ANSI escapes, so reusing
// them after a swap would leak the old hues into the freshly-tinted
// dashboard.
func RebuildStyles() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.r = nil
	outputCacheReset()
}

// newRenderer constructs a glamour TermRenderer wired to the current
// theme palette. Kept separate from Render so tests can exercise the
// construction path without going through the cache.
//
// Intentionally NOT calling glamour.WithEmoji(): the plan called for
// CommonMark + chroma, not GFM+emoji. Enabling the emoji extension
// would rewrite ":shortname:" patterns into glyphs across every task
// description / comment, which is surprising for inputs that use the
// colon-word-colon shape as a label (":commit_msg:", ":todo:"). Raw
// UTF-8 emoji (🚀) still renders fine through goldmark's normal
// text path without the extension.
func newRenderer(width int) (*glamour.TermRenderer, error) {
	cfg := StyleConfig(theme.Active())
	return glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
	)
}

// blockquoteDepth returns the maximum number of leading '>' markers
// found on any single line of md, treating CommonMark blockquote
// nesting as a per-line count of "> " (or ">") prefixes after
// optional indentation. Used by Render as a cheap pathological-input
// guard against the deeply-nested-blockquote OOM trap. A line like
// "  > > > nested" returns 3.
//
// Cheap on legitimate input: bails out of the prefix loop as soon as
// any non-prefix character is seen, so a 50-KB description with a
// single ">" quote scans in microseconds.
func blockquoteDepth(md string) int {
	maxd, d := 0, 0
	inPrefix := true
	for i := 0; i < len(md); i++ {
		c := md[i]
		if c == '\n' {
			if d > maxd {
				maxd = d
			}
			d = 0
			inPrefix = true
			continue
		}
		if !inPrefix {
			continue
		}
		switch c {
		case ' ', '\t':
			// Skip indentation, stay in prefix mode.
		case '>':
			d++
		default:
			inPrefix = false
		}
	}
	if d > maxd {
		maxd = d
	}
	return maxd
}
