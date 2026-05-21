package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/worktree"
)

// ----------------------------------------------------------------------
// Shared fixture helpers for the WT3 / WT4 CLI e2e suites.
//
// Each test stands up a fresh project dir that's ALSO a git repo (init
// + one commit so HEAD resolves), points $HOME at a temp dir so the
// worktree allocator lands somewhere t.Cleanup can sweep, and writes
// an isolated workflow JSON the tests then `workflow create`.
// ----------------------------------------------------------------------

// makeIsolatedProject sets up an isolated $HOME, creates `dir` as a
// git repo with one commit on main, and runs `autosk init`. Returns
// the absolute, symlink-resolved project root. Skips when git isn't on
// PATH \u2014 mirrors the convention used by internal/worktree's tests.
func makeIsolatedProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; CLI worktree tests skipped")
	}
	withIsolatedPackagesPrefix(t)
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	runGitOrFail(t, dir, "init", "--initial-branch=main")
	runGitOrFail(t, dir, "config", "user.email", "test@autosk.local")
	runGitOrFail(t, dir, "config", "user.name", "autosk test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOrFail(t, dir, "add", "README.md")
	runGitOrFail(t, dir, "commit", "-m", "init")
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("autosk init: %v", err)
	}
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canon = dir
	}
	return canon
}

func runGitOrFail(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// writeIsolatedWorkflow writes a single-step workflow with
// `isolation: "worktree"` referencing the @autosk/dev-fixture stub
// agent that the in-process fake-npm runner already knows how to
// install. Step `do` transitions to task_status=done.
func writeIsolatedWorkflow(t *testing.T, dir, name string) string {
	t.Helper()
	wf := map[string]any{
		"name":       name,
		"first_step": "do",
		"isolation":  "worktree",
		"steps": map[string]any{
			"do": map[string]any{
				"agent": map[string]any{"name": "@autosk/dev-fixture"},
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "ship"},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeStandardWorkflow writes a single-step workflow WITHOUT
// `isolation: "worktree"` (it defaults to none). Used by the
// non-isolated negative tests for --base-ref.
func writeStandardWorkflow(t *testing.T, dir, name string) string {
	t.Helper()
	wf := map[string]any{
		"name":       name,
		"first_step": "do",
		"steps": map[string]any{
			"do": map[string]any{
				"agent": map[string]any{"name": "@autosk/dev-fixture"},
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "ship"},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// installFixturesAndIsolatedWF installs the dev-fixture agent and
// `workflow create`s the named isolated workflow.
func installFixturesAndIsolatedWF(t *testing.T, dir, wfName string) {
	t.Helper()
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("agent install: %v", err)
	}
	wfPath := writeIsolatedWorkflow(t, dir, wfName)
	if out, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatalf("workflow create: %v\n%s", err, out)
	}
}

// createIDFromOutput pulls the trailing line (the printed task id)
// out of `autosk create` output. Mirrors the helper in enroll_test.go.
func createIDFromOutput(out string) string {
	id := strings.TrimSpace(out)
	if i := strings.LastIndex(id, "\n"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// gitBranchExists checks `git -C root show-ref` for refs/heads/<branch>.
func gitBranchExists(t *testing.T, root, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// gitBranchSHA returns the SHA at the tip of `branch`. Empty string
// on lookup failure.
func gitBranchSHA(t *testing.T, root, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "rev-parse", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ----------------------------------------------------------------------
// WT3 \u2014 `autosk create` on an isolated workflow.
// ----------------------------------------------------------------------

func TestWorktree_CLI_CreateIsolated_AllocatesWorktreeAndBranch(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-create")

	out, err := runRoot(t, root, "create", "isolated me", "--workflow", "iso-create")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	id := createIDFromOutput(out)
	if !strings.HasPrefix(id, "as-") {
		t.Fatalf("expected as-... id in stdout; got %q", out)
	}

	// Worktree directory must be present on disk.
	wtPath, err := worktree.PathFor(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("worktree dir should exist post-create at %s: %v", wtPath, statErr)
	}
	// Branch must exist on the project.
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Fatalf("expected branch autosk/%s to exist on the project", id)
	}
}

// TestWorktree_CLI_CreateIsolated_NonGitRollsBackTaskRow exercises the
// Ensure-failure rollback: the project root is NOT a git repo, so
// Ensure errors with ErrNotGitRepo; create must `DeleteTask` the
// just-inserted row so the user doesn't see a half-formed task.
func TestWorktree_CLI_CreateIsolated_NonGitRollsBackTaskRow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Isolate $HOME but DO NOT git init the project dir.
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("autosk init: %v", err)
	}
	withIsolatedPackagesPrefix(t)
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("agent install: %v", err)
	}
	wfPath := writeIsolatedWorkflow(t, dir, "iso-rollback")
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatalf("workflow create: %v", err)
	}

	// Pre-count tasks in the project DB.
	preList, err := runRoot(t, dir, "list", "--status", "all", "--json")
	if err != nil {
		t.Fatalf("list pre: %v\n%s", err, preList)
	}
	var preRows []map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(preList)), &preRows)

	// Create must fail.
	createOut, err := runRoot(t, dir, "create", "fails on Ensure", "--workflow", "iso-rollback")
	if err == nil {
		t.Fatalf("expected create to fail on non-git project; got: %s", createOut)
	}
	if !strings.Contains(err.Error(), "worktree") && !strings.Contains(err.Error(), "not a git repo") {
		t.Errorf("error should mention worktree / not a git repo: %v", err)
	}

	// Post-count must match \u2014 the rollback DeleteTask reaped the row.
	postList, err := runRoot(t, dir, "list", "--status", "all", "--json")
	if err != nil {
		t.Fatalf("list post: %v\n%s", err, postList)
	}
	var postRows []map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(postList)), &postRows)
	if len(postRows) != len(preRows) {
		t.Fatalf("rollback failed: %d tasks before, %d after\nlist=%s",
			len(preRows), len(postRows), postList)
	}
}

// ----------------------------------------------------------------------
// WT3 \u2014 `autosk enroll --base-ref`.
// ----------------------------------------------------------------------

func TestWorktree_CLI_EnrollWithBaseRef_HonouredOnFreshBranch(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-enroll")

	// Create a second branch (`feature/seed`) with a fresh commit so we
	// have a non-main ref to enroll against.
	runGitOrFail(t, root, "checkout", "-b", "feature/seed")
	runGitOrFail(t, root, "commit", "--allow-empty", "-m", "seed-only")
	seedSHA := gitBranchSHA(t, root, "feature/seed")
	runGitOrFail(t, root, "checkout", "main")

	id := createBareTask(t, root, "enroll w/ base")

	if out, err := runRoot(t, root, "enroll", id, "--workflow", "iso-enroll", "--base-ref", "feature/seed"); err != nil {
		t.Fatalf("enroll --base-ref: %v\n%s", err, out)
	}
	branchSHA := gitBranchSHA(t, root, "autosk/"+id)
	if branchSHA != seedSHA {
		t.Errorf("--base-ref ignored: branch=%s base=%s", branchSHA, seedSHA)
	}
}

func TestWorktree_CLI_EnrollWithBaseRef_BranchExistsWarning(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-warn")

	id := createBareTask(t, root, "branch exists scenario")
	// Pre-create the branch so enroll's --base-ref must be ignored.
	if _, err := runRoot(t, root, "enroll", id, "--workflow", "iso-warn"); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	// Tear the worktree dir down via the engine: cancel \u2192 cleanup
	// runs \u2192 dir gone, branch survives. Then reopen \u2192 enroll
	// with --base-ref so we hit the branch-exists path.
	if _, err := runRoot(t, root, "cancel", id); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, root, "reopen", id); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, root, "enroll", id, "--workflow", "iso-warn", "--base-ref", "main")
	if err != nil {
		t.Fatalf("re-enroll: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--base-ref ignored") {
		t.Errorf("expected --base-ref ignored warning when branch already existed, got:\n%s", out)
	}
}

func TestWorktree_CLI_EnrollBaseRef_RequiresIsolatedWorkflow(t *testing.T) {
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeStandardWorkflow(t, root, "non-iso")
	if _, err := runRoot(t, root, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	id := createBareTask(t, root, "base ref on non-iso")
	_, err := runRoot(t, root, "enroll", id, "--workflow", "non-iso", "--base-ref", "main")
	if err == nil {
		t.Fatal("expected --base-ref rejection on non-isolated workflow")
	}
	if !strings.Contains(err.Error(), "--base-ref") || !strings.Contains(err.Error(), "isolation=worktree") {
		t.Errorf("error should explain --base-ref requires isolation=worktree, got: %v", err)
	}
}

// ----------------------------------------------------------------------
// WT3 \u2014 `autosk done` / `autosk cancel` reap the worktree.
// ----------------------------------------------------------------------

func TestWorktree_CLI_Done_RemovesWorktreeKeepsBranch(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-done")

	out, err := runRoot(t, root, "create", "done me", "--workflow", "iso-done")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	id := createIDFromOutput(out)
	wtPath, _ := worktree.PathFor(root, id)
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree dir should exist pre-done: %v", err)
	}

	if _, err := runRoot(t, root, "done", id); err != nil {
		t.Fatalf("done: %v", err)
	}
	if _, err := os.Stat(wtPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree dir should be removed after done; stat err=%v", err)
	}
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Fatalf("branch autosk/%s should survive done", id)
	}
}

func TestWorktree_CLI_Cancel_RemovesWorktreeKeepsBranch(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-cancel")

	out, err := runRoot(t, root, "create", "cancel me", "--workflow", "iso-cancel")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	id := createIDFromOutput(out)
	wtPath, _ := worktree.PathFor(root, id)

	if _, err := runRoot(t, root, "cancel", id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(wtPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree dir should be removed after cancel; stat err=%v", err)
	}
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Fatalf("branch autosk/%s should survive cancel", id)
	}
}

// ----------------------------------------------------------------------
// WT3 \u2014 `autosk show` surfaces the worktree block.
// ----------------------------------------------------------------------

func TestWorktree_CLI_ShowText_IncludesWorktreeBlock(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-show")

	out, err := runRoot(t, root, "create", "show me", "--workflow", "iso-show")
	if err != nil {
		t.Fatal(err)
	}
	id := createIDFromOutput(out)

	showOut, err := runRoot(t, root, "show", id)
	if err != nil {
		t.Fatalf("show: %v\n%s", err, showOut)
	}
	if !strings.Contains(showOut, "worktree:") {
		t.Errorf("expected worktree: line in text show, got:\n%s", showOut)
	}
	if !strings.Contains(showOut, "branch:") || !strings.Contains(showOut, "autosk/"+id) {
		t.Errorf("expected branch: line referencing autosk/%s, got:\n%s", id, showOut)
	}
	if !strings.Contains(showOut, "(exists)") {
		t.Errorf("expected (exists) flag in worktree line, got:\n%s", showOut)
	}
}

func TestWorktree_CLI_ShowJSON_IncludesWorktreeBlock(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-show-json")

	out, err := runRoot(t, root, "create", "show me json", "--workflow", "iso-show-json")
	if err != nil {
		t.Fatal(err)
	}
	id := createIDFromOutput(out)
	wtPath, _ := worktree.PathFor(root, id)

	showOut, err := runRoot(t, root, "show", id, "--json")
	if err != nil {
		t.Fatalf("show --json: %v\n%s", err, showOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(showOut)), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, showOut)
	}
	wt, ok := got["worktree"].(map[string]any)
	if !ok {
		t.Fatalf("expected worktree block in JSON; got: %s", showOut)
	}
	if wt["path"] != wtPath {
		t.Errorf("worktree.path mismatch: got %v want %s", wt["path"], wtPath)
	}
	if wt["branch"] != "autosk/"+id {
		t.Errorf("worktree.branch: got %v", wt["branch"])
	}
	if wt["exists"] != true {
		t.Errorf("worktree.exists: got %v", wt["exists"])
	}
}

// TestWorktree_CLI_ShowJSON_OmitsBlockOnNonIsolated guards the
// no-regression invariant: tasks whose workflow has isolation=none
// (or no workflow at all) must NOT see a worktree key. Otherwise
// every existing JSON consumer breaks.
func TestWorktree_CLI_ShowJSON_OmitsBlockOnNonIsolated(t *testing.T) {
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeStandardWorkflow(t, root, "non-iso-show")
	if _, err := runRoot(t, root, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, root, "create", "plain", "--workflow", "non-iso-show")
	if err != nil {
		t.Fatal(err)
	}
	id := createIDFromOutput(out)

	showOut, err := runRoot(t, root, "show", id, "--json")
	if err != nil {
		t.Fatalf("show --json: %v\n%s", err, showOut)
	}
	if strings.Contains(showOut, `"worktree"`) {
		t.Errorf("non-isolated task must omit worktree key, got:\n%s", showOut)
	}
}

// ----------------------------------------------------------------------
// WT4 \u2014 `autosk worktree {list,path,rm}`.
// ----------------------------------------------------------------------

func TestWorktree_CLI_Path_DeterministicNoStat(t *testing.T) {
	root := makeIsolatedProject(t)

	// We do NOT need to create the task in the DB: `worktree path` is a
	// pure helper. Pass any well-formed id.
	want, err := worktree.PathFor(root, "as-xxxx")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, root, "worktree", "path", "as-xxxx")
	if err != nil {
		t.Fatalf("worktree path: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != want {
		t.Errorf("worktree path: got %q want %q", strings.TrimSpace(out), want)
	}
	// Even though the directory doesn't exist, the command must succeed
	// (the verb's contract is \"derive, don't stat\").
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test setup: dir should NOT exist before `worktree path`; got %v", err)
	}
}

func TestWorktree_CLI_PathJSON_Shape(t *testing.T) {
	root := makeIsolatedProject(t)
	wantPath, _ := worktree.PathFor(root, "as-jsonpath")

	out, err := runRoot(t, root, "worktree", "path", "as-jsonpath", "--json")
	if err != nil {
		t.Fatalf("worktree path --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got["task_id"] != "as-jsonpath" {
		t.Errorf("task_id: %v", got["task_id"])
	}
	if got["path"] != wantPath {
		t.Errorf("path: got %v want %s", got["path"], wantPath)
	}
	if got["branch"] != "autosk/as-jsonpath" {
		t.Errorf("branch: %v", got["branch"])
	}
}

func TestWorktree_CLI_List_MixedWorkflows(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-list")
	// Also create a NON-isolated workflow + task. Its task must NOT
	// appear in worktree list.
	wfPath := writeStandardWorkflow(t, root, "std-list")
	if _, err := runRoot(t, root, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	isoOut, err := runRoot(t, root, "create", "iso task", "--workflow", "iso-list")
	if err != nil {
		t.Fatal(err)
	}
	isoID := createIDFromOutput(isoOut)
	stdOut, err := runRoot(t, root, "create", "std task", "--workflow", "std-list")
	if err != nil {
		t.Fatal(err)
	}
	stdID := createIDFromOutput(stdOut)

	// Also close the iso task so we exercise the \"closed task with
	// surviving dir\" branch: after done, dir gone, branch alive, but
	// the task should still appear in `worktree list` (with
	// exists=false).
	doneOut, err := runRoot(t, root, "create", "closed iso", "--workflow", "iso-list")
	if err != nil {
		t.Fatal(err)
	}
	doneID := createIDFromOutput(doneOut)
	if _, err := runRoot(t, root, "done", doneID); err != nil {
		t.Fatal(err)
	}

	listJSON, err := runRoot(t, root, "worktree", "list", "--json")
	if err != nil {
		t.Fatalf("worktree list --json: %v\n%s", err, listJSON)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(listJSON)), &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, listJSON)
	}
	seen := map[string]map[string]any{}
	for _, r := range rows {
		if id, _ := r["task_id"].(string); id != "" {
			seen[id] = r
		}
	}
	if _, ok := seen[stdID]; ok {
		t.Errorf("non-isolated task %s must NOT appear in worktree list", stdID)
	}
	if _, ok := seen[isoID]; !ok {
		t.Errorf("isolated task %s missing from worktree list:\n%s", isoID, listJSON)
	}
	closed, ok := seen[doneID]
	if !ok {
		t.Fatalf("closed isolated task %s missing from worktree list:\n%s", doneID, listJSON)
	}
	if closed["exists"] != false {
		t.Errorf("closed isolated task should have exists=false, got %v", closed["exists"])
	}
	if closed["status"] != "done" {
		t.Errorf("closed task status: %v", closed["status"])
	}
	// Required JSON keys are present on every row.
	for _, k := range []string{"task_id", "status", "workflow", "path", "branch", "exists", "project_root"} {
		if _, ok := seen[isoID][k]; !ok {
			t.Errorf("row missing key %q: %v", k, seen[isoID])
		}
	}
	// Text mode renders something non-empty.
	listTxt, err := runRoot(t, root, "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list (txt): %v\n%s", err, listTxt)
	}
	if !strings.Contains(listTxt, isoID) {
		t.Errorf("text list missing %s:\n%s", isoID, listTxt)
	}
}

func TestWorktree_CLI_Rm_Terminal_RemovesDirKeepsBranch(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-rm")

	out, err := runRoot(t, root, "create", "rm me", "--workflow", "iso-rm")
	if err != nil {
		t.Fatal(err)
	}
	id := createIDFromOutput(out)
	wtPath, _ := worktree.PathFor(root, id)

	// Close the task; the engine reaps the dir on done. The test then
	// re-allocates a fresh worktree out-of-band so `worktree rm` has
	// something to reap on its own. (Mirrors the orphan-cleanup case
	// the verb exists for.)
	if _, err := runRoot(t, root, "done", id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test setup: engine should have reaped the dir on done; got %v", err)
	}
	// Re-create the worktree dir via the package helper.
	mgr := worktree.NewManager()
	if _, err := mgr.Ensure(t.Context(), root, id, ""); err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("re-Ensure should have created the dir: %v", err)
	}

	rmOut, err := runRoot(t, root, "worktree", "rm", id)
	if err != nil {
		t.Fatalf("worktree rm: %v\n%s", err, rmOut)
	}
	if _, err := os.Stat(wtPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree rm did not reap dir: %v", err)
	}
	if !gitBranchExists(t, root, "autosk/"+id) {
		t.Errorf("branch must survive `worktree rm`")
	}
	if !strings.Contains(rmOut, "removed:") {
		t.Errorf("expected 'removed:' in human output: %s", rmOut)
	}
}

func TestWorktree_CLI_Rm_OnHumanFeedback_Permitted(t *testing.T) {
	// Documented recovery flow for worktree_stranded (and the legacy
	// worktree_missing flow that the daemon now auto-recovers, but
	// which an operator can still invoke manually): run parks \u2192
	// `autosk worktree rm <id>` \u2192 cancel \u2192 reopen \u2192 enroll.
	// `worktree rm` must therefore NOT refuse `human` tasks.
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-park")

	out, err := runRoot(t, root, "create", "park me", "--workflow", "iso-park")
	if err != nil {
		t.Fatal(err)
	}
	id := createIDFromOutput(out)
	// Park the task via raw SQL (same trick as enroll's human
	// test) \u2014 keep current_step_id intact so the CHECK passes.
	q := fmt.Sprintf("UPDATE tasks SET status='human' WHERE id='%s'", id)
	if _, err := runRoot(t, root, "sql", "--write", q); err != nil {
		t.Fatalf("force human: %v", err)
	}

	rmOut, err := runRoot(t, root, "worktree", "rm", id)
	if err != nil {
		t.Fatalf("`worktree rm` must accept human tasks (plan \u00a78.3 recovery): %v\n%s", err, rmOut)
	}
}

func TestWorktree_CLI_Rm_OnInWorkflow_Refused(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-live")

	out, err := runRoot(t, root, "create", "live one", "--workflow", "iso-live")
	if err != nil {
		t.Fatal(err)
	}
	id := createIDFromOutput(out)
	// Task is now work. `worktree rm` must refuse.
	_, err = runRoot(t, root, "worktree", "rm", id)
	if err == nil {
		t.Fatal("expected `worktree rm` to refuse a work task")
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("error should mention work, got: %v", err)
	}
}

// ----------------------------------------------------------------------
// Enroll EnterStep-failure rollback for isolated workflows.
//
// Mirror of TestWorktree_CLI_CreateIsolated_NonGitRollsBackTaskRow but
// for the enroll path: when Ensure has already allocated the worktree
// and EnterStep then fails, the worktree must be reaped so the user
// doesn't end up with a leak that `autosk cancel` won't clean up (the
// cleanup-on-terminal guard bails on empty workflow_id).
//
// We provoke EnterStep failure deterministically by pre-bumping the
// entry step's step_visits counter to the cap via `metadata set`. The
// EnterStep call then trips MaxVisitsExceededError, which is exactly
// the failure mode the cap-rollback path was designed for.
// ----------------------------------------------------------------------

// writeCappedIsolatedWorkflow builds a single-step isolated workflow
// named `name` with the entry step capped at max_visits=1 so the
// SECOND enroll attempt against the same step trips EnterStep.
func writeCappedIsolatedWorkflow(t *testing.T, dir, name string) string {
	t.Helper()
	wf := map[string]any{
		"name":       name,
		"first_step": "do",
		"isolation":  "worktree",
		"steps": map[string]any{
			"do": map[string]any{
				"agent":      map[string]any{"name": "@autosk/dev-fixture"},
				"max_visits": 1,
				"next_steps": []any{
					map[string]any{"task_status": "done", "prompt_rule": "ship"},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// stepIDForIsolatedWF looks up the named step's id via `workflow show
// --json` so the test stays resilient to id format drift.
func stepIDForIsolatedWF(t *testing.T, dir, wfName, stepName string) string {
	t.Helper()
	out, err := runRoot(t, dir, "workflow", "show", wfName, "--json")
	if err != nil {
		t.Fatalf("workflow show: %v", err)
	}
	var wfShow map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &wfShow); err != nil {
		t.Fatalf("unmarshal wf show: %v\n%s", err, out)
	}
	steps, _ := wfShow["steps"].([]any)
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		if sm["name"] == stepName {
			id, _ := sm["id"].(string)
			return id
		}
	}
	t.Fatalf("step %q not found in workflow %q", stepName, wfName)
	return ""
}

func TestWorktree_CLI_EnrollEnterStepFailure_RollsBackWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeCappedIsolatedWorkflow(t, root, "iso-rollback-enroll")
	if _, err := runRoot(t, root, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	id := createBareTask(t, root, "enroll cap rollback")
	stepID := stepIDForIsolatedWF(t, root, "iso-rollback-enroll", "do")

	// Pre-bump step_visits[do] to the cap so EnterStep on the first
	// enroll attempt fires MaxVisitsExceededError. This is the
	// deterministic way to provoke the failure mode the rollback path
	// guards against.
	if _, err := runRoot(t, root, "metadata", "set", id,
		"--key", "step_visits."+stepID, "--value", "1"); err != nil {
		t.Fatalf("metadata set: %v", err)
	}

	// Enroll must fail: cap exceeded.
	_, err := runRoot(t, root, "enroll", id, "--workflow", "iso-rollback-enroll")
	if err == nil {
		t.Fatal("expected enroll to fail with cap exceeded")
	}

	// Worktree dir + branch must NOT survive the failed enroll: the
	// rollback path runs OnTerminal before returning the error.
	wtPath, _ := worktree.PathFor(root, id)
	if _, statErr := os.Stat(wtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("orphan worktree leak: dir should be gone after failed enroll, got %v", statErr)
	}
	// Task row must still exist (we didn't DeleteTask) and remain new.
	got := statusOf(t, root, id)
	if got["status"] != "new" {
		t.Errorf("task should remain `new` after failed enroll, got: %v", got["status"])
	}
}

// ----------------------------------------------------------------------
// Workflow show --json must always carry the isolation field.
// ----------------------------------------------------------------------

func TestWorkflow_ShowJSON_IsolationField_Worktree(t *testing.T) {
	root := makeIsolatedProject(t)
	installFixturesAndIsolatedWF(t, root, "iso-show-json")

	out, err := runRoot(t, root, "workflow", "show", "iso-show-json", "--json")
	if err != nil {
		t.Fatalf("workflow show --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	iso, ok := got["isolation"]
	if !ok {
		t.Fatalf("isolation key missing from --json shape:\n%s", out)
	}
	if iso != "worktree" {
		t.Errorf("isolation: got %v want \"worktree\"", iso)
	}
}

func TestWorkflow_ShowJSON_IsolationField_NoneAlwaysPresent(t *testing.T) {
	// The field is always present (no omitempty), even for non-isolated
	// workflows. Downstream consumers therefore see the same shape
	// regardless of opt-in, and a regression that flips omitempty back
	// would trip this test immediately.
	root := makeIsolatedProject(t)
	if _, err := runRoot(t, root, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	wfPath := writeStandardWorkflow(t, root, "non-iso-show-json")
	if _, err := runRoot(t, root, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, root, "workflow", "show", "non-iso-show-json", "--json")
	if err != nil {
		t.Fatalf("workflow show --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	iso, ok := got["isolation"]
	if !ok {
		t.Fatalf("isolation key must be present (no omitempty) for non-isolated workflows; --json:\n%s", out)
	}
	if iso != "none" {
		t.Errorf("isolation: got %v want \"none\"", iso)
	}
}
