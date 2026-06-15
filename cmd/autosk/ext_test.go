package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/daemon/rpcclient"
)

// These exercise the `autosk ext` CLI wiring against a real isolated autoskd
// (see daemon_harness_test.go). We use a LOCAL-path source so the test never
// shells out to npm (a local add only verifies the path exists and writes
// settings.json — it is referenced in place, never copied).

func TestExtAddListRemove(t *testing.T) {
	dir := initProject(t)

	// A local single-file extension; its mere existence is enough for a local
	// add (the loader resolves a .js/.ts file as an entry).
	extPath := filepath.Join(dir, "my-ext.js")
	if err := os.WriteFile(extPath, []byte("export default function () {}\n"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}

	// add -l (project scope): not copied, installed=false, path stored.
	out, err := runRoot(t, dir, "ext", "add", extPath, "-l", "--json")
	if err != nil {
		t.Fatalf("ext add: %v\n%s", err, out)
	}
	var ins map[string]any
	if err := json.Unmarshal([]byte(out), &ins); err != nil {
		t.Fatalf("unmarshal add: %v\n%s", err, out)
	}
	if ins["scope"] != "project" {
		t.Errorf("expected project scope, got %v", ins["scope"])
	}
	if ins["installed"] != false {
		t.Errorf("a local add must not run npm (installed=false), got %v", ins["installed"])
	}
	if ins["source"] != extPath {
		t.Errorf("expected source %q, got %v", extPath, ins["source"])
	}

	// list --json: the project entry shows kind=local, resolved=true.
	list, err := runRoot(t, dir, "ext", "list", "--json")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, list)
	}
	var lr struct {
		Entries []struct {
			Source   string `json:"source"`
			Scope    string `json:"scope"`
			Kind     string `json:"kind"`
			Resolved bool   `json:"resolved"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(list), &lr); err != nil {
		t.Fatalf("unmarshal list: %v\n%s", err, list)
	}
	var found bool
	for _, e := range lr.Entries {
		if e.Source == extPath {
			found = true
			if e.Scope != "project" || e.Kind != "local" || !e.Resolved {
				t.Errorf("unexpected entry: %+v", e)
			}
		}
	}
	if !found {
		t.Errorf("added entry %q missing from list: %s", extPath, list)
	}

	// remove -l: drops the entry from project settings.
	rem, err := runRoot(t, dir, "ext", "remove", extPath, "-l", "--json")
	if err != nil {
		t.Fatalf("remove: %v\n%s", err, rem)
	}
	var rr map[string]any
	if err := json.Unmarshal([]byte(rem), &rr); err != nil {
		t.Fatalf("unmarshal remove: %v\n%s", err, rem)
	}
	if rr["removed"] != true {
		t.Errorf("expected removed=true, got %v", rr["removed"])
	}

	// After removal the entry is gone.
	list2, err := runRoot(t, dir, "ext", "list", "--json")
	if err != nil {
		t.Fatalf("list2: %v\n%s", err, list2)
	}
	if strings.Contains(list2, extPath) {
		t.Errorf("entry still present after remove: %s", list2)
	}
}

// TestExtAddRejectsBareSource verifies the explicit-source rule: a bare token
// (neither npm: nor a path) is rejected with a clear error.
func TestExtAddRejectsBareSource(t *testing.T) {
	dir := initProject(t)
	out, err := runRoot(t, dir, "ext", "add", "bare-name")
	if err == nil {
		t.Fatalf("expected an error for a bare-name source, got output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "unrecognised extension source") {
		t.Errorf("expected an unrecognised-source error, got: %v", err)
	}
}

// TestExtUpdateDryRunLocalSkipped runs `ext update --dry-run --json` over a
// project whose only extension is a LOCAL path. A local entry loads in place, so
// it is reported as `skipped` (never version-checked or installed) — the dry-run
// touches no network and exits 0.
func TestExtUpdateDryRunLocalSkipped(t *testing.T) {
	dir := initProject(t)
	extPath := filepath.Join(dir, "local-ext.js")
	if err := os.WriteFile(extPath, []byte("export default function () {}\n"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}
	// Add it to the project scope so update enumerates it.
	if out, err := runRoot(t, dir, "ext", "add", extPath, "-l", "--json"); err != nil {
		t.Fatalf("ext add: %v\n%s", err, out)
	}

	out, err := runRoot(t, dir, "ext", "update", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("ext update --dry-run: %v\n%s", err, out)
	}
	var res struct {
		Entries []struct {
			Source string `json:"source"`
			Name   string `json:"name"`
			Scope  string `json:"scope"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"entries"`
		DryRun  bool `json:"dry_run"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal update: %v\n%s", err, out)
	}
	if !res.DryRun {
		t.Errorf("expected dry_run=true, got %v", res.DryRun)
	}
	if res.Changed {
		t.Errorf("a dry-run must never report changed=true, got %v", res.Changed)
	}
	var found bool
	for _, e := range res.Entries {
		if e.Source == extPath {
			found = true
			if e.Status != "skipped" {
				t.Errorf("a local entry must be skipped, got status %q (reason %q)", e.Status, e.Reason)
			}
			if e.Scope != "project" {
				t.Errorf("expected project scope, got %q", e.Scope)
			}
		}
	}
	if !found {
		t.Errorf("local entry %q missing from update output: %s", extPath, out)
	}
}

// TestExtUpdateRejectsLocalSource verifies a local-path `[source]` target is
// rejected — local extensions load in place and cannot be updated.
func TestExtUpdateRejectsLocalSource(t *testing.T) {
	dir := initProject(t)
	out, err := runRoot(t, dir, "ext", "update", "./some-ext.js")
	if err == nil {
		t.Fatalf("expected an error for a local-path update target, got output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("expected a local-not-updatable error, got: %v", err)
	}
}

// TestExtUpdateFailedExitDecoupledFromFormat locks in the contract that the
// process exit code on a real-run failure is decided by extUpdateFailed —
// independent of whether output is rendered as a table or JSON. This is the
// regression guard for the bug where the --json branch returned nil (exit 0)
// even when a package failed. Driving a genuine npm failure at the CLI level
// needs the network, so we assert the pure decision function directly.
func TestExtUpdateFailedExitDecoupledFromFormat(t *testing.T) {
	cases := []struct {
		name    string
		res     rpcclient.ExtensionUpdateResult
		wantBad bool
	}{
		{
			name: "a failed entry exits non-zero",
			res: rpcclient.ExtensionUpdateResult{Entries: []rpcclient.ExtensionUpdateEntry{
				{Name: "a", Status: "updated"},
				{Name: "b", Status: "failed"},
			}},
			wantBad: true,
		},
		{
			name: "all updated/up-to-date exits 0",
			res: rpcclient.ExtensionUpdateResult{Entries: []rpcclient.ExtensionUpdateEntry{
				{Name: "a", Status: "updated"},
				{Name: "b", Status: "up-to-date"},
				{Name: "c", Status: "skipped"},
			}},
			wantBad: false,
		},
		{
			name:    "dry-run never has failures",
			res:     rpcclient.ExtensionUpdateResult{DryRun: true, Entries: []rpcclient.ExtensionUpdateEntry{{Name: "a", Status: "available"}, {Name: "b", Status: "unknown"}}},
			wantBad: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extUpdateFailed(tc.res); got != tc.wantBad {
				t.Errorf("extUpdateFailed = %v, want %v", got, tc.wantBad)
			}
		})
	}
}

// TestInstallVerbRemoved asserts the clean break: the old top-level `install`
// verbs are gone (Cobra errors on an unknown command, non-zero exit).
func TestInstallVerbRemoved(t *testing.T) {
	dir := initProject(t)
	for _, argv := range [][]string{
		{"install", "npm:@x/y"},
		{"install", "list"},
		{"install", "remove", "npm:@x/y"},
	} {
		out, err := runRoot(t, dir, argv...)
		if err == nil {
			t.Errorf("expected `autosk %s` to error as an unknown command, got output:\n%s", strings.Join(argv, " "), out)
		}
	}
}
