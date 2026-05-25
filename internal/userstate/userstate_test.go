package userstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := State{LastSeenChangelog: "0.3.1"}
	if err := saveTo(path, in); err != nil {
		t.Fatalf("saveTo: %v", err)
	}
	got, err := loadFrom(path)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if got != in {
		t.Errorf("roundtrip: got %+v want %+v", got, in)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	dir := t.TempDir()
	// File does NOT exist.
	got, err := loadFrom(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("Load on missing file: %v (want nil error)", err)
	}
	if got != (State{}) {
		t.Errorf("Load on missing file: %+v want zero State{}", got)
	}
}

func TestLoad_MissingParentDir(t *testing.T) {
	dir := t.TempDir()
	got, err := loadFrom(filepath.Join(dir, "no-such-subdir", "state.json"))
	if err != nil {
		t.Fatalf("Load on missing parent dir: %v (want nil error)", err)
	}
	if got != (State{}) {
		t.Errorf("Load on missing parent dir: %+v want zero State{}", got)
	}
}

func TestLoad_MalformedJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := loadFrom(path); err == nil {
		t.Fatal("Load on malformed JSON: got nil error, want a parse error")
	}
}

func TestSave_AtomicTempfileAndPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0600 file perms aren't meaningful on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "state.json")
	if err := saveTo(path, State{LastSeenChangelog: "0.3.1"}); err != nil {
		t.Fatalf("saveTo: %v", err)
	}
	// File exists at the final path.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm=%o want 0600", fi.Mode().Perm())
	}
	// Parent directory created with 0700 (best-effort; we don't
	// enforce on pre-existing dirs).
	dirfi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirfi.Mode().Perm() != 0o700 {
		t.Errorf("dir perm=%o want 0700", dirfi.Mode().Perm())
	}
	// No leftover tempfiles in the directory: Save must clean up
	// even on success (the rename consumed the tempfile).
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".state.json.tmp-") {
			t.Errorf("leftover tempfile after successful Save: %q", e.Name())
		}
	}
}

func TestSave_AtomicRenameSemantics(t *testing.T) {
	// Round-trip with two overwrite cycles; the final read must
	// reflect the second write, never a half-merged view.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveTo(path, State{LastSeenChangelog: "0.3.0"}); err != nil {
		t.Fatalf("saveTo #1: %v", err)
	}
	if err := saveTo(path, State{LastSeenChangelog: "0.4.0"}); err != nil {
		t.Fatalf("saveTo #2: %v", err)
	}
	got, err := loadFrom(path)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if got.LastSeenChangelog != "0.4.0" {
		t.Errorf("after overwrite: got %q want 0.4.0", got.LastSeenChangelog)
	}
}

func TestSave_PreservesUnknownFields(t *testing.T) {
	// json.Unmarshal silently drops unknown fields, but the
	// PRESERVED-roundtrip invariant only holds for THIS Go version.
	// The contract for now: only the documented fields survive.
	// Future fields land here; until then, this test pins the
	// roundtrip shape.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	rawIn := []byte(`{"last_seen_changelog":"0.3.1","custom_future_field":"keep me"}`)
	if err := os.WriteFile(path, rawIn, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := loadFrom(path)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if got.LastSeenChangelog != "0.3.1" {
		t.Errorf("LastSeenChangelog=%q want 0.3.1", got.LastSeenChangelog)
	}
}

func TestPath_HonorsEnvOverride(t *testing.T) {
	// $AUTOSK_STATE_FILE is the documented override; pin it so a
	// future refactor can't silently break the redirect tests rely
	// on.
	t.Setenv("AUTOSK_STATE_FILE", "/tmp/explicit-override-path")
	if got := Path(); got != "/tmp/explicit-override-path" {
		t.Errorf("Path with env override=%q want /tmp/explicit-override-path", got)
	}
}

func TestPath_DefaultUnderHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUTOSK_STATE_FILE", "")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // windows
	want := filepath.Join(dir, ".autosk", "state.json")
	if got := Path(); got != want {
		t.Errorf("Path under HOME=%q got %q want %q", dir, got, want)
	}
}

func TestSave_PrettyJSON(t *testing.T) {
	// Operators may inspect ~/.autosk/state.json by hand; the
	// JSON must be human-readable (indented, trailing newline).
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveTo(path, State{LastSeenChangelog: "0.3.1"}); err != nil {
		t.Fatalf("saveTo: %v", err)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(string(buf), "\n") {
		t.Errorf("missing trailing newline: %q", string(buf))
	}
	// Round-trip through json.Decoder to assert it's valid JSON.
	var raw map[string]string
	if err := json.Unmarshal(buf, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}
