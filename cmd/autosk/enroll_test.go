package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/worktree"
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
// Uses the shared lastLine helper (init_test.go) so the id-extraction
// rule lives in exactly one place across the test suite.
func createBareTask(t *testing.T, dir, title string) string {
	t.Helper()
	out, err := runRoot(t, dir, "create", title)
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	id := lastLine(out)
	if !strings.HasPrefix(id, "ask-") || len(id) != 10 {
		t.Fatalf("create did not return an ask-XXXXXX id; got %q", out)
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
//	create (status=new) → enroll --workflow → work at first step.
func TestEnroll_IntoNamedWorkflow_Happy(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if got["status"] != "work" {
		t.Fatalf("expected status=work, got %v", got["status"])
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if got["status"] != "work" {
		t.Fatalf("status: %v", got["status"])
	}
	if got["current_step"] != "do" {
		t.Fatalf("current_step: %v", got["current_step"])
	}
}

// TestEnroll_AlreadyEnrolled_Rejected verifies a second enroll on an
// already-work task is refused with a message that points the
// user at the engine + cancel/reopen path — NOT at `resume`, which
// only handles human (covered by its own test below).
func TestEnroll_AlreadyEnrolled_Rejected(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
		t.Errorf("work hint should mention cancel+reopen, got: %v", err)
	}
	// `resume` advice is for human only; it must NOT appear in
	// the work rejection (resume.go hard-rejects work).
	if strings.Contains(err.Error(), "resume") {
		t.Errorf("work hint should NOT advise `resume`, got: %v", err)
	}
}

// TestEnroll_FromHuman_OK verifies a task parked at `human` accepts
// enroll cleanly (no need to resume/reopen first). The chosen
// workflow's first step is stamped and status flips to `work`.
func TestEnroll_FromHuman_OK(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "Park me")
	if _, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	// Park the task in human the cheap way — raw SQL bypasses
	// the daemon/step engine but keeps current_step_id intact (DB CHECK
	// allows human with a step set).
	q := fmt.Sprintf("UPDATE tasks SET status='human' WHERE id='%s'", id)
	if out, err := runRoot(t, dir, "sql", "--write", q); err != nil {
		t.Fatalf("force human: %v\n%s", err, out)
	}
	pre := statusOf(t, dir, id)
	if pre["status"] != "human" {
		t.Fatalf("setup failed: status=%v", pre["status"])
	}

	if out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("enroll from human: %v\n%s", err, out)
	}
	got := statusOf(t, dir, id)
	if got["status"] != "work" {
		t.Errorf("status: want work, got %v", got["status"])
	}
	if got["current_step"] != "do" {
		t.Errorf("current_step: want do (single:<agent> entry), got %v", got["current_step"])
	}
	if got["workflow_id"] == nil || got["workflow_id"] == "" {
		t.Errorf("workflow_id should be stamped on the task, got %v", got["workflow_id"])
	}
}

// TestEnroll_FromHuman_SwitchWorkflow verifies enroll on a human task
// can flip workflow_id to a different workflow and lands on the new
// workflow's first step. Pins the design promise that enroll on
// human is the canonical "switch workflows" verb (no
// cancel + reopen detour required).
func TestEnroll_FromHuman_SwitchWorkflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	// Workflow A = feature-dev-generic-style (multi-step `dev`/`review`).
	wfPath := writeFeatureDevWorkflow(t, dir)
	if out, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatalf("workflow create A: %v\n%s", err, out)
	}
	// Workflow B = the single:<agent> shorthand we'll switch into.
	id := createBareTask(t, dir, "switch workflow")
	if _, err := runRoot(t, dir, "enroll", id, "--workflow", "feature-dev-generic"); err != nil {
		t.Fatal(err)
	}
	preShow := statusOf(t, dir, id)
	preWF, _ := preShow["workflow_id"].(string)
	if preWF == "" {
		t.Fatalf("pre-state missing workflow_id: %v", preShow)
	}

	// Park as human via raw SQL (no daemon).
	q := fmt.Sprintf("UPDATE tasks SET status='human' WHERE id='%s'", id)
	if out, err := runRoot(t, dir, "sql", "--write", q); err != nil {
		t.Fatalf("force human: %v\n%s", err, out)
	}

	// Switch to a different workflow (single:@autosk/dev-fixture).
	if out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("switch enroll: %v\n%s", err, out)
	}
	post := statusOf(t, dir, id)
	if post["status"] != "work" {
		t.Errorf("status: want work, got %v", post["status"])
	}
	postWF, _ := post["workflow_id"].(string)
	if postWF == "" || postWF == preWF {
		t.Errorf("workflow_id should have flipped to a different workflow, pre=%q post=%q", preWF, postWF)
	}
	if post["current_step"] != "do" {
		t.Errorf("current_step: want do (new workflow entry), got %v", post["current_step"])
	}
}

// TestEnroll_FromDone_OK verifies enroll succeeds on a terminal
// `done` task without an intervening `reopen` and re-stamps the
// workflow + step pointers (status flips back to work).
func TestEnroll_FromDone_OK(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")
	if _, err := runRoot(t, dir, "done", id); err != nil {
		t.Fatal(err)
	}

	if out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("enroll from done: %v\n%s", err, out)
	}
	got := statusOf(t, dir, id)
	if got["status"] != "work" {
		t.Errorf("status: want work, got %v", got["status"])
	}
	if got["current_step"] != "do" {
		t.Errorf("current_step: want do, got %v", got["current_step"])
	}
	if got["workflow_id"] == nil || got["workflow_id"] == "" {
		t.Errorf("workflow_id should be stamped post-enroll, got %v", got["workflow_id"])
	}
}

// TestEnroll_FromCancel_OK verifies the cancel → enroll fast path.
// Same shape as TestEnroll_FromDone_OK with a cancelled source.
func TestEnroll_FromCancel_OK(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "X")
	if _, err := runRoot(t, dir, "cancel", id); err != nil {
		t.Fatal(err)
	}
	if s := statusOf(t, dir, id); s["status"] != "cancel" {
		t.Fatalf("setup failed: status=%v", s["status"])
	}

	if out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("enroll from cancel: %v\n%s", err, out)
	}
	got := statusOf(t, dir, id)
	if got["status"] != "work" {
		t.Errorf("status: want work, got %v", got["status"])
	}
	if got["current_step"] != "do" {
		t.Errorf("current_step: want do, got %v", got["current_step"])
	}
}

// TestEnroll_FromDone_PreservesStepVisits is the regression fence for
// the "step_visits survives enroll" decision. After running a task to
// done and re-enrolling it into the same workflow, the entry step's
// counter must be >= the count we left behind — i.e. enroll never
// resets the counter. If a future refactor zeroed step_visits at
// enroll time, this test would catch it.
func TestEnroll_FromDone_PreservesStepVisits(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	id := createBareTask(t, dir, "counter survivor")
	if _, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	// Capture the entry step id + the post-first-enroll counter via
	// the metadata column. `show --json` exposes the step *name*
	// (current_step), not the id, so we go through metadata.step_visits
	// which is keyed by step id directly.
	stepID := onlyStepVisitKey(t, dir, id)
	firstCounter := readStepVisits(t, dir, id, stepID)
	if firstCounter < 1 {
		t.Fatalf("step_visits[%s] = %d (expected >= 1 after one enroll)", stepID, firstCounter)
	}

	// Done + re-enroll into the SAME synthetic workflow.
	if _, err := runRoot(t, dir, "done", id); err != nil {
		t.Fatal(err)
	}
	// step_visits must survive the terminal flip (tasksvc.Done only
	// clears current_step_id, never the metadata column).
	if post := readStepVisits(t, dir, id, stepID); post != firstCounter {
		t.Fatalf("step_visits cleared by `done`: pre=%d post=%d", firstCounter, post)
	}
	if out, err := runRoot(t, dir, "enroll", id, "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("second enroll: %v\n%s", err, out)
	}
	afterSecond := readStepVisits(t, dir, id, stepID)
	if afterSecond != firstCounter+1 {
		t.Fatalf("step_visits[%s]: want %d (counter survived + one new bump), got %d",
			stepID, firstCounter+1, afterSecond)
	}
}

// readStepVisits pulls metadata.step_visits[stepID] out of
// `autosk metadata show --json` so the regression test can assert on
// the counter without reaching into the store directly. Returns 0 when
// the key is absent (matches the engine's "never visited" semantics).
func readStepVisits(t *testing.T, dir, id, stepID string) int {
	t.Helper()
	out, err := runRoot(t, dir, "metadata", "show", id, "--json")
	if err != nil {
		t.Fatalf("metadata show: %v\n%s", err, out)
	}
	var md map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &md); err != nil {
		t.Fatalf("unmarshal metadata: %v\nraw=%s", err, out)
	}
	sv, _ := md["step_visits"].(map[string]any)
	if sv == nil {
		return 0
	}
	v, _ := sv[stepID].(float64)
	return int(v)
}

// onlyStepVisitKey returns the sole key present under
// metadata.step_visits. Used by tests that enroll into a synthetic
// single:<agent> workflow (one step → one counter key) and need the
// engine-assigned step id without going through `workflow show`.
func onlyStepVisitKey(t *testing.T, dir, id string) string {
	t.Helper()
	out, err := runRoot(t, dir, "metadata", "show", id, "--json")
	if err != nil {
		t.Fatalf("metadata show: %v\n%s", err, out)
	}
	var md map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &md); err != nil {
		t.Fatalf("unmarshal metadata: %v\nraw=%s", err, out)
	}
	sv, _ := md["step_visits"].(map[string]any)
	if len(sv) != 1 {
		t.Fatalf("step_visits should hold exactly one key, got %d: %v", len(sv), sv)
	}
	for k := range sv {
		return k
	}
	return ""
}

// TestEnroll_FromDone_Isolated_ReusesBranch verifies that enrolling a
// terminal task back into the same isolation=worktree workflow re-
// allocates the per-task worktree dir on the EXISTING `autosk/<id>`
// branch (no --base-ref needed, no fresh branch). The Done path
// removes the on-disk worktree but keeps the branch around for
// exactly this case.
func TestEnroll_FromDone_Isolated_ReusesBranch(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-reenroll")

	out, err := runRoot(t, root, "create", "reuse me", "--workflow", "iso-reenroll")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	id := createIDFromOutput(out)
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Fatalf("expected branch autosk/%s after create", id)
	}
	branchSHA := gitBranchSHA(t, root, "autosk/"+id)

	// done → worktree dir reaped, branch survives.
	if _, err := runRoot(t, root, "done", id); err != nil {
		t.Fatal(err)
	}
	wtPath, _ := worktree.PathFor(root, id)
	if _, statErr := os.Stat(wtPath); statErr == nil {
		t.Fatalf("worktree dir should be reaped after done: %s exists", wtPath)
	}
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Fatalf("branch autosk/%s should survive done", id)
	}

	// enroll back into the same isolated workflow — no --base-ref.
	enrollOut, err := runRoot(t, root, "enroll", id, "--workflow", "iso-reenroll")
	if err != nil {
		t.Fatalf("enroll from done into isolated workflow: %v\n%s", err, enrollOut)
	}
	if strings.Contains(enrollOut, "--base-ref ignored") {
		t.Errorf("enroll without --base-ref must not warn about ignoring it: %s", enrollOut)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("worktree dir should be re-allocated post-enroll at %s: %v", wtPath, statErr)
	}
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Fatalf("branch autosk/%s should still exist after re-enroll", id)
	}
	if got := gitBranchSHA(t, root, "autosk/"+id); got != branchSHA {
		t.Errorf("branch SHA moved across done+re-enroll: pre=%s post=%s (expected reuse, not rebase)", branchSHA, got)
	}
}

// TestEnroll_FlagValidation covers the mutual-exclusivity and
// at-least-one-required rules on --workflow / --agent.
func TestEnroll_FlagValidation(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, dir, "enroll", "ask-zzzzzz", "--workflow", "anything")
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if got["status"] != "work" {
		t.Errorf("status: want work, got %v", got["status"])
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	id := lastLine(out)
	got := statusOf(t, dir, id)
	if got["status"] != "work" {
		t.Errorf("status: want work, got %v", got["status"])
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
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
	id := lastLine(targetOut)

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
	if got["status"] != "work" {
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
