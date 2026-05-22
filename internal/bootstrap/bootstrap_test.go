package bootstrap_test

import (
	"context"
	"testing"

	"autosk/internal/bootstrap"
	"autosk/internal/workflow"
)

// TestBootstrap_FeatureDevGenericParses guards the invariant that the
// embedded JSON is well-formed, references @autogent/generic on every
// step, and never uses first_message_file (which would require a base
// directory to resolve and break the embed code path).
func TestBootstrap_FeatureDevGenericParses(t *testing.T) {
	def, err := bootstrap.FeatureDevGeneric()
	if err != nil {
		t.Fatalf("FeatureDevGeneric: %v", err)
	}
	if def.Name != bootstrap.FeatureDevGenericName {
		t.Errorf("name = %q, want %q", def.Name, bootstrap.FeatureDevGenericName)
	}
	if def.FirstStep != "dev" {
		t.Errorf("first_step = %q, want %q", def.FirstStep, "dev")
	}
	// dev → review → docs → validator (four sibling steps; the fifth
	// transition target on validator is task_status=human).
	if len(def.Steps) != 4 {
		t.Errorf("steps count = %d, want 4", len(def.Steps))
	}
	wantStepNames := []string{"dev", "review", "docs", "validator"}
	if got := def.StepNames; len(got) != len(wantStepNames) {
		t.Errorf("step order length = %d, want %d (%v)", len(got), len(wantStepNames), got)
	} else {
		for i, want := range wantStepNames {
			if got[i] != want {
				t.Errorf("step order[%d] = %q, want %q", i, got[i], want)
			}
		}
	}
	for name, s := range def.Steps {
		if s.AgentName != "@autogent/generic" {
			t.Errorf("step %q agent = %q, want @autogent/generic", name, s.AgentName)
		}
		if s.AgentParams != nil && s.AgentParams.FirstMessageFile != "" {
			t.Errorf("step %q uses first_message_file=%q; embedded JSON cannot resolve relative paths",
				name, s.AgentParams.FirstMessageFile)
		}
	}
	// Validate runs without an agent store so it skips the existence
	// check; this is enough to catch transition / target typos.
	if err := workflow.Validate(context.Background(), def, nil, workflow.ValidateOpts{}); err != nil {
		t.Fatalf("Validate(embedded): %v", err)
	}
}

// TestBootstrap_FeatureDevGenericRawIsACopy makes sure FeatureDevGenericRaw
// hands out a defensive copy: mutating the returned slice must not
// corrupt the embed for the next caller.
func TestBootstrap_FeatureDevGenericRawIsACopy(t *testing.T) {
	a := bootstrap.FeatureDevGenericRaw()
	if len(a) == 0 {
		t.Fatal("raw bytes empty")
	}
	a[0] = '!'
	b := bootstrap.FeatureDevGenericRaw()
	if b[0] == '!' {
		t.Fatalf("raw bytes shared with embed; mutation leaked")
	}
}
