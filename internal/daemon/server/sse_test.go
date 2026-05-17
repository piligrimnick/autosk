package server_test

import "testing"

// SSE tests are removed during the v0.2 transition for the same reason
// as the rest of the server tests (see server_test.go). W6 reinstates
// them against the workflow engine.

func TestSSE_PlaceholderUntilW6(t *testing.T) {
	t.Skip("v0.2 transition: SSE tests are rewritten in W6 (workflow engine)")
}
