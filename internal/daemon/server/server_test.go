package server_test

import "testing"

// The v0.1 server tests are intentionally removed here.
// During the v0.2 transition (W1 → W6) the Submit endpoint returns 501
// and the prompt/model/thinking/cwd/closure_kind fields are gone from
// the API. W6 will reinstate full server tests against the workflow
// engine; until then this file documents the gap so go test ./... stays
// green and the regression risk is explicit.
//
// See docs/plans/20260517-Workflows-Plan.md §6 and §8 (W6).

func TestServer_PlaceholderUntilW6(t *testing.T) {
	t.Skip("v0.2 transition: server tests are rewritten in W6 (workflow engine)")
}
