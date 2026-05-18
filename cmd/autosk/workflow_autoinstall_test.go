package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowCreate_AutoInstallsScopedAgent verifies that `autosk
// workflow create` auto-installs any scoped npm agent name referenced
// by a step that isn't yet installed.
func TestWorkflowCreate_AutoInstallsScopedAgent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Write a workflow that references @autosk/dev-fixture (provided by
	// the fakeNpmInProcess fixture used by the rest of this test file).
	wf := map[string]any{
		"name":       "auto-wf",
		"first_step": "do",
		"steps": map[string]any{
			"do": map[string]any{
				"agent": map[string]any{"name": "@autosk/dev-fixture"},
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "when complete"},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	wfPath := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(wfPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "create", "--file", wfPath)
	if err != nil {
		t.Fatalf("workflow create: %v\n%s", err, out)
	}
	if !strings.Contains(out, "installing") {
		t.Errorf("expected auto-install notice in output, got:\n%s", out)
	}
	if !strings.Contains(out, "auto-wf") {
		t.Errorf("expected workflow name in output:\n%s", out)
	}

	// Verify the agent is now in the registry + DB.
	agentList, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agentList, "@autosk/dev-fixture") {
		t.Errorf("agent list missing @autosk/dev-fixture:\n%s", agentList)
	}
}

// TestWorkflowCreate_NoInstallFlag verifies --no-install short-circuits
// the auto-install path and falls back to the standard validation
// error.
func TestWorkflowCreate_NoInstallFlag(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}

	wf := map[string]any{
		"name":       "no-install-wf",
		"first_step": "do",
		"steps": map[string]any{
			"do": map[string]any{
				"agent": map[string]any{"name": "@autosk/dev-fixture"},
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "."},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	wfPath := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(wfPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "workflow", "create", "--file", wfPath, "--no-install")
	if err == nil {
		t.Fatal("expected validation failure with --no-install")
	}
	if !strings.Contains(err.Error(), "autosk agent install @autosk/dev-fixture") {
		t.Errorf("error should suggest install hint: %v", err)
	}
}

// TestWorkflowCreate_BareNameNotAutoInstalled verifies bare (unscoped)
// names still fall through to the validation error rather than being
// blindly installed from the public npm registry.
func TestWorkflowCreate_BareNameNotAutoInstalled(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}

	wf := map[string]any{
		"name":       "bare-wf",
		"first_step": "do",
		"steps": map[string]any{
			"do": map[string]any{
				"agent": map[string]any{"name": "developer"}, // bare name; not auto-installed
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "."},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	wfPath := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(wfPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "workflow", "create", "--file", wfPath)
	if err == nil {
		t.Fatal("bare name should not be auto-installed")
	}
	if !strings.Contains(err.Error(), "autosk agent install developer") {
		t.Errorf("expected install hint for bare name: %v", err)
	}
}

func TestLooksLikeScopedNpmName(t *testing.T) {
	yes := []string{"@autosk/dev", "@a/b", "@scope/sub-name"}
	no := []string{"", "@", "@only", "developer", "code-reviewer", "@/name", "@scope/"}
	for _, s := range yes {
		if !looksLikeScopedNpmName(s) {
			t.Errorf("looksLikeScopedNpmName(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeScopedNpmName(s) {
			t.Errorf("looksLikeScopedNpmName(%q) = true, want false", s)
		}
	}
}
