package main

import (
	"os"
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
