// Package changelog parses the project's Keep-a-Changelog file and
// answers the "what's new since lastSeen?" question that the lazy
// TUI needs to drive its first-run-of-a-new-release popup.
//
// The release CHANGELOG.md at repo root is embedded into the binary
// via go:embed (see embed.go in this package). Authoring is fully
// manual — humans edit CHANGELOG.md before tagging a release; this
// package only reads it. There is no generator in v1.
//
// Format expectation: each release header is
//
//	## [X.Y.Z] - YYYY-MM-DD
//
// (an optional leading "v" inside the brackets is accepted and
// stripped). The body is everything between this header and the next
// "## [" header — typically a list of `### Added` / `### Changed` /
// `### Fixed` / etc. subsections. The "## [Unreleased]" section
// (no date suffix) is intentionally skipped: it is in-flight work
// and we don't want to fire a popup for it.
//
// All parse failures (bad date, missing version, empty body) are
// fail-open: the malformed section is logged via the optional
// SetLogger hook and skipped, the valid neighbours survive. This
// matches the binary-shipping invariant — the popup is a UX nicety
// and must never crash or stall the TUI.
package changelog

import (
	"bufio"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// Entry is one parsed release section.
type Entry struct {
	// Version is the bare semver string (no leading "v"), e.g. "0.3.1".
	Version string
	// Date is the YYYY-MM-DD date parsed from the section header.
	Date time.Time
	// Body is the markdown body BELOW the header (no trailing
	// blank-line padding), suitable for feeding to
	// internal/lazy/markdown.Render together with a synthesised
	// "## Version - Date" line if the caller wants the header
	// rendered too.
	Body string
}

// Header returns the rendered markdown header for e in the
// "## X.Y.Z - YYYY-MM-DD" shape used by the lazy "what's new"
// popup. Note: this intentionally does NOT bracket the version
// like the source CHANGELOG.md does (`## [X.Y.Z] - ...`). The
// brackets would make the markdown renderer resolve the version
// token against the trailing `[X.Y.Z]: https://...compare/...`
// reference-link footer that lives at the bottom of CHANGELOG.md
// — which would inline the compare URL into the visible header
// (e.g. `## 0.1.4 https://github.com/.../compare/v0.1.3...v0.1.4
// - 2026-05-24`). Stripping the brackets here keeps the popup
// header to a plain `## 0.1.4 - 2026-05-24`.
func (e Entry) Header() string {
	return fmt.Sprintf("## %s - %s", e.Version, e.Date.Format("2006-01-02"))
}

// RenderMarkdown returns header + blank line + body for the given
// entries, in slice order. Used by the TUI to compose a single
// markdown blob from one or more Entry values before handing the
// string to internal/lazy/markdown.Render.
func RenderMarkdown(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(e.Header())
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(e.Body, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// logger is the optional sink for malformed-section warnings. nil
// means "drop silently"; tests swap it in to assert that a
// pathological section was actually skipped.
var (
	loggerMu sync.RWMutex
	logger   func(format string, args ...any)
)

// SetLogger installs a log sink for malformed-section warnings.
// Pass nil to silence. Safe for concurrent use.
func SetLogger(fn func(format string, args ...any)) {
	loggerMu.Lock()
	defer loggerMu.Unlock()
	logger = fn
}

func logf(format string, args ...any) {
	loggerMu.RLock()
	fn := logger
	loggerMu.RUnlock()
	if fn != nil {
		fn(format, args...)
	}
}

// Parse splits raw into Entry values, validates each header, and
// returns the surviving entries sorted newest-first by semver
// (lexicographic semver order via golang.org/x/mod/semver).
// Malformed sections are skipped, not surfaced as errors. The only
// error this function returns is reserved for future use — today it
// always returns nil — but keeping the signature lets the embedded
// accessor stay an explicit `(_, error)` pair so a future strict
// mode is a one-line change away.
func Parse(raw []byte) ([]Entry, error) {
	entries := splitSections(string(raw))
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		ent, ok := parseSection(e.header, e.body)
		if !ok {
			continue
		}
		out = append(out, ent)
	}
	sortNewestFirst(out)
	return out, nil
}

// rawSection is a header line + the body that follows it, before
// validation. Pulled out so splitSections doesn't have to allocate
// Entry values for the sections it'll later throw away.
type rawSection struct {
	header string
	body   string
}

// splitSections walks the markdown looking for "## [" headings and
// returns one rawSection per heading (header line + everything
// until the next "## [" heading or EOF).
//
// We use a line scanner so the parser is robust against CRLF inputs
// (Windows-edited CHANGELOG.md). Trailing blank lines on each body
// are trimmed before validation.
func splitSections(s string) []rawSection {
	var (
		out  []rawSection
		cur  *rawSection
		body strings.Builder
	)
	flush := func() {
		if cur == nil {
			return
		}
		cur.body = strings.TrimRight(body.String(), "\n")
		out = append(out, *cur)
		body.Reset()
		cur = nil
	}
	sc := bufio.NewScanner(strings.NewReader(s))
	// 1 MiB cap — the embedded CHANGELOG.md is tiny but a tampered
	// file could pin pathologically long lines; bumping from the
	// default 64 KiB keeps a long URL-only release-link footer
	// from tripping the scanner.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## [") {
			flush()
			cur = &rawSection{header: line}
			continue
		}
		if cur == nil {
			// Pre-amble (the file's intro paragraph). Skip.
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return out
}

// parseSection validates a header line in the shape
//
//	## [X.Y.Z] - YYYY-MM-DD
//
// (allowing "—" or "–" instead of "-" as the date separator; many
// human-edited CHANGELOG files use the Unicode dash). Returns
// (Entry{}, false) on any mismatch and logs the reason.
//
// The "## [Unreleased]" section is recognised and skipped silently
// — it isn't a release, so we don't want a popup on it.
func parseSection(header, body string) (Entry, bool) {
	// Strip the leading "## ".
	rest := strings.TrimPrefix(header, "## ")
	// Find the "[...]" version token.
	if !strings.HasPrefix(rest, "[") {
		logf("changelog: malformed header (no [): %q", header)
		return Entry{}, false
	}
	close := strings.IndexRune(rest, ']')
	if close < 0 {
		logf("changelog: malformed header (no ]): %q", header)
		return Entry{}, false
	}
	versionTok := rest[1:close]
	tail := strings.TrimSpace(rest[close+1:])
	if strings.EqualFold(versionTok, "Unreleased") {
		// Intentionally skipped — not a release.
		return Entry{}, false
	}
	bare := strings.TrimPrefix(versionTok, "v")
	if !semver.IsValid("v" + bare) {
		logf("changelog: invalid semver %q in header %q", versionTok, header)
		return Entry{}, false
	}
	// Tail should be "- YYYY-MM-DD" (or unicode dash variants).
	tail = strings.TrimLeft(tail, "-—–")
	tail = strings.TrimSpace(tail)
	if tail == "" {
		logf("changelog: missing date in header %q", header)
		return Entry{}, false
	}
	date, err := time.Parse("2006-01-02", tail)
	if err != nil {
		logf("changelog: invalid date %q in header %q: %v", tail, header, err)
		return Entry{}, false
	}
	body = strings.TrimRight(body, "\n")
	return Entry{Version: bare, Date: date, Body: body}, true
}

// sortNewestFirst sorts in place by semver, newest first. semver
// strings need the leading "v" added back for x/mod/semver.Compare,
// which is what we do here.
func sortNewestFirst(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		return semver.Compare("v"+es[i].Version, "v"+es[j].Version) > 0
	})
}

// Unseen returns the subset of entries whose version is strictly
// newer than lastSeen, preserving the newest-first order. When
// lastSeen is empty, every entry is returned (first-run path — a
// brand-new operator sees the complete embedded history once).
//
// lastSeen may carry an optional leading "v"; both forms are
// accepted on the wire because operators may have hand-edited
// state.json. An unparseable lastSeen is treated as empty (the
// brand-new operator path), with a log line — silently dropping
// would hide a real bug.
func Unseen(entries []Entry, lastSeen string) []Entry {
	if lastSeen == "" {
		out := make([]Entry, len(entries))
		copy(out, entries)
		return out
	}
	bare := strings.TrimPrefix(lastSeen, "v")
	if !semver.IsValid("v" + bare) {
		logf("changelog: invalid last_seen_changelog %q; treating as first-run", lastSeen)
		out := make([]Entry, len(entries))
		copy(out, entries)
		return out
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if semver.Compare("v"+e.Version, "v"+bare) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// Latest returns the newest entry's version (bare semver, no "v"),
// or "" when entries is empty. Used by the popup-dismiss handler to
// stamp `last_seen_changelog` in `~/.autosk/state.json`.
//
// Assumes entries is already sorted newest-first (Parse guarantees
// that). Doesn't re-sort defensively because the only caller is the
// TUI which gets its slice straight out of Embedded() / Parse().
func Latest(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	return entries[0].Version
}
