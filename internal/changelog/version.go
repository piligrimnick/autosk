package changelog

import (
	"strings"

	"golang.org/x/mod/semver"
)

// NormalizeBuild collapses buildinfo.Version into a (bareSemver,
// releaseBuild) pair.
//
// Pinned shapes per the task plan:
//
//	"dev"                     → ("", false)
//	""                        → ("", false)
//	"v0.3.1"                  → ("0.3.1", true)
//	"0.3.1"                   → ("0.3.1", true)
//	"v0.3.1-5-gabc1234-dirty" → ("", false)
//	"v0.3.1-rc.1"             → ("", false)   (pre-release)
//	anything else             → ("", false)
//
// The "release build" predicate is intentionally strict: anything
// that looks like a `git describe` post-tag suffix (the `-N-gSHA`
// or `-dirty` tails) or a pre-release tag is treated as a dev
// build so the popup never fires on a developer workstation that
// happens to be sitting on a tagged commit with local edits. A
// dev session must not be able to accidentally mark a future
// release's changelog as seen.
func NormalizeBuild(v string) (bare string, releaseBuild bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return "", false
	}
	bare = strings.TrimPrefix(v, "v")
	canon := "v" + bare
	if !semver.IsValid(canon) {
		return "", false
	}
	// semver.Canonical normalises "v1.2" → "v1.2.0", but we want
	// the EXACT shape used in the changelog ("X.Y.Z" with all
	// three components present and no pre-release / build metadata
	// suffix). Build metadata is the part after "+"; pre-release
	// is the part after "-". Reject both.
	if semver.Prerelease(canon) != "" || semver.Build(canon) != "" {
		return "", false
	}
	// Require exactly three dotted components so "v1" or "v1.2"
	// don't slip through as a release.
	if strings.Count(bare, ".") != 2 {
		return "", false
	}
	// Canonical normalises any leading zeros / formatting quirks
	// (e.g. "v01.2.3" → "v1.2.3"); use the canonical bare form so
	// downstream comparisons against the embedded changelog match
	// the on-disk header byte-for-byte.
	can := semver.Canonical(canon)
	if can == "" {
		return "", false
	}
	return strings.TrimPrefix(can, "v"), true
}
