// File-level comment (intentionally separated from `package
// changelog` by a blank line so godoc does NOT pick this up as a
// second package-doc block — the canonical doc lives in
// changelog.go; see review R11). The two-file split would
// otherwise trip staticcheck ST1000 and concatenate the doc
// output of `go doc -all ./internal/changelog`.
//
// CHANGELOG.md in this directory IS the canonical source from
// the embed directive's perspective. The repo-root CHANGELOG.md
// is a relative symlink pointing here so humans + release tooling
// (scripts/changelog-section.sh, .github/workflows/release.yml,
// GitHub's auto-renderer) keep using the familiar repo-root path
// while the embed directive sees a real file inside the package.
// The embed directive rejects symlinks and parent-directory
// traversal (..), so the canonical file MUST live next to
// embed.go — don't try to relocate it back to the repo root and
// turn this file into a symlink, the build will fail.
//
// Embedded() parses the blob once via sync.Once. A parse failure
// is treated as a release-process bug (the blob is baked in at
// build time), but per review R7 the binary stays alive: we log
// via the optional changelog SetLogger sink and return an empty
// slice so the lazy startup hook silently skips the popup instead
// of crashing the TUI. A panic here would have no operator-side
// mitigation — the embedded blob is, well, embedded.

package changelog

import (
	_ "embed"
	"sync"
)

//go:embed CHANGELOG.md
var embeddedSource string

var (
	embeddedOnce    sync.Once
	embeddedEntries []Entry
)

// Embedded returns the parsed entries from the binary's embedded
// CHANGELOG.md, newest-first by semver. Parsing is done once on
// the first call; subsequent calls return the cached slice (NOT a
// copy — callers must not mutate it).
//
// Non-panicking on parse failure (review R7): the embedded blob is
// baked into the binary, so a malformed CHANGELOG.md is a release-
// process bug the operator can't fix at runtime. We log via the
// optional logger (SetLogger) and return an empty slice; the lazy
// startup hook treats len(Embedded())==0 as "no popup" and the TUI
// keeps running. Any future Parse implementation that actually
// returns errors MUST keep this code path non-panicking — see
// review R7 for the rationale.
func Embedded() []Entry {
	embeddedOnce.Do(func() {
		es, err := Parse([]byte(embeddedSource))
		if err != nil {
			logf("changelog.Embedded: parse failed: %v; popup disabled", err)
			embeddedEntries = nil
			return
		}
		embeddedEntries = es
	})
	return embeddedEntries
}
