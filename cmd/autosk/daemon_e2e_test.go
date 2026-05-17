package main

import "testing"

// The v0.1 end-to-end test drove the now-defunct submit-prompt-with-ad-hoc
// flow. Under v0.2 the daemon is being rewired into a workflow engine and
// Submit returns 501 until W6 lands.
//
// W9 (docs/plans/20260517-Workflows-Plan.md §9) replaces this with the
// workflow acceptance scenario. Keeping the file as a placeholder so the
// package compiles and the regression is documented.

func TestE2E_DaemonSpawnsFakePiAndCompletes(t *testing.T) {
	t.Skip("v0.2 transition: rewritten in W9 (workflow acceptance test)")
}
