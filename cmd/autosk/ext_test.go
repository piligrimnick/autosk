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

// TestExtHotReload exercises the headline feature: `ext add` of a NEW workflow
// makes it enrollable + listable on the SAME running daemon with no restart, and
// the CLI drops the restart hint (printing what it applied instead); `ext remove`
// then drops it live the same way. A human-only workflow keeps the assertion
// race-free (the scheduler parks at `human` and never runs an agent).
func TestExtHotReload(t *testing.T) {
	dir := initProject(t)

	// Open the project on the daemon first; the workflow does not exist yet.
	if list, err := runRoot(t, dir, "workflow", "list"); err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	} else if strings.Contains(list, "hot-human") {
		t.Fatalf("hot-human must not exist before the add:\n%s", list)
	}

	// A NEW local extension (outside .autosk/extensions, so only the explicit add
	// surfaces it) registering a human-only workflow. Use the symlink-resolved
	// project dir so the absolute settings entry is canonical: the compiled autoskd
	// imports an extension by its file:// path, and macOS's /var → /private/var
	// symlink in t.TempDir() would otherwise make a fresh import fail to resolve.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	extPath := filepath.Join(realDir, "hot.js")
	extSrc := "export default function (autosk) {\n" +
		"  autosk.registerWorkflow({ name: \"hot-human\", firstStep: \"review\", steps: {\n" +
		"    review: { status: \"human\" }, accept: { status: \"human\" },\n" +
		"  } });\n}\n"
	if err := os.WriteFile(extPath, []byte(extSrc), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}

	// add -l: hot-applies to the one open project, no restart hint.
	add, err := runRoot(t, dir, "ext", "add", extPath, "-l")
	if err != nil {
		t.Fatalf("ext add: %v\n%s", err, add)
	}
	if !strings.Contains(add, "applied live to 1 open project") {
		t.Errorf("expected an 'applied live' line, got:\n%s", add)
	}
	if strings.Contains(add, "restart the daemon") {
		t.Errorf("a hot-applied add must NOT print the restart hint, got:\n%s", add)
	}

	// The new workflow is now listable WITHOUT a daemon restart ...
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list (post-add): %v\n%s", err, list)
	}
	if !strings.Contains(list, "hot-human") {
		t.Errorf("hot-human missing after a hot add:\n%s", list)
	}

	// ... and enrollable: a task enrolls into it (parking at human), proving the
	// engine schedules over the freshly-added workflow with no restart.
	id := createTask(t, dir, "enroll into hot-human")
	enr, err := runRoot(t, dir, "enroll", id, "--workflow", "hot-human", "--json")
	if err != nil {
		t.Fatalf("enroll into hot-added workflow: %v\n%s", err, enr)
	}
	var tv map[string]any
	if err := json.Unmarshal([]byte(enr), &tv); err != nil {
		t.Fatalf("unmarshal enroll: %v\n%s", err, enr)
	}
	if tv["workflow"] != "hot-human" {
		t.Errorf("expected workflow hot-human after enroll, got %v", tv["workflow"])
	}

	// remove -l: drops it live the same way (no restart hint).
	rem, err := runRoot(t, dir, "ext", "remove", extPath, "-l")
	if err != nil {
		t.Fatalf("ext remove: %v\n%s", err, rem)
	}
	if !strings.Contains(rem, "applied live to 1 open project") {
		t.Errorf("expected an 'applied live' line on remove, got:\n%s", rem)
	}
	if strings.Contains(rem, "restart the daemon") {
		t.Errorf("a hot-applied remove must NOT print the restart hint, got:\n%s", rem)
	}
	list2, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list (post-remove): %v\n%s", err, list2)
	}
	if strings.Contains(list2, "hot-human") {
		t.Errorf("hot-human still listed after a hot remove:\n%s", list2)
	}
}

// TestExtReloadVerb checks `autosk ext reload`: a brand-new local extension file
// dropped into .autosk/extensions/ is picked up on demand, with the reload
// summary reporting the rebuilt workflow set.
func TestExtReloadVerb(t *testing.T) {
	dir := initProject(t)
	// Open the project; reloadable workflow not present yet.
	if _, err := runRoot(t, dir, "workflow", "list"); err != nil {
		t.Fatalf("workflow list: %v", err)
	}

	extDir := filepath.Join(dir, ".autosk", "extensions")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extensions: %v", err)
	}
	extSrc := "export default function (autosk) {\n" +
		"  autosk.registerWorkflow({ name: \"reload-human\", firstStep: \"review\", steps: {\n" +
		"    review: { status: \"human\" },\n" +
		"  } });\n}\n"
	if err := os.WriteFile(filepath.Join(extDir, "reload.js"), []byte(extSrc), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}

	out, err := runRoot(t, dir, "ext", "reload", "--json")
	if err != nil {
		t.Fatalf("ext reload: %v\n%s", err, out)
	}
	var res struct {
		Root      string   `json:"root"`
		Workflows []string `json:"workflows"`
		Parked    []struct {
			TaskID string `json:"task_id"`
			Error  string `json:"error"`
		} `json:"parked"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal reload: %v\n%s", err, out)
	}
	var found bool
	for _, w := range res.Workflows {
		if w == "reload-human" {
			found = true
		}
	}
	if !found {
		t.Errorf("reload summary missing reload-human: %+v", res.Workflows)
	}
	if len(res.Parked) != 0 {
		t.Errorf("expected no parked tasks, got %+v", res.Parked)
	}
	// The registry view reflects the reload without a restart.
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list (post-reload): %v\n%s", err, list)
	}
	if !strings.Contains(list, "reload-human") {
		t.Errorf("reload-human missing after ext reload:\n%s", list)
	}
}

// TestExtReloadDiagnostics is the safety guarantee (acceptance criterion 8): a
// bad extension dropped alongside a good one is recorded as a diagnostic on the
// reload result (and printed on the human path), the daemon does NOT crash, and
// the sibling good workflow stays loaded + listable. Exercises the Go mirror of
// ExtensionReloadResult.diagnostics + renderExtReload.
func TestExtReloadDiagnostics(t *testing.T) {
	dir := initProject(t)
	// Open the project on the daemon first.
	if _, err := runRoot(t, dir, "workflow", "list"); err != nil {
		t.Fatalf("workflow list: %v", err)
	}

	extDir := filepath.Join(dir, ".autosk", "extensions")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extensions: %v", err)
	}
	// A GOOD human-only workflow next to a BAD extension whose factory throws.
	good := "export default function (autosk) {\n" +
		"  autosk.registerWorkflow({ name: \"diag-good\", firstStep: \"review\", steps: {\n" +
		"    review: { status: \"human\" },\n  } });\n}\n"
	if err := os.WriteFile(filepath.Join(extDir, "good.js"), []byte(good), 0o644); err != nil {
		t.Fatalf("write good ext: %v", err)
	}
	bad := "export default function () { throw new Error(\"boom\"); }\n"
	if err := os.WriteFile(filepath.Join(extDir, "bad.js"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad ext: %v", err)
	}

	// --json: the reload SUCCEEDS (no crash) and carries the diagnostic.
	out, err := runRoot(t, dir, "ext", "reload", "--json")
	if err != nil {
		t.Fatalf("ext reload --json: %v\n%s", err, out)
	}
	var res struct {
		Workflows   []string `json:"workflows"`
		Diagnostics []struct {
			Source string `json:"source"`
			Error  string `json:"error"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal reload: %v\n%s", err, out)
	}
	if len(res.Diagnostics) == 0 {
		t.Fatalf("expected a load diagnostic for the bad extension, got none:\n%s", out)
	}
	var sawThrew bool
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Error, "factory threw") {
			sawThrew = true
		}
	}
	if !sawThrew {
		t.Errorf("expected a 'factory threw' diagnostic, got: %+v", res.Diagnostics)
	}
	var sawGood bool
	for _, w := range res.Workflows {
		if w == "diag-good" {
			sawGood = true
		}
	}
	if !sawGood {
		t.Errorf("the sibling good workflow must stay loaded despite the bad one, got: %+v", res.Workflows)
	}

	// Human path: the diagnostic is printed (renderExtReload, on stderr) and the
	// command still exits cleanly.
	human, err := runRoot(t, dir, "ext", "reload")
	if err != nil {
		t.Fatalf("ext reload (human): %v\n%s", err, human)
	}
	if !strings.Contains(human, "load diagnostic") {
		t.Errorf("human reload output must surface the load diagnostic, got:\n%s", human)
	}
	if !strings.Contains(human, "diag-good") {
		t.Errorf("human reload output must still list the good workflow, got:\n%s", human)
	}

	// The good workflow is genuinely usable after the bad reload: enroll succeeds.
	id := createTask(t, dir, "enroll into diag-good")
	if enr, err := runRoot(t, dir, "enroll", id, "--workflow", "diag-good", "--json"); err != nil {
		t.Fatalf("enroll into good workflow after a bad reload: %v\n%s", err, enr)
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
