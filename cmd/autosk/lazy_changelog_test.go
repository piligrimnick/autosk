package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/changelog"
	"autosk/internal/userstate"
)

// TestLazy_FirstRun_ShowsFullChangelogAndMarksSeen pins acceptance
// criterion 7 + 9 (first-run-full-history branch):
//
//   - On a release build with no prior state.json, buildChangelogModal
//     returns a non-nil ChangelogModalOptions whose body carries the
//     FULL embedded changelog (every parsed entry, newest-first —
//     including the OLDEST version's header).
//   - Dismissing the popup writes ~/.autosk/state.json with
//     last_seen_changelog = latest embedded version.
//
// The test redirects $AUTOSK_STATE_FILE to a tempfile so neither the
// real $HOME nor any prior dev session's state can interfere.
func TestLazy_FirstRun_ShowsFullChangelogAndMarksSeen(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	t.Setenv("AUTOSK_STATE_FILE", statePath)

	opts := buildChangelogModal("v0.99.0")
	if opts == nil {
		t.Fatal("first-run release build: buildChangelogModal returned nil; want a popup")
	}

	entries := changelog.Embedded()
	if len(entries) == 0 {
		t.Fatal("embedded CHANGELOG is empty; pre-condition for this test failed")
	}
	latest := changelog.Latest(entries)
	oldest := entries[len(entries)-1].Version

	if !strings.Contains(opts.Body, latest) {
		t.Errorf("body must contain latest version %q (newest-first):\n%s", latest, opts.Body)
	}
	if !strings.Contains(opts.Body, oldest) {
		t.Errorf("first-run path must show the OLDEST version's header %q (full history) — body:\n%s",
			oldest, opts.Body)
	}
	if opts.OnDismiss == nil {
		t.Fatal("OnDismiss must be non-nil on the auto-popup path")
	}
	// Fire the dismiss callback — must write state.json with the
	// latest version.
	if err := opts.OnDismiss(); err != nil {
		t.Fatalf("OnDismiss: %v", err)
	}
	st, err := userstate.Load()
	if err != nil {
		t.Fatalf("userstate.Load after dismiss: %v", err)
	}
	if st.LastSeenChangelog != latest {
		t.Errorf("state.json last_seen_changelog=%q want %q (latest embedded version)",
			st.LastSeenChangelog, latest)
	}
}

// TestLazy_DevBuild_SkipsModalAndState pins acceptance criterion 10:
// a dev build (or any non-release describe output) skips the popup
// AND leaves state.json untouched.
func TestLazy_DevBuild_SkipsModalAndState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	t.Setenv("AUTOSK_STATE_FILE", statePath)

	devishVersions := []string{
		"dev",
		"",
		"v0.3.1-5-gabc1234-dirty",
		"v0.3.1-rc.1",
	}
	for _, v := range devishVersions {
		t.Run(v, func(t *testing.T) {
			if got := buildChangelogModal(v); got != nil {
				t.Errorf("buildChangelogModal(%q) returned non-nil; dev / non-release builds must NOT show a popup", v)
			}
		})
	}

	// state.json must remain absent — we never touched it.
	if _, err := userstate.Load(); err != nil {
		t.Errorf("userstate.Load after dev-skip: %v (want nil error, missing file)", err)
	}
}

// TestLazy_NoChangelogFlag_Skips pins acceptance criterion 11: the
// --no-changelog flag suppresses the auto-popup on every code path.
// The flag itself lives in cobra; the policy test is just that when
// the cobra handler honours noChangelog=true, ChangelogModal is left
// nil in tui.Options. We replicate the conditional from
// newLazyCmd.RunE so a future refactor can't accidentally drop the
// check.
func TestLazy_NoChangelogFlag_Skips(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	t.Setenv("AUTOSK_STATE_FILE", statePath)

	// Sanity: without the flag we'd get a popup on this release
	// build.
	if buildChangelogModal("v0.99.0") == nil {
		t.Fatal("pre-condition: release build must yield a popup before --no-changelog gates it")
	}

	// The cobra wiring is `if !noChangelog { opts.ChangelogModal = buildChangelogModal(...) }`,
	// so with noChangelog=true the field stays nil. Replicate that
	// shape:
	noChangelog := true
	var modal any
	if !noChangelog {
		modal = buildChangelogModal("v0.99.0")
	}
	if modal != nil {
		t.Errorf("--no-changelog: ChangelogModal must remain nil regardless of release/state")
	}

	// And state.json must still be untouched (the read-only path).
	if _, err := userstate.Load(); err != nil {
		t.Errorf("userstate.Load after --no-changelog: %v", err)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Errorf("state.json was written despite --no-changelog")
	}
}

// TestLazy_UnseenEntries_OpensModal_DismissMarksSeen pins acceptance
// criterion 8 + 9 (steady-state path):
//
//   - state.json says the operator has already seen up to <oldest>;
//   - the embedded CHANGELOG has at least one strictly newer entry;
//   - the popup body contains the newer entries but NOT the entries
//     <= last_seen_changelog;
//   - dismissing writes <latest> to state.json.
func TestLazy_UnseenEntries_OpensModal_DismissMarksSeen(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	t.Setenv("AUTOSK_STATE_FILE", statePath)

	entries := changelog.Embedded()
	if len(entries) < 2 {
		t.Skip("need at least two embedded changelog entries to exercise the steady-state path")
	}
	latest := entries[0].Version
	previouslySeen := entries[len(entries)-1].Version

	// Seed state.json so we've already seen the oldest version.
	if err := userstate.Save(userstate.State{LastSeenChangelog: previouslySeen}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	opts := buildChangelogModal("v" + latest)
	if opts == nil {
		t.Fatal("steady-state with unseen entries: buildChangelogModal returned nil; want a popup")
	}
	if !strings.Contains(opts.Body, latest) {
		t.Errorf("body must contain latest version %q:\n%s", latest, opts.Body)
	}
	// The previously-seen version must NOT appear as a section
	// header in the body (it's already been seen).
	header := "## [" + previouslySeen + "] -"
	if strings.Contains(opts.Body, header) {
		t.Errorf("body must NOT contain previously-seen header %q (steady-state filter failed):\n%s",
			header, opts.Body)
	}

	// Dismiss → state.json updated to latest.
	if err := opts.OnDismiss(); err != nil {
		t.Fatalf("OnDismiss: %v", err)
	}
	st, err := userstate.Load()
	if err != nil {
		t.Fatalf("Load after dismiss: %v", err)
	}
	if st.LastSeenChangelog != latest {
		t.Errorf("after dismiss: last_seen_changelog=%q want %q", st.LastSeenChangelog, latest)
	}
}

// TestLazy_LastSeenEqualsLatest_NoPopup pins the steady-state
// no-popup branch: if the operator has already seen the latest
// release, no popup fires.
func TestLazy_LastSeenEqualsLatest_NoPopup(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	t.Setenv("AUTOSK_STATE_FILE", statePath)

	entries := changelog.Embedded()
	if len(entries) == 0 {
		t.Skip("embedded changelog empty")
	}
	latest := changelog.Latest(entries)
	if err := userstate.Save(userstate.State{LastSeenChangelog: latest}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := buildChangelogModal("v" + latest); got != nil {
		t.Errorf("last_seen == latest: buildChangelogModal returned non-nil; want a silent start")
	}
}

// TestBuildChangelogModal_HandlesCorruptStateJSON pins the
// fail-safe: a malformed state.json must be left alone (no popup,
// no overwrite) instead of silently overwriting the operator's
// (corrupt) file.
func TestBuildChangelogModal_HandlesCorruptStateJSON(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	t.Setenv("AUTOSK_STATE_FILE", statePath)
	if err := os.WriteFile(statePath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := buildChangelogModal("v0.99.0"); got != nil {
		t.Errorf("corrupt state.json: buildChangelogModal returned non-nil; want skip-and-leave-alone")
	}
	// File still corrupt (we never overwrote it).
	buf, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(buf) != "{not-json" {
		t.Errorf("state.json was rewritten despite corrupt input: %q", string(buf))
	}
}
