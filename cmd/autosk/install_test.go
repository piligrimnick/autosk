package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These exercise the `autosk install` CLI wiring against a real isolated
// autoskd (see daemon_harness_test.go). We use a LOCAL-path source so the test
// never shells out to npm (a local install only verifies the path exists and
// writes settings.json — it is referenced in place, never copied).

func TestInstallLocalListRemove(t *testing.T) {
	dir := initProject(t)

	// A local single-file extension; its mere existence is enough for a local
	// install (the loader resolves a .js/.ts file as an entry).
	extPath := filepath.Join(dir, "my-ext.js")
	if err := os.WriteFile(extPath, []byte("export default function () {}\n"), 0o644); err != nil {
		t.Fatalf("write ext: %v", err)
	}

	// install -l (project scope): not copied, installed=false, path stored.
	out, err := runRoot(t, dir, "install", extPath, "-l", "--json")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	var ins map[string]any
	if err := json.Unmarshal([]byte(out), &ins); err != nil {
		t.Fatalf("unmarshal install: %v\n%s", err, out)
	}
	if ins["scope"] != "project" {
		t.Errorf("expected project scope, got %v", ins["scope"])
	}
	if ins["installed"] != false {
		t.Errorf("a local install must not run npm (installed=false), got %v", ins["installed"])
	}
	if ins["source"] != extPath {
		t.Errorf("expected source %q, got %v", extPath, ins["source"])
	}

	// list --json: the project entry shows kind=local, resolved=true.
	list, err := runRoot(t, dir, "install", "list", "--json")
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
		t.Errorf("installed entry %q missing from list: %s", extPath, list)
	}

	// remove -l: drops the entry from project settings.
	rem, err := runRoot(t, dir, "install", "remove", extPath, "-l", "--json")
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
	list2, err := runRoot(t, dir, "install", "list", "--json")
	if err != nil {
		t.Fatalf("list2: %v\n%s", err, list2)
	}
	if strings.Contains(list2, extPath) {
		t.Errorf("entry still present after remove: %s", list2)
	}
}

// TestInstallRejectsBareSource verifies the explicit-source rule: a bare token
// (neither npm: nor a path) is rejected with a clear error.
func TestInstallRejectsBareSource(t *testing.T) {
	dir := initProject(t)
	out, err := runRoot(t, dir, "install", "bare-name")
	if err == nil {
		t.Fatalf("expected an error for a bare-name source, got output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "unrecognised extension source") {
		t.Errorf("expected an unrecognised-source error, got: %v", err)
	}
}
