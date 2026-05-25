package changelog

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/mod/semver"
)

const sampleKeepAChangelog = `# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- in-flight: foo bar baz.

## [0.2.0] - 2026-03-15

### Added
- New rocket-launch command.
- More cowbell.

### Changed
- Renamed ` + "`shoes`" + ` to ` + "`boots`" + `.

### Fixed
- Off-by-one in the rocket telemetry.

## [0.1.1] - 2026-03-01

### Fixed
- Initial release shipped with a typo in --help.

## [0.1.0] - 2026-02-15

### Added
- First public release.
`

func TestParse_KeepAChangelog(t *testing.T) {
	entries, err := Parse([]byte(sampleKeepAChangelog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := len(entries), 3; got != want {
		t.Fatalf("entries=%d want %d (Unreleased must be filtered out)", got, want)
	}
	// Newest-first ordering.
	want := []string{"0.2.0", "0.1.1", "0.1.0"}
	for i, w := range want {
		if entries[i].Version != w {
			t.Errorf("entries[%d].Version=%q want %q", i, entries[i].Version, w)
		}
	}
	// Date parsing.
	if got := entries[0].Date.Format("2006-01-02"); got != "2026-03-15" {
		t.Errorf("entries[0].Date=%s want 2026-03-15", got)
	}
	// Body carries the section content (sans header line).
	if !strings.Contains(entries[0].Body, "rocket-launch") {
		t.Errorf("entries[0].Body missing 'rocket-launch':\n%s", entries[0].Body)
	}
	if strings.Contains(entries[0].Body, "Unreleased") {
		t.Errorf("entries[0].Body leaked the Unreleased header:\n%s", entries[0].Body)
	}
	// Pinned ordering: entries[1].Body must be the 0.1.1 body.
	if !strings.Contains(entries[1].Body, "typo in --help") {
		t.Errorf("entries[1].Body=\n%s\nexpected the 0.1.1 body", entries[1].Body)
	}
}

func TestParse_SkipsMalformedSections(t *testing.T) {
	// Capture log lines so we can prove the skip was loud (it's
	// fail-open but must NOT be silent).
	var logged []string
	var mu sync.Mutex
	SetLogger(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, format)
	})
	defer SetLogger(nil)

	in := `## [0.3.0] - 2026-04-01

### Added
- good.

## [0.2.0] - NOT-A-DATE

### Fixed
- bad date — section must be dropped.

## [0.1.0] - 2026-02-15

### Added
- another good one.
`
	entries, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(entries); got != 2 {
		t.Fatalf("entries=%d want 2 (the bad-date section must be dropped)", got)
	}
	want := []string{"0.3.0", "0.1.0"}
	for i, w := range want {
		if entries[i].Version != w {
			t.Errorf("entries[%d].Version=%q want %q", i, entries[i].Version, w)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) == 0 {
		t.Errorf("malformed section was skipped silently; SetLogger never fired")
	}
}

// TestParse_AcceptsUnicodeDashSeparator pins review R4: the
// parser accepts ASCII "-", em-dash "—" and en-dash "–" between
// the version token and the date. The currently-embedded
// CHANGELOG.md uses em-dashes, so Embedded() depends on this
// branch every time the binary starts; this test pins the
// invariant directly instead of through the embedded blob (which
// could quietly switch to ASCII hyphens and drop the unicode-dash
// coverage).
func TestParse_AcceptsUnicodeDashSeparator(t *testing.T) {
	for _, sep := range []string{"-", "—", "–"} {
		t.Run(sep, func(t *testing.T) {
			in := fmt.Sprintf("## [0.3.0] %s 2026-04-01\n\n### Added\n- ok.\n", sep)
			es, err := Parse([]byte(in))
			if err != nil {
				t.Fatalf("Parse(sep=%q): %v", sep, err)
			}
			if len(es) != 1 || es[0].Version != "0.3.0" {
				t.Errorf("sep=%q parse failed: %+v", sep, es)
			}
			if got := es[0].Date.Format("2006-01-02"); got != "2026-04-01" {
				t.Errorf("sep=%q date=%q want 2026-04-01", sep, got)
			}
		})
	}
}

func TestParse_AcceptsLeadingVPrefix(t *testing.T) {
	in := `## [v0.3.0] - 2026-04-01

### Added
- ok.
`
	entries, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	if entries[0].Version != "0.3.0" {
		t.Errorf("Version=%q want bare 0.3.0 (leading v stripped)", entries[0].Version)
	}
}

func TestUnseen_SemverOrdering(t *testing.T) {
	entries := []Entry{
		{Version: "0.10.0", Date: mustDate(t, "2026-05-01")},
		{Version: "0.9.9", Date: mustDate(t, "2026-04-15")},
		{Version: "0.4.0", Date: mustDate(t, "2026-03-01")},
		{Version: "0.3.0", Date: mustDate(t, "2026-02-01")},
		{Version: "0.1.0", Date: mustDate(t, "2026-01-01")},
	}
	// Sanity: 0.10.0 must rank higher than 0.9.9 (semver, not
	// lexicographic ordering). sortNewestFirst already enforces
	// it; we re-call it here so the test pins the invariant the
	// parser relies on.
	sortNewestFirst(entries)
	if entries[0].Version != "0.10.0" {
		t.Fatalf("sortNewestFirst put %q first; want 0.10.0", entries[0].Version)
	}
	got := Unseen(entries, "0.3.0")
	if len(got) != 3 {
		t.Fatalf("Unseen(_, 0.3.0) len=%d want 3", len(got))
	}
	wantVers := []string{"0.10.0", "0.9.9", "0.4.0"}
	for i, w := range wantVers {
		if got[i].Version != w {
			t.Errorf("got[%d].Version=%q want %q", i, got[i].Version, w)
		}
	}
}

func TestUnseen_EmptyLastSeenReturnsAll(t *testing.T) {
	entries := []Entry{
		{Version: "0.3.0", Date: mustDate(t, "2026-03-01")},
		{Version: "0.2.0", Date: mustDate(t, "2026-02-01")},
		{Version: "0.1.0", Date: mustDate(t, "2026-01-01")},
	}
	got := Unseen(entries, "")
	if len(got) != len(entries) {
		t.Fatalf("Unseen(_, \"\") len=%d want %d (first-run path returns every entry)",
			len(got), len(entries))
	}
	for i := range entries {
		if got[i].Version != entries[i].Version {
			t.Errorf("got[%d]=%q want %q", i, got[i].Version, entries[i].Version)
		}
	}
	// Mutating the returned slice must not corrupt the input.
	got[0].Version = "mutated"
	if entries[0].Version == "mutated" {
		t.Errorf("Unseen returned an aliasing slice; caller mutation leaked into input")
	}
}

func TestUnseen_LastSeenEqualsLatestReturnsNone(t *testing.T) {
	entries := []Entry{
		{Version: "0.3.0", Date: mustDate(t, "2026-03-01")},
		{Version: "0.2.0", Date: mustDate(t, "2026-02-01")},
	}
	if got := Unseen(entries, "0.3.0"); len(got) != 0 {
		t.Fatalf("Unseen(_, latest) len=%d want 0", len(got))
	}
}

func TestUnseen_GarbageLastSeenFallsBackToFirstRun(t *testing.T) {
	entries := []Entry{
		{Version: "0.3.0", Date: mustDate(t, "2026-03-01")},
	}
	got := Unseen(entries, "not-a-semver")
	if len(got) != 1 {
		t.Fatalf("Unseen(_, garbage) len=%d want 1 (fall back to first-run)", len(got))
	}
}

func TestLatest(t *testing.T) {
	if got := Latest(nil); got != "" {
		t.Errorf("Latest(nil)=%q want empty", got)
	}
	entries := []Entry{
		{Version: "0.10.0"},
		{Version: "0.9.0"},
	}
	if got := Latest(entries); got != "0.10.0" {
		t.Errorf("Latest=%q want 0.10.0", got)
	}
}

func TestNormalizeBuild(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		isRel bool
	}{
		{"", "", false},
		{"dev", "", false},
		{"v0.3.1", "0.3.1", true},
		{"0.3.1", "0.3.1", true},
		{"v0.3.1-5-gabc1234-dirty", "", false},
		{"v0.3.1-dirty", "", false},
		{"v0.3.1-rc.1", "", false},
		{"v0.3.1+build.7", "", false},
		{"v1", "", false},
		{"v1.2", "", false},
		{"v1.2.3.4", "", false},
		{"garbage", "", false},
		{"  v0.3.1  ", "0.3.1", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, isRel := NormalizeBuild(c.in)
			if got != c.want || isRel != c.isRel {
				t.Errorf("NormalizeBuild(%q)=(%q,%v) want (%q,%v)",
					c.in, got, isRel, c.want, c.isRel)
			}
		})
	}
}

// TestEmbedded_NonEmptyAndSorted pins the embedded slice's
// newest-first-by-semver invariant (review R5). Strict semver
// comparison via x/mod/semver.Compare catches a reversed sort
// AND implicitly catches duplicates (strictly-newer-than implies
// not-equal). The previous t.Logf-only date check was a no-op
// against a sort regression; the strict assertion below makes
// any sortNewestFirst flip fail loudly.
func TestEmbedded_NonEmptyAndSorted(t *testing.T) {
	es := Embedded()
	if len(es) == 0 {
		t.Fatalf("Embedded() returned no entries; CHANGELOG.md must seed at least one entry")
	}
	for i := 1; i < len(es); i++ {
		if semver.Compare("v"+es[i-1].Version, "v"+es[i].Version) <= 0 {
			t.Errorf("entries not strictly newest-first at i=%d: %q before %q",
				i, es[i-1].Version, es[i].Version)
		}
	}
}

func TestRenderMarkdown_ComposesHeaderAndBody(t *testing.T) {
	entries := []Entry{
		{
			Version: "0.2.0",
			Date:    mustDate(t, "2026-03-15"),
			Body:    "### Added\n- thing.",
		},
		{
			Version: "0.1.0",
			Date:    mustDate(t, "2026-02-15"),
			Body:    "### Added\n- first.",
		},
	}
	got := RenderMarkdown(entries)
	if !strings.Contains(got, "## 0.2.0 - 2026-03-15") {
		t.Errorf("rendered body missing first header:\n%s", got)
	}
	if !strings.Contains(got, "## 0.1.0 - 2026-02-15") {
		t.Errorf("rendered body missing second header:\n%s", got)
	}
	// Brackets must NOT appear around the version: the source
	// CHANGELOG.md carries a trailing `[X.Y.Z]: ...compare/...`
	// reference-link footer and a bracketed header in the popup
	// would resolve against it, inlining the URL into the visible
	// heading. See Entry.Header godoc for the full rationale.
	if strings.Contains(got, "## [0.2.0]") || strings.Contains(got, "## [0.1.0]") {
		t.Errorf("rendered header must not bracket the version (would resolve as a reference link):\n%s", got)
	}
	if !strings.Contains(got, "- thing.") {
		t.Errorf("rendered body missing first body line:\n%s", got)
	}
	if !strings.Contains(got, "- first.") {
		t.Errorf("rendered body missing second body line:\n%s", got)
	}
	if got == "" {
		t.Fatal("RenderMarkdown returned empty")
	}
	if RenderMarkdown(nil) != "" {
		t.Errorf("RenderMarkdown(nil) must return empty string")
	}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("mustDate(%q): %v", s, err)
	}
	return d
}
