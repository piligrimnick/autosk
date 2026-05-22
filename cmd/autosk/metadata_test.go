package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMetadata_ShowEmpty verifies that a fresh task with no metadata
// prints `{}` (an empty JSON object) so callers can rely on the output
// being a valid JSON object even when the column is SQL NULL.
func TestMetadata_ShowEmpty(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "no metadata")
	out, err := runRoot(t, dir, "metadata", "show", id)
	if err != nil {
		t.Fatalf("metadata show: %v\n%s", err, out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, out)
	}
	if len(m) != 0 {
		t.Fatalf("empty metadata expected, got %v", m)
	}
}

// TestMetadata_SetRoundTrip writes a string and a JSON value and reads
// them back via `metadata show`.
func TestMetadata_SetRoundTrip(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "rt")
	if out, err := runRoot(t, dir, "metadata", "set", id, "--key", "tags", "--value", `["urgent","p0"]`); err != nil {
		t.Fatalf("set array: %v\n%s", err, out)
	}
	if out, err := runRoot(t, dir, "metadata", "set", id, "--key", "notes", "--value", "needs design review"); err != nil {
		t.Fatalf("set string: %v\n%s", err, out)
	}
	if out, err := runRoot(t, dir, "metadata", "set", id, "--key", "nested.deep.k", "--value", "42"); err != nil {
		t.Fatalf("set nested int: %v\n%s", err, out)
	}

	out, err := runRoot(t, dir, "metadata", "show", id)
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, out)
	}
	tags, _ := m["tags"].([]any)
	if len(tags) != 2 || tags[0] != "urgent" || tags[1] != "p0" {
		t.Errorf("tags lost: %v", m["tags"])
	}
	if m["notes"] != "needs design review" {
		t.Errorf("notes lost: %v", m["notes"])
	}
	nested, _ := m["nested"].(map[string]any)
	deep, _ := nested["deep"].(map[string]any)
	if v, _ := deep["k"].(float64); int(v) != 42 {
		t.Errorf("nested.deep.k: %v", deep["k"])
	}
}

// TestMetadata_SetRejectsBadStepVisitsLeaf guards humans from
// corrupting the engine's typed view of step_visits.
func TestMetadata_SetRejectsBadStepVisitsLeaf(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "guard")

	_, err := runRoot(t, dir, "metadata", "set", id, "--key", "step_visits.st-x", "--value", `"not-an-int"`)
	if err == nil {
		t.Fatal("expected validation error for string leaf")
	}
	if !strings.Contains(err.Error(), "step_visits") {
		t.Errorf("error should mention step_visits: %v", err)
	}
}

// TestMetadata_SetRejectsNonObjectStepVisits guards against replacing
// the whole reserved object with something that isn't a map.
func TestMetadata_SetRejectsNonObjectStepVisits(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "guard2")

	_, err := runRoot(t, dir, "metadata", "set", id, "--key", "step_visits", "--value", `["foo"]`)
	if err == nil {
		t.Fatal("expected validation error for non-object step_visits")
	}
	if !strings.Contains(err.Error(), "step_visits") {
		t.Errorf("error should mention step_visits: %v", err)
	}
}

// TestMetadata_UnsetRemovesEmptyParents writes a nested key, then unsets
// it, and verifies both the key itself AND any now-empty parent
// containers are gone so the metadata blob round-trips back to NULL.
func TestMetadata_UnsetRemovesEmptyParents(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "prune")
	if _, err := runRoot(t, dir, "metadata", "set", id, "--key", "a.b.c", "--value", `"v"`); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "metadata", "unset", id, "--key", "a.b.c"); err != nil {
		t.Fatal(err)
	}
	out, _ := runRoot(t, dir, "metadata", "show", id)
	var m map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &m)
	if len(m) != 0 {
		t.Fatalf("expected pruned-to-empty metadata, got %v", m)
	}
}

// TestMetadata_UnsetMissingKey is a no-op (exit 0) with a "(no change)"
// note on stderr.
func TestMetadata_UnsetMissingKey(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "noop")
	out, err := runRoot(t, dir, "metadata", "unset", id, "--key", "doesnt.exist")
	if err != nil {
		t.Fatalf("unset of missing key should not error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no change") {
		t.Errorf("expected 'no change' hint, got: %q", out)
	}
}

// TestMetadata_ResetVisitsAll clears the entire step_visits map.
func TestMetadata_ResetVisitsAll(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "reset")
	// Seed two counters via the JSON-value form (bypasses step_visits validation? — no,
	// the validation also applies to writing the whole object, so we must use proper ids).
	if _, err := runRoot(t, dir, "metadata", "set", id, "--key", "step_visits.st-a", "--value", "3"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "metadata", "set", id, "--key", "step_visits.st-b", "--value", "5"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "metadata", "reset-visits", id); err != nil {
		t.Fatal(err)
	}
	out, _ := runRoot(t, dir, "metadata", "show", id)
	var m map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &m)
	if _, ok := m["step_visits"]; ok {
		t.Fatalf("step_visits should be gone, got %v", m)
	}
}

// TestMetadata_ResetVisitsByStepID removes a single counter by step id.
func TestMetadata_ResetVisitsByStepID(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "one")
	if _, err := runRoot(t, dir, "metadata", "set", id, "--key", "step_visits.st-a", "--value", "3"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "metadata", "set", id, "--key", "step_visits.st-b", "--value", "5"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "metadata", "reset-visits", id, "--step-id", "st-a"); err != nil {
		t.Fatal(err)
	}
	out, _ := runRoot(t, dir, "metadata", "show", id)
	var m map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &m)
	sv, _ := m["step_visits"].(map[string]any)
	if _, ok := sv["st-a"]; ok {
		t.Errorf("st-a should be gone, got %v", sv)
	}
	if v, _ := sv["st-b"].(float64); int(v) != 5 {
		t.Errorf("st-b counter should remain at 5, got %v", sv["st-b"])
	}
}

// TestMetadata_ResetVisitsByStepName resolves the step name against the
// task's workflow and clears the corresponding counter.
func TestMetadata_ResetVisitsByStepName(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	// Set up a workflow with two steps so we can resolve a name.
	wfPath := writeFeatureDevWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "namedreset")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "feature-dev-generic"); err != nil {
		t.Fatal(err)
	}
	// At this point step_visits[<dev id>] = 1. Look up dev's step id via
	// `workflow show --json` so we can verify the counter is gone after
	// reset.
	wfBody, _ := runRoot(t, dir, "workflow", "show", "feature-dev-generic", "--json")
	var wfShow map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(wfBody)), &wfShow)
	steps, _ := wfShow["steps"].([]any)
	var devID string
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		if sm["name"] == "dev" {
			devID, _ = sm["id"].(string)
		}
	}
	if devID == "" {
		t.Fatalf("could not resolve dev step id from %v", wfShow)
	}

	if _, err := runRoot(t, dir, "metadata", "reset-visits", id, "--step", "dev"); err != nil {
		t.Fatal(err)
	}
	out, _ := runRoot(t, dir, "metadata", "show", id)
	var m map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &m)
	if sv, ok := m["step_visits"].(map[string]any); ok {
		if _, present := sv[devID]; present {
			t.Errorf("dev counter should be cleared, got %v", sv)
		}
	}
}

// TestMetadata_SetFromFile reads a JSON literal from --json-value FILE.
func TestMetadata_SetFromFile(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "json-from-file")
	fp := filepath.Join(dir, "val.json")
	if err := os.WriteFile(fp, []byte(`{"x":1,"y":[true,false]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "metadata", "set", id, "--key", "blob", "--json-value", fp); err != nil {
		t.Fatal(err)
	}
	out, _ := runRoot(t, dir, "metadata", "show", id)
	var m map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &m)
	blob, _ := m["blob"].(map[string]any)
	if v, _ := blob["x"].(float64); int(v) != 1 {
		t.Errorf("blob.x: %v", blob["x"])
	}
}

// TestShow_RendersVisitsSummary verifies the human renderer surfaces a
// `visits:` line when step_visits is populated.
func TestShow_RendersVisitsSummary(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeCappedWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "capped")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "capped"); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, dir, "show", id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "visits:") {
		t.Fatalf("show should render a visits: line, got:\n%s", out)
	}
	// First enroll bumps the dev counter to 1/2.
	if !strings.Contains(out, "dev 1/2") {
		t.Errorf("expected 'dev 1/2' in visits summary, got:\n%s", out)
	}
}

// TestShow_JSONIncludesMetadata verifies `show --json` carries the raw
// metadata map under a `metadata` key, including the engine-managed
// step_visits sub-object after an enroll.
func TestShow_JSONIncludesMetadata(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeCappedWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "json-md")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "capped"); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, dir, "show", id, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatal(err)
	}
	md, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata key missing or not object: %v", got)
	}
	sv, ok := md["step_visits"].(map[string]any)
	if !ok || len(sv) != 1 {
		t.Fatalf("step_visits missing or wrong shape: %v", md)
	}
}

// writeCappedWorkflow writes a tiny capped two-step workflow named
// "capped" referencing the @autosk/dev-fixture stub agent.
func writeCappedWorkflow(t *testing.T, dir string) string {
	t.Helper()
	wf := map[string]any{
		"name":       "capped",
		"first_step": "dev",
		"steps": map[string]any{
			"dev": map[string]any{
				"agent":      map[string]any{"name": "@autosk/dev-fixture"},
				"max_visits": 2,
				"next_steps": []any{
					map[string]any{"step": "review", "prompt_rule": "."},
				},
			},
			"review": map[string]any{
				"agent":      map[string]any{"name": "@autosk/dev-fixture"},
				"max_visits": 2,
				"next_steps": []any{
					map[string]any{"step": "dev", "prompt_rule": "."},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	path := filepath.Join(dir, "capped.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
