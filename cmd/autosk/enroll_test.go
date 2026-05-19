package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFeatureDevWorkflow writes a minimal multi-step workflow file
// (dev → review → done) referencing the @autosk/dev-fixture stub agent
// that the in-process fake npm runner knows how to install.
func writeFeatureDevWorkflow(t *testing.T, dir string) string {
	t.Helper()
	wf := map[string]any{
		"name":       "feature-dev-generic",
		"first_step": "dev",
		"steps": map[string]any{
			"dev": map[string]any{
				"agent": map[string]any{"name": "@autosk/dev-fixture"},
				"next_steps": []any{
					map[string]any{"step": "review", "prompt_rule": "after implementation"},
				},
			},
			"review": map[string]any{
				"agent": map[string]any{"name": "@autosk/dev-fixture"},
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "after review"},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	path := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// createBareTask creates a status='new' task and returns its id.
func createBareTask(t *testing.T, dir, title string) string {
	t.Helper()
	out, err := runRoot(t, dir, "create", title)
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	// Strip any prefatory lines (e.g. "autosk: created ..."), take last line.
	if i := strings.LastIndex(id, "\n"); i >= 0 {
		id = id[i+1:]
	}
	if !strings.HasPrefix(id, "as-") {
		t.Fatalf("create did not return an id; got %q", out)
	}
	return id
}

// statusOf returns the status field of `autosk show --json <id>`.
func statusOf(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	out, err := runRoot(t, dir, "show", id, "--json")
	if err != nil {
		t.Fatalf("show --json: %v\n%s", err, out)
	}
	// runRoot returns the captured stdout which is exactly the JSON line.
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("unmarshal show output: %v\nraw=%s", err, out)
	}
	return got
}

// TestEnroll_IntoNamedWorkflow_Happy verifies the workflow path:
//
//	create (status=new) → enroll --workflow → in_workflow at first step.
func TestEnroll_IntoNamedWorkflow_Happy(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	wfPath := writeFeatureDevWorkflow(t, dir)
	if out, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatalf("workflow create: %v\n%s", err, out)
	}
	id := createBareTask(t, dir, "Implement enroll")

	pre := statusOf(t, dir, id)
	if pre["status"] != "new" {
		t.Fatalf("pre-state should be 'new', got %v", pre["status"])
	}

	out, err := runRoot(t, dir, "enroll", id, "--workflow", "feature-dev-generic")
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}

	got := statusOf(t, dir, id)
	if got["status"] != "in_workflow" {
		t.Fatalf("expected status=in_workflow, got %v", got["status"])
	}
	if got["current_step"] != "dev" {
		t.Fatalf("expected current_step=dev (first step), got %v", got["current_step"])
	}

	// Plan acceptance §1: workflow_id must point at THIS workflow, not
	// some other one that happens to expose a step named "dev" (e.g. a
	// synthetic single:* substituted by a regression).
	wfShow, err := runRoot(t, dir, "workflow", "show", "feature-dev-generic", "--json")
	if err != nil {
		t.Fatalf("workflow show --json: %v\n%s", err, wfShow)
	}
	var wfMeta map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(wfShow)), &wfMeta); err != nil {
		t.Fatalf("unmarshal workflow show: %v\nraw=%s", err, wfShow)
	}
	wfID, _ := wfMeta["id"].(string)
	if wfID == "" {
		t.Fatalf("workflow show payload missing id: %v", wfMeta)
	}
	if got["workflow_id"] != wfID {
		t.Fatalf("workflow_id mismatch: task says %v, workflow says %s", got["workflow_id"], wfID)
	}
}

// TestEnroll_IntoSingleAgent_AutoCreatesSyntheticWorkflow verifies the
// --agent shorthand auto-creates single:<NAME> on first use and pins the
// task to step "do".
func TestEnroll_IntoSingleAgent_AutoCreatesSyntheticWorkflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "Ship the thing")

	// Before enroll the synthetic workflow doesn't exist.
	beforeList, err := runRoot(t, dir, "workflow", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(beforeList, "single:@autosk/dev-fixture") {
		t.Fatalf("synthetic workflow should NOT exist yet:\n%s", beforeList)
	}

	if out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("enroll --agent: %v\n%s", err, out)
	}

	afterList, err := runRoot(t, dir, "workflow", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(afterList, "single:@autosk/dev-fixture") {
		t.Fatalf("expected synthetic workflow created:\n%s", afterList)
	}
	got := statusOf(t, dir, id)
	if got["status"] != "in_workflow" {
		t.Fatalf("status: %v", got["status"])
	}
	if got["current_step"] != "do" {
		t.Fatalf("current_step: %v", got["current_step"])
	}
}

// TestEnroll_AlreadyEnrolled_Rejected verifies a second enroll on an
// already-in_workflow task is refused with a message that points the
// user at the engine + cancel/reopen path — NOT at `resume`, which
// only handles human_feedback (covered by its own test below).
func TestEnroll_AlreadyEnrolled_Rejected(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")
	if _, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture")
	if err == nil {
		t.Fatal("expected enroll on already-enrolled task to fail")
	}
	if !strings.Contains(err.Error(), "already enrolled") {
		t.Errorf("error should mention 'already enrolled', got: %v", err)
	}
	if !strings.Contains(err.Error(), "cancel") || !strings.Contains(err.Error(), "reopen") {
		t.Errorf("in_workflow hint should mention cancel+reopen, got: %v", err)
	}
	// `resume` advice is for human_feedback only; it must NOT appear in
	// the in_workflow rejection (resume.go hard-rejects in_workflow).
	if strings.Contains(err.Error(), "resume") {
		t.Errorf("in_workflow hint should NOT advise `resume`, got: %v", err)
	}
}

// TestEnroll_HumanFeedback_Rejected covers the second already-enrolled
// branch: a task parked at `human_feedback` should be told about
// `autosk resume --to` rather than the cancel/reopen path that
// in_workflow gets.
func TestEnroll_HumanFeedback_Rejected(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "Park me")
	if _, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	// Park the task in human_feedback the cheap way — raw SQL bypasses
	// the daemon/step engine but keeps current_step_id intact (DB CHECK
	// allows human_feedback with a step set).
	q := fmt.Sprintf("UPDATE tasks SET status='human_feedback' WHERE id='%s'", id)
	if out, err := runRoot(t, dir, "sql", "--write", q); err != nil {
		t.Fatalf("force human_feedback: %v\n%s", err, out)
	}
	pre := statusOf(t, dir, id)
	if pre["status"] != "human_feedback" {
		t.Fatalf("setup failed: status=%v", pre["status"])
	}

	_, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture")
	if err == nil {
		t.Fatal("expected enroll on human_feedback task to fail")
	}
	if !strings.Contains(err.Error(), "human feedback") {
		t.Errorf("error should reference human feedback, got: %v", err)
	}
	if !strings.Contains(err.Error(), "resume") {
		t.Errorf("human_feedback hint should mention `resume`, got: %v", err)
	}
}

// TestEnroll_TerminalTask_Rejected verifies enroll refuses a done task
// with a message pointing at `reopen`.
func TestEnroll_TerminalTask_Rejected(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")
	if _, err := runRoot(t, dir, "done", id); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture")
	if err == nil {
		t.Fatal("expected enroll on done task to fail")
	}
	if !strings.Contains(err.Error(), "terminal") || !strings.Contains(err.Error(), "reopen") {
		t.Errorf("error should mention 'terminal' and 'reopen', got: %v", err)
	}
}

// TestEnroll_FlagValidation covers the mutual-exclusivity and
// at-least-one-required rules on --workflow / --agent.
func TestEnroll_FlagValidation(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")

	// Neither flag.
	if _, err := runRoot(t, dir, "enroll", id); err == nil {
		t.Fatal("expected error when neither --workflow nor --agent is given")
	}
	// Both flags.
	_, err := runRoot(t, dir, "enroll", id, "--workflow", "foo", "--agent", "bar")
	if err == nil {
		t.Fatal("expected error when both --workflow and --agent are given")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion: %v", err)
	}
}

// TestEnroll_TaskNotFound covers the case where the id doesn't exist.
func TestEnroll_TaskNotFound(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, dir, "enroll", "as-zzzz", "--workflow", "anything")
	if err == nil {
		t.Fatal("expected error for missing task id")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("error should say 'task not found': %v", err)
	}
}

// TestEnroll_UnknownWorkflow surfaces a clean error rather than a SQL
// trace when --workflow names a workflow that doesn't exist.
func TestEnroll_UnknownWorkflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")
	_, err := runRoot(t, dir, "enroll", id, "--workflow", "no-such-wf")
	if err == nil {
		t.Fatal("expected error for unknown workflow")
	}
	if !strings.Contains(err.Error(), "workflow not found") {
		t.Errorf("error should mention 'workflow not found': %v", err)
	}
}

// TestEnroll_RejectsUninstalledAgent mirrors TestCreate_RejectsUninstalledAgent
// (agent_install_test.go) for the --agent shorthand path: the resolver
// must reject unknown packages with the canonical `agent_not_installed`
// sentinel, so a regression that silently created an orphan synthetic
// workflow would be caught here.
func TestEnroll_RejectsUninstalledAgent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")
	_, err := runRoot(t, dir, "enroll", id, "--agent", "@noone/here")
	if err == nil {
		t.Fatal("expected agent_not_installed rejection")
	}
	if !strings.Contains(err.Error(), "agent_not_installed") {
		t.Errorf("wrong error: %v", err)
	}
	// And the task must be untouched (status still `new`).
	got := statusOf(t, dir, id)
	if got["status"] != "new" {
		t.Errorf("task should remain `new` after failed enroll, got: %v", got["status"])
	}
	// No synthetic workflow should have been auto-created behind the
	// scenes — EnsureByName fails before EnsureSingle is called.
	wfs, err := runRoot(t, dir, "workflow", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wfs, "single:@noone/here") {
		t.Errorf("failed enroll must not create synthetic workflow:\n%s", wfs)
	}
}

// TestEnroll_AtSpecificStep_Workflow verifies the --step flag lands the
// task on the named step instead of the workflow's first_step. The
// fixture workflow above has `dev` as first_step and `review` as a
// second step; we enroll directly into `review` and check both the
// status and the resolved step name.
func TestEnroll_AtSpecificStep_Workflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeFeatureDevWorkflow(t, dir)
	if out, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatalf("workflow create: %v\n%s", err, out)
	}
	id := createBareTask(t, dir, "Land at review")

	if out, err := runRoot(t, dir, "enroll", id, "--workflow", "feature-dev-generic", "--step", "review"); err != nil {
		t.Fatalf("enroll --step: %v\n%s", err, out)
	}
	got := statusOf(t, dir, id)
	if got["status"] != "in_workflow" {
		t.Errorf("status: want in_workflow, got %v", got["status"])
	}
	if got["current_step"] != "review" {
		t.Errorf("current_step: want review (not first_step `dev`), got %v", got["current_step"])
	}
}

// TestEnroll_AtSpecificStep_Unknown surfaces a clean, helpful error
// when --step names a step that doesn't exist in the chosen workflow.
// The error must include the available step names so the user can fix
// the typo without a separate `workflow show`.
func TestEnroll_AtSpecificStep_Unknown(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeFeatureDevWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")

	_, err := runRoot(t, dir, "enroll", id, "--workflow", "feature-dev-generic", "--step", "docs")
	if err == nil {
		t.Fatal("expected enroll to fail for unknown step name")
	}
	if !strings.Contains(err.Error(), "step") || !strings.Contains(err.Error(), "docs") {
		t.Errorf("error should mention the bad step name: %v", err)
	}
	if !strings.Contains(err.Error(), "dev") || !strings.Contains(err.Error(), "review") {
		t.Errorf("error should list available step names (dev, review): %v", err)
	}
	// Task must be untouched on failure.
	if s := statusOf(t, dir, id); s["status"] != "new" {
		t.Errorf("task should remain `new` after failed enroll, got: %v", s["status"])
	}
}

// TestEnroll_StepIncompatibleWithAgent guards the rejection rule for
// --step + --agent. single:<agent> workflows have one step by
// construction, so combining --agent with --step is nonsensical and
// the CLI must say so up front.
func TestEnroll_StepIncompatibleWithAgent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")

	_, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture", "--step", "do")
	if err == nil {
		t.Fatal("expected enroll to reject --agent + --step")
	}
	if !strings.Contains(err.Error(), "--step") || !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("error should explain --step requires --workflow, got: %v", err)
	}
}

// TestCreate_AtSpecificStep_Workflow is the create-side mirror of
// TestEnroll_AtSpecificStep_Workflow: create + --workflow + --step must
// land the brand-new task on the named step rather than first_step.
func TestCreate_AtSpecificStep_Workflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeFeatureDevWorkflow(t, dir)
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "create", "Land at review on create", "--workflow", "feature-dev-generic", "--step", "review")
	if err != nil {
		t.Fatalf("create --step: %v\n%s", err, out)
	}
	id := strings.TrimSpace(out)
	if i := strings.LastIndex(id, "\n"); i >= 0 {
		id = id[i+1:]
	}
	got := statusOf(t, dir, id)
	if got["status"] != "in_workflow" {
		t.Errorf("status: want in_workflow, got %v", got["status"])
	}
	if got["current_step"] != "review" {
		t.Errorf("current_step: want review, got %v", got["current_step"])
	}
}

// TestCreate_StepWithoutWorkflow rejects `create --step` when there's
// no --workflow / --agent to anchor it; `--step` is meaningless without
// a target workflow.
func TestCreate_StepWithoutWorkflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, dir, "create", "X", "--step", "review")
	if err == nil {
		t.Fatal("expected create --step (without --workflow) to fail")
	}
	if !strings.Contains(err.Error(), "--step") || !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("error should explain --step requires --workflow, got: %v", err)
	}
}

// TestEnroll_JSONOutput verifies --json round-trips through enroll and
// emits the expected shape (matches `show --json`), INCLUDING the
// derived `blocked` / `blocked_by` / `blocks` fields — the plan's
// shape-parity guarantee (acceptance §5). To exercise that, we create
// a blocker task and assert the enroll output sees it.
func TestEnroll_JSONOutput(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	// Create a blocker first, then a target that's blocked by it. The
	// blocker stays in `new`, so it stays an open dependency.
	blocker := createBareTask(t, dir, "Blocker")
	targetOut, err := runRoot(t, dir, "create", "JSON me", "--blocked-by", blocker)
	if err != nil {
		t.Fatalf("create --blocked-by: %v\n%s", err, targetOut)
	}
	id := strings.TrimSpace(targetOut)
	if i := strings.LastIndex(id, "\n"); i >= 0 {
		id = id[i+1:]
	}

	out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture", "--json")
	if err != nil {
		t.Fatalf("enroll --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, out)
	}
	if got["id"] != id {
		t.Errorf("id mismatch: %v vs %v", got["id"], id)
	}
	if got["status"] != "in_workflow" {
		t.Errorf("status: %v", got["status"])
	}
	if got["current_step"] != "do" {
		t.Errorf("current_step: %v", got["current_step"])
	}

	// Shape-parity with `show --json`: `blocked` must be true and
	// `blocked_by` must list the blocker id.
	if blocked, _ := got["blocked"].(bool); !blocked {
		t.Errorf("blocked: expected true, got %v", got["blocked"])
	}
	bb, _ := got["blocked_by"].([]any)
	if len(bb) != 1 || bb[0] != blocker {
		t.Errorf("blocked_by: expected [%s], got %v", blocker, got["blocked_by"])
	}

	// And confirm shape parity by diffing the keysets against `show --json`.
	showOut, err := runRoot(t, dir, "show", id, "--json")
	if err != nil {
		t.Fatalf("show --json: %v\n%s", err, showOut)
	}
	var shown map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(showOut)), &shown); err != nil {
		t.Fatalf("unmarshal show: %v\nraw=%s", err, showOut)
	}
	for _, k := range []string{"id", "status", "workflow_id", "current_step", "current_agent", "blocked", "blocked_by", "blocks"} {
		if fmt.Sprintf("%v", got[k]) != fmt.Sprintf("%v", shown[k]) {
			t.Errorf("shape parity diverged on %q: enroll=%v show=%v", k, got[k], shown[k])
		}
	}
}
