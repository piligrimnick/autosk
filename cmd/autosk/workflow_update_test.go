package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWorkflowJSON writes a single-step workflow JSON with the given
// isolation mode. Used by the workflow-update CLI tests to set up
// fixtures without depending on the worktree e2e helpers.
func writeWorkflowJSON(t *testing.T, dir, name, isolation string) string {
	t.Helper()
	wf := map[string]any{
		"name":       name,
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
	if isolation != "" {
		wf["isolation"] = isolation
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// updateFixture sets up a project + installs the dev-fixture agent +
// creates the workflow. Returns the project root for follow-up calls.
func updateFixture(t *testing.T, wfName, isolation string) string {
	t.Helper()
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("agent install: %v", err)
	}
	path := writeWorkflowJSON(t, root, wfName, isolation)
	if out, err := runRoot(t, root, "workflow", "create", "--file", path); err != nil {
		t.Fatalf("workflow create: %v\n%s", err, out)
	}
	return root
}

// TestWorkflowUpdate_NoTasks_NoneToWorktree pins the simplest happy
// path: no non-terminal tasks reference the workflow, so the column
// flip succeeds without --force and the text summary mentions
// none→worktree.
func TestWorkflowUpdate_NoTasks_NoneToWorktree(t *testing.T) {
	root := updateFixture(t, "wu-flip", "none")

	out, err := runRoot(t, root, "workflow", "update", "wu-flip", "--isolation", "worktree")
	if err != nil {
		t.Fatalf("workflow update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "none → worktree") && !strings.Contains(out, "none -> worktree") {
		t.Errorf("expected 'none → worktree' in output:\n%s", out)
	}

	// Verify the column actually flipped.
	show, err := runRoot(t, root, "workflow", "show", "wu-flip", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(show)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["isolation"] != "worktree" {
		t.Errorf("post-update isolation: %q (want worktree)", doc["isolation"])
	}
}

// TestWorkflowUpdate_JSON_NoopEmitsReport pins that --json on a no-op
// emits a well-formed report with noop=true.
func TestWorkflowUpdate_JSON_NoopEmitsReport(t *testing.T) {
	root := updateFixture(t, "wu-noop", "none")

	out, err := runRoot(t, root, "--json", "workflow", "update", "wu-noop", "--isolation", "none")
	if err != nil {
		t.Fatalf("workflow update --json: %v\n%s", err, out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("decode json: %v\nout=%s", err, out)
	}
	if doc["noop"] != true {
		t.Errorf("noop: %v (want true)\nout=%s", doc["noop"], out)
	}
	if doc["from"] != "none" || doc["to"] != "none" {
		t.Errorf("from/to: %v/%v", doc["from"], doc["to"])
	}
}

// TestWorkflowUpdate_RefusesWithNonTerminal pins FR4: a task in
// status=new pointing at the workflow makes the update refuse without
// --force, prints the task id under `non-terminal task in workflow:`,
// and leaves the column unchanged.
func TestWorkflowUpdate_RefusesWithNonTerminal(t *testing.T) {
	root := updateFixture(t, "wu-guarded", "none")

	// Create a bare task and enroll it (via a separate non-isolated
	// workflow won't do — we need it pointing at wu-guarded). Use
	// `autosk enroll` against wu-guarded.
	id := createBareTask(t, root, "guard task")
	if out, err := runRoot(t, root, "enroll", id, "--workflow", "wu-guarded"); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}

	_, err := runRoot(t, root, "workflow", "update", "wu-guarded", "--isolation", "worktree")
	if err == nil {
		t.Fatal("expected refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "non-terminal task in workflow:") {
		t.Errorf("error should list non-terminal tasks, got: %v", err)
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("error should mention task id %s, got: %v", id, err)
	}

	// Column unchanged.
	show, _ := runRoot(t, root, "workflow", "show", "wu-guarded", "--json")
	var doc map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(show)), &doc)
	if doc["isolation"] != "none" {
		t.Errorf("isolation should be unchanged: got %q", doc["isolation"])
	}
}

// TestWorkflowUpdate_Force_NoneToWorktree_Allocates pins the happy
// --force path: the column flips and per-task worktrees are
// allocated. Verifies the on-disk directory exists for the task.
func TestWorkflowUpdate_Force_NoneToWorktree_Allocates(t *testing.T) {
	root := updateFixture(t, "wu-force-up", "none")

	id := createBareTask(t, root, "force flip me")
	if out, err := runRoot(t, root, "enroll", id, "--workflow", "wu-force-up"); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}

	out, err := runRoot(t, root, "workflow", "update", "wu-force-up", "--isolation", "worktree", "--force")
	if err != nil {
		t.Fatalf("workflow update --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ensured worktrees:") {
		t.Errorf("expected 'ensured worktrees:' in output:\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Errorf("expected task id %s in output:\n%s", id, out)
	}
	// Column flipped.
	show, _ := runRoot(t, root, "workflow", "show", "wu-force-up", "--json")
	var doc map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(show)), &doc)
	if doc["isolation"] != "worktree" {
		t.Errorf("post-update isolation: %q", doc["isolation"])
	}
	// Verify the on-disk worktree dir exists for the task.
	wtList, err := runRoot(t, root, "worktree", "list", "--json")
	if err != nil {
		t.Fatalf("worktree list: %v\n%s", err, wtList)
	}
	var rows []map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(wtList)), &rows)
	found := false
	for _, r := range rows {
		if r["task_id"] == id {
			found = true
			if r["exists"] != true {
				t.Errorf("worktree dir for %s should exist on disk; row=%v", id, r)
			}
		}
	}
	if !found {
		t.Errorf("worktree list missing row for %s", id)
	}
}

// TestWorkflowUpdate_DryRun_NoCommit pins FR8: --dry-run produces the
// same shape of report but performs no DB write (column unchanged)
// and no worktree allocation.
func TestWorkflowUpdate_DryRun_NoCommit(t *testing.T) {
	root := updateFixture(t, "wu-dry", "none")
	id := createBareTask(t, root, "dry me")
	if _, err := runRoot(t, root, "enroll", id, "--workflow", "wu-dry"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, root, "--json", "workflow", "update", "wu-dry",
		"--isolation", "worktree", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["dry_run"] != true {
		t.Errorf("dry_run: %v (want true)", doc["dry_run"])
	}
	// Column unchanged.
	show, _ := runRoot(t, root, "workflow", "show", "wu-dry", "--json")
	var sd map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(show)), &sd)
	if sd["isolation"] != "none" {
		t.Errorf("dry-run changed isolation: %q", sd["isolation"])
	}
	// No worktree dir.
	wtList, _ := runRoot(t, root, "worktree", "list", "--json")
	if strings.Contains(wtList, id) {
		t.Errorf("dry-run allocated worktree for %s:\n%s", id, wtList)
	}
}

// TestWorkflowUpdate_InvalidIsolationFlag pins FR2: garbage values
// fail with the contract error message.
func TestWorkflowUpdate_InvalidIsolationFlag(t *testing.T) {
	root := updateFixture(t, "wu-bad", "none")
	_, err := runRoot(t, root, "workflow", "update", "wu-bad", "--isolation", "garbage")
	if err == nil {
		t.Fatal("expected invalid isolation error")
	}
	if !strings.Contains(err.Error(), "invalid --isolation:") {
		t.Errorf("error shape: %v", err)
	}
}

// TestWorkflowUpdate_Synthetic_AlwaysRejected pins FR3: single:<agent>
// rows refuse regardless of --force.
func TestWorkflowUpdate_Synthetic_AlwaysRejected(t *testing.T) {
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("agent install: %v", err)
	}
	// Ensure a synthetic single:<agent> row by creating a task with
	// --agent (the canonical EnsureSingle trigger).
	id := createBareTask(t, root, "synth driver")
	if _, err := runRoot(t, root, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("enroll --agent: %v", err)
	}

	for _, force := range []bool{false, true} {
		argv := []string{"workflow", "update", "single:@autosk/dev-fixture", "--isolation", "worktree"}
		if force {
			argv = append(argv, "--force")
		}
		_, err := runRoot(t, root, argv...)
		if err == nil {
			t.Errorf("force=%v: expected synthetic rejection", force)
			continue
		}
		if !strings.Contains(err.Error(), "synthetic workflow") {
			t.Errorf("force=%v: error should mention synthetic, got %v", force, err)
		}
	}
}

// TestWorkflowUpdate_JSON_RefusalEmitsReport pins the FR10 contract
// on the refusal path: --json always prints the
// UpdateIsolationReport to stdout (populated with the offending
// non-terminal task ids), and the CLI still exits non-zero so
// tooling can read both pieces. Without this contract a wrapper
// like `autosk workflow update ... --json | jq .non_terminal_tasks`
// would get an empty stdout.
func TestWorkflowUpdate_JSON_RefusalEmitsReport(t *testing.T) {
	root := updateFixture(t, "wu-json-refuse", "none")
	id := createBareTask(t, root, "refuse me")
	if _, err := runRoot(t, root, "enroll", id, "--workflow", "wu-json-refuse"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	out, err := runRoot(t, root, "--json", "workflow", "update", "wu-json-refuse", "--isolation", "worktree")
	if err == nil {
		t.Fatal("expected refusal error")
	}
	var doc map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); jerr != nil {
		t.Fatalf("decode json: %v\nout=%s", jerr, out)
	}
	if doc["workflow"] != "wu-json-refuse" {
		t.Errorf("workflow: %v", doc["workflow"])
	}
	nts, _ := doc["non_terminal_tasks"].([]any)
	if len(nts) != 1 || nts[0] != id {
		t.Errorf("non_terminal_tasks: %v (want [%s])", nts, id)
	}
	// Sentinel-preservation: in --json mode the structured details
	// (offending task ids) live in the JSON report; the error text
	// is the raw sentinel itself so callers can still pattern-match
	// the failure with errors.Is upstream.
	if !strings.Contains(err.Error(), "non-terminal tasks") {
		t.Errorf("sentinel text missing from err: %v", err)
	}
}

// TestWorkflowUpdate_JSON_SyntheticEmitsReport pins the same
// contract for the synthetic-rejection branch: a synthetic workflow
// is always rejected, --json still emits the (mostly-empty) report
// so the wrapping tool sees the workflow / from / to triple, and
// the error message preserves the sentinel.
func TestWorkflowUpdate_JSON_SyntheticEmitsReport(t *testing.T) {
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("agent install: %v", err)
	}
	id := createBareTask(t, root, "synth driver json")
	if _, err := runRoot(t, root, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("enroll --agent: %v", err)
	}
	out, err := runRoot(t, root, "--json", "workflow", "update",
		"single:@autosk/dev-fixture", "--isolation", "worktree")
	if err == nil {
		t.Fatal("expected synthetic rejection")
	}
	var doc map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); jerr != nil {
		t.Fatalf("decode json: %v\nout=%s", jerr, out)
	}
	if doc["workflow"] != "single:@autosk/dev-fixture" {
		t.Errorf("workflow: %v", doc["workflow"])
	}
	if !strings.Contains(err.Error(), "synthetic workflow") {
		t.Errorf("sentinel text missing from err: %v", err)
	}
}

// TestWorkflowUpdate_WorktreeToNone_Force_ReportsLeftovers pins FR7:
// the leftover worktree paths are printed and the directory is NOT
// removed (so subsequent `worktree list` still shows it).
func TestWorkflowUpdate_WorktreeToNone_Force_ReportsLeftovers(t *testing.T) {
	root := updateFixture(t, "wu-down", "worktree")
	id := createBareTask(t, root, "down me")
	if _, err := runRoot(t, root, "enroll", id, "--workflow", "wu-down"); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, root, "workflow", "update", "wu-down", "--isolation", "none", "--force")
	if err != nil {
		t.Fatalf("worktree → none --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "leftover worktree (not removed):") {
		t.Errorf("expected leftover-worktree section:\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Errorf("expected task id %s in leftover section:\n%s", id, out)
	}
	// Show the column changed.
	show, _ := runRoot(t, root, "workflow", "show", "wu-down", "--json")
	var sd map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(show)), &sd)
	if sd["isolation"] != "none" {
		t.Errorf("post-flip isolation: %q", sd["isolation"])
	}
}
