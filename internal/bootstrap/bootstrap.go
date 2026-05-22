// Package bootstrap owns the workflow JSON shipped with the autosk
// binary and applied by `autosk init`. The canonical source of every
// bootstrap workflow lives next to this file as a *.json sibling and is
// embedded via go:embed; runtime callers parse it through the standard
// workflow.ParseReader path so the bootstrap workflows go through the
// same validation as user-authored ones.
//
// Adding a new bootstrap workflow:
//
//  1. Drop the JSON next to this file.
//  2. Add a `//go:embed` line + an exported accessor that calls
//     fromJSON below.
//  3. Wire it into `cmd/autosk/init.go`.
//
// The embedded JSON must use the inline `first_message` form on every
// step (the embed has no on-disk base dir to resolve
// `first_message_file` against). bootstrap_test.go pins that invariant.
package bootstrap

import (
	"bytes"
	_ "embed"
	"fmt"

	"autosk/internal/workflow"
)

// FeatureDevGenericName is the name of the bootstrap workflow shipped
// with autosk. Exported so callers (init, tests) don't hard-code the
// literal.
const FeatureDevGenericName = "feature-dev-generic"

//go:embed feature-dev-generic.json
var featureDevGenericJSON []byte

// FeatureDevGeneric returns the canonical bootstrap workflow definition.
// A fresh parse runs on every call so callers can mutate the returned
// struct without affecting a cached copy.
func FeatureDevGeneric() (workflow.Definition, error) {
	return fromJSON(featureDevGenericJSON)
}

// FeatureDevGenericRaw returns the raw embedded JSON bytes. Useful for
// tests and `autosk workflow show --json`-style debugging only — runtime
// callers should go through FeatureDevGeneric so they get a parsed
// Definition with the same checks ParseReader performs.
func FeatureDevGenericRaw() []byte {
	out := make([]byte, len(featureDevGenericJSON))
	copy(out, featureDevGenericJSON)
	return out
}

// fromJSON is the shared parse path. ParseReader is used (not ParseFile)
// because the embedded bytes have no on-disk base directory; the
// invariant "no step uses first_message_file" is asserted by
// bootstrap_test.go so a future edit can't silently break this code
// path.
func fromJSON(body []byte) (workflow.Definition, error) {
	def, err := workflow.ParseReader(bytes.NewReader(body))
	if err != nil {
		return workflow.Definition{}, fmt.Errorf("parse embedded bootstrap workflow: %w", err)
	}
	return def, nil
}
