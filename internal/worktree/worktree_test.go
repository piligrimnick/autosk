package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"autosk/internal/worktree"
)

// gitProject initialises a fresh git repo under t.TempDir() with one
// commit so HEAD resolves. Returns the absolute project root. Skips
// the test if `git` isn't available.
func gitProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; skipping worktree tests")
	}
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "--initial-branch=main")
	mustRun(t, dir, "git", "config", "user.email", "test@autosk.local")
	mustRun(t, dir, "git", "config", "user.name", "autosk test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRun(t, dir, "git", "add", "README.md")
	mustRun(t, dir, "git", "commit", "-m", "init")
	// Resolve symlinks so test assertions match canonRoot's view.
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canon = dir
	}
	return canon
}

func mustRun(t *testing.T, cwd, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// isolateHome points $HOME (and HOMEDRIVE / HOMEPATH on Windows) at a
// temp dir for the duration of the test so PathFor's derivation lands
// somewhere t.Cleanup will sweep.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestPathFor_DeterministicSlug(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		canon = root
	}
	a, err := worktree.PathFor(canon, "as-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := worktree.PathFor(canon, "as-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("PathFor not deterministic: %s vs %s", a, b)
	}
	if !strings.Contains(a, ".autosk/worktrees/") {
		t.Fatalf("path does not live under ~/.autosk/worktrees: %s", a)
	}
	if !strings.HasSuffix(a, "/as-aaaa") {
		t.Fatalf("path does not end with task id: %s", a)
	}
}

func TestPathFor_DifferentRootsDifferentSlugs(t *testing.T) {
	isolateHome(t)
	root1 := t.TempDir()
	root2 := t.TempDir()
	a, _ := worktree.PathFor(root1, "as-1111")
	b, _ := worktree.PathFor(root2, "as-1111")
	if a == b {
		t.Fatalf("expected different slugs for distinct roots: %s == %s", a, b)
	}
}

func TestBranchFor(t *testing.T) {
	if got := worktree.BranchFor("as-bea9"); got != "autosk/as-bea9" {
		t.Fatalf("BranchFor: %q", got)
	}
}

func TestEnsure_CreatesBranchAndWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	res, err := mgr.Ensure(ctx, root, "as-0001", "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Existing || res.BaseRefIgnored {
		t.Fatalf("unexpected flags on fresh Ensure: %+v", res)
	}
	if res.Path == "" || res.Branch != "autosk/as-0001" {
		t.Fatalf("Result missing path/branch: %+v", res)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("worktree directory not created: %v", err)
	}
	// Branch should now exist.
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/heads/autosk/as-0001").CombinedOutput()
	if err != nil {
		t.Fatalf("expected branch to exist: %v: %s", err, out)
	}
}

func TestEnsure_IdempotentSecondCall(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "as-0002", ""); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	res2, err := mgr.Ensure(ctx, root, "as-0002", "")
	if err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if !res2.Existing {
		t.Fatalf("second Ensure should report Existing=true: %+v", res2)
	}
}

func TestEnsure_ReusesExistingBranch_BaseRefIgnored(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Allocate, then remove the worktree directory (mimicking
	// `autosk done` cleanup) so the branch survives.
	if _, err := mgr.Ensure(ctx, root, "as-0003", ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := mgr.OnTerminal(ctx, root, "as-0003"); err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	// Re-ensure with an explicit base-ref; we should get a warning back
	// and the existing branch should be reused.
	res, err := mgr.Ensure(ctx, root, "as-0003", "main")
	if err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if !res.BaseRefIgnored {
		t.Fatalf("expected BaseRefIgnored=true on re-use, got %+v", res)
	}
	if res.Existing {
		t.Fatalf("expected Existing=false after directory removal, got %+v", res)
	}
}

func TestEnsure_NonGitRepo(t *testing.T) {
	isolateHome(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	dir := t.TempDir()
	_, err := mgr.Ensure(ctx, dir, "as-9999", "")
	if !errors.Is(err, worktree.ErrNotGitRepo) {
		t.Fatalf("want ErrNotGitRepo, got %v", err)
	}
}

func TestEnsure_BaseRefHonouredOnFreshBranch(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	// Create a second branch off main and use it as base.
	mustRun(t, root, "git", "checkout", "-b", "feature/seed")
	mustRun(t, root, "git", "commit", "--allow-empty", "-m", "seed-only")
	seedSHA := strings.TrimSpace(mustOutput(t, root, "git", "rev-parse", "HEAD"))
	mustRun(t, root, "git", "checkout", "main")

	mgr := worktree.NewManager()
	if _, err := mgr.Ensure(context.Background(), root, "as-0004", "feature/seed"); err != nil {
		t.Fatalf("Ensure with base: %v", err)
	}
	// The new branch tip should equal feature/seed's tip.
	got := strings.TrimSpace(mustOutput(t, root, "git", "rev-parse", "refs/heads/autosk/as-0004"))
	if got != seedSHA {
		t.Fatalf("new branch did not start at base-ref: branch=%s base=%s", got, seedSHA)
	}
}

func TestOnTerminal_Idempotent(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "as-0005", ""); err != nil {
		t.Fatal(err)
	}
	r1, err := mgr.OnTerminal(ctx, root, "as-0005")
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Existed {
		t.Fatalf("first OnTerminal should report Existed=true, got %+v", r1)
	}
	if _, err := os.Stat(r1.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected path removed, stat err=%v", err)
	}
	r2, err := mgr.OnTerminal(ctx, root, "as-0005")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Existed {
		t.Fatalf("second OnTerminal should be a no-op, got %+v", r2)
	}
	// Branch survives.
	if _, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/heads/autosk/as-0005").CombinedOutput(); err != nil {
		t.Fatalf("branch should survive OnTerminal, lookup err=%v", err)
	}
}

func TestVerify_OK(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "as-0006", ""); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Verify(ctx, root, "as-0006"); err != nil {
		t.Fatalf("Verify after Ensure should succeed: %v", err)
	}
}

func TestVerify_MissingDirectory(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "as-0007", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := worktree.PathFor(root, "as-0007")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	err := mgr.Verify(ctx, root, "as-0007")
	if !errors.Is(err, worktree.ErrWorktreeMissing) {
		t.Fatalf("want ErrWorktreeMissing, got %v", err)
	}
}

func TestEnsure_PerTaskMutex_Serialises(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Race N concurrent Ensure on the SAME task. Exactly one must
	// observe Existing=false (the real allocator); the rest must see
	// Existing=true. All N must succeed.
	const N = 4
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		fresh    int
		existing int
		errs     []error
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := mgr.Ensure(ctx, root, "as-0010", "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if res.Existing {
				existing++
			} else {
				fresh++
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("racing Ensure returned errors: %v", errs)
	}
	if fresh != 1 || existing != N-1 {
		t.Fatalf("expected exactly one fresh allocator, got fresh=%d existing=%d", fresh, existing)
	}
}

// TestOnTerminal_RemovesDirEvenWhenGitBroken asserts that the
// directory is still reaped when the project's git state has been
// nuked (operator re-init'd or rm -rf'd .git). Without this, both
// the executor's cleanup hook and `autosk worktree rm` would leak
// the dir forever — the branch is the only thing requiring a
// working git and OnTerminal never touches it.
func TestOnTerminal_RemovesDirEvenWhenGitBroken(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "as-0020", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := worktree.PathFor(root, "as-0020")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-condition: worktree dir should exist: %v", err)
	}

	// Nuke .git — verifyGitRepo will now fail.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}

	res, err := mgr.OnTerminal(ctx, root, "as-0020")
	if err != nil {
		t.Fatalf("OnTerminal should succeed best-effort when git is broken: %v", err)
	}
	if !res.Existed {
		t.Errorf("expected Existed=true (we did reap the dir), got %+v", res)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree dir should be removed even though git is broken; stat err=%v", err)
	}
}

// TestOnTerminal_NeverAllocated_GitBroken_ReportsExistedFalse asserts
// that the git-broken recovery branch only sets Result.Existed=true
// when it actually reaped a directory. Without this guard, a stat-of
// -a-nonexistent path returns nil + ErrNotExist, the (now removed)
// pathWasRemoved helper used to return true, and Existed was reported
// as true even though nothing happened. CLI rendering then printed
// "removed: <path>" on a no-op, which is a real correctness lie in
// `autosk worktree rm` on the §8.3 recovery path.
func TestOnTerminal_NeverAllocated_GitBroken_ReportsExistedFalse(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Nuke .git so verifyGitRepo fails immediately.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}

	// Task was never allocated; OnTerminal must report Existed=false.
	res, err := mgr.OnTerminal(ctx, root, "as-0022")
	if err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	if res.Existed {
		t.Errorf("Existed=true reported for path that was never allocated (git broken): %+v", res)
	}
	// Path should remain absent (we never created it, and the recovery
	// branch must not have side-effects on missing paths).
	if _, err := os.Stat(res.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path should remain absent, stat err=%v", err)
	}
}

// TestVerify_StatErrorMapsToStranded asserts that any stat error other
// than ErrNotExist (e.g. EACCES on the parent directory) surfaces as
// ErrWorktreeStranded so the executor doesn't mislabel the run
// "worktree_missing" when the directory is right there.
func TestVerify_StatErrorMapsToStranded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 is ineffective")
	}
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "as-0021", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := worktree.PathFor(root, "as-0021")
	parent := filepath.Dir(path)
	// Drop search bit on the parent so stat(path) returns EACCES.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	err := mgr.Verify(ctx, root, "as-0021")
	if err == nil {
		t.Fatal("expected Verify to error on EACCES")
	}
	if errors.Is(err, worktree.ErrWorktreeMissing) {
		t.Errorf("stat-EACCES must NOT map to ErrWorktreeMissing: %v", err)
	}
	if !errors.Is(err, worktree.ErrWorktreeStranded) {
		t.Errorf("want ErrWorktreeStranded, got %v", err)
	}
}

func TestEnsure_PathOccupied(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Pre-create the target path with some unrelated content.
	path, _ := worktree.PathFor(root, "as-0011")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "loitering.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := mgr.Ensure(ctx, root, "as-0011", "")
	if !errors.Is(err, worktree.ErrPathOccupied) {
		t.Fatalf("want ErrPathOccupied, got %v", err)
	}
}

func mustOutput(t *testing.T, cwd, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
