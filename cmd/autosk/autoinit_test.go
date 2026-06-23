package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withInteractiveStdin swaps the package-level TTY check + stdin reader for the
// duration of the test so a write verb running in a fresh dir takes the
// prompting code path. The supplied `answer` is piped verbatim into the
// confirmation prompt; multiple answers may be supplied by joining them with
// "\n".
func withInteractiveStdin(t *testing.T, answer string) {
	t.Helper()
	prevIsTTY := isInteractiveFn
	prevReader := confirmReader
	isInteractiveFn = func() bool { return true }
	confirmReader = strings.NewReader(answer)
	t.Cleanup(func() {
		isInteractiveFn = prevIsTTY
		confirmReader = prevReader
	})
}

// TestAutoInit_NonTTYCreatesProject: a fresh dir + a write verb auto-creates the
// .autosk/ project (non-TTY path) without an explicit `autosk init`. v2 has no
// per-project bootstrap seeding (feature-dev ships bundled).
func TestAutoInit_NonTTYCreatesProject(t *testing.T) {
	dir := t.TempDir()
	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create on fresh dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "autosk: created") {
		t.Errorf("expected 'autosk: created' notice on stderr:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk")); err != nil {
		t.Errorf(".autosk not created: %v", err)
	}

	// A second write verb is a normal hit — no second "autosk: created".
	out2, err := runRoot(t, dir, "create", "smoke-2")
	if err != nil {
		t.Fatalf("second create: %v\n%s", err, out2)
	}
	if strings.Contains(out2, "autosk: created") {
		t.Errorf("second create should not re-create the project:\n%s", out2)
	}
}

// TestAutoInit_TTYPromptYes: stdin/stderr report as TTY, user answers "y", the
// project is created.
func TestAutoInit_TTYPromptYes(t *testing.T) {
	withInteractiveStdin(t, "y\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create after y: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Create a new autosk project") {
		t.Errorf("expected interactive prompt on stderr:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk")); err != nil {
		t.Errorf(".autosk not created after 'y': %v", err)
	}
}

// TestAutoInit_TTYPromptDefaultYes: an empty answer (Enter) is treated as "y".
func TestAutoInit_TTYPromptDefaultYes(t *testing.T) {
	withInteractiveStdin(t, "\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create after empty answer: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk")); err != nil {
		t.Errorf("empty answer should default to yes and create the project:\n%s", out)
	}
}

// TestAutoInit_TTYPromptNoRefuses: user answers "n", no project is created, the
// command exits with a clear error pointing at `autosk init`.
func TestAutoInit_TTYPromptNoRefuses(t *testing.T) {
	withInteractiveStdin(t, "n\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err == nil {
		t.Fatalf("expected error after declining prompt, got nil; output:\n%s", out)
	}
	combined := out + err.Error()
	if !strings.Contains(combined, "declined to create one") {
		t.Errorf("expected 'declined to create one' error, got: err=%v\nout=%s", err, out)
	}
	if !strings.Contains(combined, "autosk init") {
		t.Errorf("expected error to mention `autosk init`:\nerr=%v\nout=%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk")); !os.IsNotExist(err) {
		t.Errorf(".autosk directory should NOT exist after decline; stat err=%v", err)
	}
}

// TestAutoInit_TTYPromptReprompt: a junk first answer triggers a hint, then "y"
// goes through.
func TestAutoInit_TTYPromptReprompt(t *testing.T) {
	withInteractiveStdin(t, "huh\ny\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create after re-prompt: %v\n%s", err, out)
	}
	if !strings.Contains(out, "please answer 'y' or 'n'") {
		t.Errorf("expected re-prompt hint on stderr:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk")); err != nil {
		t.Errorf("re-prompt then 'y' should still create the project:\n%s", out)
	}
}

// TestAutoInit_JSONSuppressesPrompt: even with a TTY attached, --json skips the
// prompt and falls through to silent auto-init.
func TestAutoInit_JSONSuppressesPrompt(t *testing.T) {
	prevIsTTY := isInteractiveFn
	isInteractiveFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveFn = prevIsTTY })
	prevReader := confirmReader
	confirmReader = blockingReader{}
	t.Cleanup(func() { confirmReader = prevReader })

	dir := t.TempDir()
	out, err := runRoot(t, dir, "--json", "create", "smoke")
	if err != nil {
		t.Fatalf("create --json on fresh dir: %v\n%s", err, out)
	}
	if strings.Contains(out, "Create a new autosk project") {
		t.Errorf("--json should not surface the interactive prompt:\n%s", out)
	}
}

// TestAutoInit_AssumeYesEnv: AUTOSK_AUTOINIT_ASSUME_YES suppresses the prompt
// under a TTY and proceeds as if the user said "y".
func TestAutoInit_AssumeYesEnv(t *testing.T) {
	t.Setenv("AUTOSK_AUTOINIT_ASSUME_YES", "1")
	prevIsTTY := isInteractiveFn
	isInteractiveFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveFn = prevIsTTY })
	prevReader := confirmReader
	confirmReader = blockingReader{}
	t.Cleanup(func() { confirmReader = prevReader })

	dir := t.TempDir()
	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create with assume-yes: %v\n%s", err, out)
	}
	if strings.Contains(out, "Create a new autosk project") {
		t.Errorf("AUTOSK_AUTOINIT_ASSUME_YES should skip the prompt:\n%s", out)
	}
}

// TestAutoInit_ExistingProjectNoPrompt: when a project is already discoverable,
// no prompt fires even if the TTY hooks are stubbed to interactive.
func TestAutoInit_ExistingProjectNoPrompt(t *testing.T) {
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	prevIsTTY := isInteractiveFn
	isInteractiveFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveFn = prevIsTTY })
	prevReader := confirmReader
	confirmReader = blockingReader{}
	t.Cleanup(func() { confirmReader = prevReader })

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create against existing project: %v\n%s", err, out)
	}
	if strings.Contains(out, "Create a new autosk project") {
		t.Errorf("existing project must not trigger the prompt:\n%s", out)
	}
}

// TestAutoInit_NoAutoInitEnv: AUTOSK_NO_AUTOINIT disables auto-init on a write
// verb in a fresh dir.
func TestAutoInit_NoAutoInitEnv(t *testing.T) {
	t.Setenv("AUTOSK_NO_AUTOINIT", "1")
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "create", "smoke"); err == nil {
		t.Fatal("expected auto-init to be disabled by AUTOSK_NO_AUTOINIT")
	}
}

// blockingReader is used by tests that want to prove no read happens.
type blockingReader struct{}

func (blockingReader) Read(_ []byte) (int, error) {
	panic("autoinit prompt read should not have happened in this test")
}

var _ io.Reader = blockingReader{}
