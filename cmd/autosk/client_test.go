package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCallerAgentName_Default verifies the env-var default.
//
// The lazy-insert of a caller's agents row (formerly resolveCallerAgent in this
// package, exercised against a direct store) now lives behind the daemon: the
// write verbs pass `caller`/`author` and autoskd ensures the row. That
// behaviour is covered by the daemon's own verb tests; here we only verify the
// client-side name resolution.
func TestCallerAgentName_Default(t *testing.T) {
	os.Unsetenv(envAgentName)
	if got := callerAgentName(); got != "human" {
		t.Fatalf("default: got %q want human", got)
	}
	t.Setenv(envAgentName, "  bot  ")
	if got := callerAgentName(); got != "bot" {
		t.Fatalf("trim: got %q want bot", got)
	}
}

// TestCallerCwd_EnvOverride verifies AUTOSK_CWD overrides the process working
// directory for the project selector (the worktree-isolation fix): a workflow
// agent runs in a throwaway worktree, and the daemon sets AUTOSK_CWD to the real
// project root so `autosk` resolves the right project.
func TestCallerCwd_EnvOverride(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	t.Setenv(envProjectCwd, "")
	os.Unsetenv(envProjectCwd)
	if got, err := callerCwd(); err != nil || got != wd {
		t.Fatalf("default: got %q err %v want %q", got, err, wd)
	}

	t.Setenv(envProjectCwd, "/repo/project")
	if got, err := callerCwd(); err != nil || got != "/repo/project" {
		t.Fatalf("absolute override: got %q err %v want /repo/project", got, err)
	}

	t.Setenv(envProjectCwd, "sub/dir")
	want := filepath.Join(wd, "sub/dir")
	if got, err := callerCwd(); err != nil || got != want {
		t.Fatalf("relative override: got %q err %v want %q", got, err, want)
	}

	t.Setenv(envProjectCwd, "   ")
	if got, err := callerCwd(); err != nil || got != wd {
		t.Fatalf("blank override: got %q err %v want %q", got, err, wd)
	}
}
