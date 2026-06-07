//! Real git-backed worktree test (acceptance #4): the worktree path/branch are
//! byte-identical to the Go derivation, `Ensure` allocates the dir on a fresh
//! `autosk/<task>` branch, and `OnTerminal` removes the dir while PRESERVING
//! the branch. The slug's SHA-256 is cross-checked against the system
//! `sha256sum`/`shasum` so the dependency-free hash in `worktree.rs` can't drift.

use std::path::Path;
use std::process::Command;

use autosk_core::ctx::Ctx;
use autosk_core::worktree::{branch_for, path_for, Manager, WorktreeError, WorktreeManager};

/// Serialises the tests in this binary: each mutates the process-global `$HOME`
/// (which `path_for` reads), so they must not run concurrently. `into_inner`
/// keeps a panic in one test from poisoning the lock for the rest.
static HOME_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

fn lock_home() -> std::sync::MutexGuard<'static, ()> {
    HOME_LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

fn git(args: &[&str], cwd: &Path) {
    let st = Command::new("git")
        .args(args)
        .current_dir(cwd)
        .env("GIT_AUTHOR_NAME", "t")
        .env("GIT_AUTHOR_EMAIL", "t@e")
        .env("GIT_COMMITTER_NAME", "t")
        .env("GIT_COMMITTER_EMAIL", "t@e")
        .output()
        .expect("git");
    assert!(
        st.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&st.stderr)
    );
}

fn git_ok(args: &[&str], cwd: &Path) -> bool {
    Command::new("git")
        .args(args)
        .current_dir(cwd)
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

/// SHA-256 of `s` via the system tool (sha256sum or shasum -a 256), first 8 hex.
fn system_sha256_8(s: &str) -> Option<String> {
    for (bin, args) in [("sha256sum", vec![]), ("shasum", vec!["-a", "256"])] {
        use std::io::Write;
        let mut child = match Command::new(bin)
            .args(&args)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .spawn()
        {
            Ok(c) => c,
            Err(_) => continue,
        };
        child.stdin.take().unwrap().write_all(s.as_bytes()).unwrap();
        let out = child.wait_with_output().unwrap();
        if out.status.success() {
            let hex = String::from_utf8_lossy(&out.stdout);
            let hex = hex.split_whitespace().next().unwrap_or("");
            if hex.len() >= 8 {
                return Some(hex[..8].to_string());
            }
        }
    }
    None
}

#[test]
fn ensure_then_terminal_preserves_branch_and_path_matches_go() {
    if Command::new("git").arg("--version").output().is_err() {
        eprintln!("skip: git not available");
        return;
    }
    let _home_lock = lock_home();
    let home = tempfile::tempdir().unwrap();
    // Hermetic HOME so the worktree lands under the temp tree (path_for reads
    // $HOME). The HOME_LOCK serialises the HOME-mutating tests in this binary.
    std::env::set_var("HOME", home.path());

    let proj = tempfile::tempdir().unwrap();
    let root = proj.path();
    git(&["init", "-q"], root);
    std::fs::write(root.join("README"), "x").unwrap();
    git(&["add", "."], root);
    git(&["commit", "-q", "-m", "init"], root);

    let root_s = root.to_string_lossy().to_string();
    let task = "ask-deadbe";

    // ---- byte-identical path derivation ----
    let canon = std::fs::canonicalize(root)
        .unwrap()
        .to_string_lossy()
        .to_string();
    let derived = path_for(&root_s, task).unwrap();
    if let Some(hex8) = system_sha256_8(&canon) {
        let base = Path::new(&canon)
            .file_name()
            .unwrap()
            .to_string_lossy()
            .to_string();
        let expected = home
            .path()
            .join(".autosk")
            .join("worktrees")
            .join(format!("{base}-{hex8}"))
            .join(task)
            .to_string_lossy()
            .to_string();
        assert_eq!(
            derived, expected,
            "path_for must match the Go slug formula (basename-8hex(sha256(canonRoot)))"
        );
    }
    assert_eq!(branch_for(task), "autosk/ask-deadbe");

    // ---- Ensure allocates dir + branch ----
    let mgr = Manager::new();
    let res = mgr
        .ensure(&Ctx::background(), &root_s, task, "")
        .expect("ensure");
    assert_eq!(res.path, derived);
    assert!(Path::new(&derived).is_dir(), "worktree dir created");
    assert!(
        git_ok(
            &[
                "show-ref",
                "--verify",
                "--quiet",
                "refs/heads/autosk/ask-deadbe"
            ],
            root
        ),
        "branch created"
    );

    // ---- verify() classifies a healthy worktree as Ok ----
    mgr.verify(&Ctx::background(), &root_s, task)
        .expect("verify Ok on a freshly-ensured worktree");

    // ---- OnTerminal removes dir, PRESERVES branch ----
    let out = mgr
        .on_terminal(&Ctx::background(), &root_s, task)
        .expect("on_terminal");
    assert!(out.existed);
    assert!(
        !Path::new(&derived).exists(),
        "worktree dir removed on terminal"
    );
    assert!(
        git_ok(
            &[
                "show-ref",
                "--verify",
                "--quiet",
                "refs/heads/autosk/ask-deadbe"
            ],
            root
        ),
        "branch PRESERVED after terminal cleanup"
    );
}

/// `verify()` error-classification — the executor's auto-recovery-vs-park
/// decision hinges on it: a vanished dir is WorktreeMissing (auto-recoverable),
/// a dir whose gitdir no longer matches the project is WorktreeStranded (park),
/// and a non-repo project root is NotGitRepo.
#[test]
fn verify_classifies_missing_and_stranded() {
    if Command::new("git").arg("--version").output().is_err() {
        eprintln!("skip: git not available");
        return;
    }
    let _home_lock = lock_home();
    let home = tempfile::tempdir().unwrap();
    std::env::set_var("HOME", home.path());
    let proj = tempfile::tempdir().unwrap();
    let root = proj.path();
    git(&["init", "-q"], root);
    std::fs::write(root.join("README"), "x").unwrap();
    git(&["add", "."], root);
    git(&["commit", "-q", "-m", "init"], root);
    let root_s = root.to_string_lossy().to_string();
    let mgr = Manager::new();
    let ctx = Ctx::background();

    // (a) Never-allocated → the dir is absent → WorktreeMissing.
    let task = "ask-aaa111";
    match mgr.verify(&ctx, &root_s, task) {
        Err(WorktreeError::WorktreeMissing(_)) => {}
        other => panic!("want WorktreeMissing for an absent dir, got {other:?}"),
    }

    // Allocate, then `rm -rf` the dir out from under git (orphaned registration)
    // → still WorktreeMissing (the path itself is gone).
    let res = mgr.ensure(&ctx, &root_s, task, "").expect("ensure");
    std::fs::remove_dir_all(&res.path).unwrap();
    match mgr.verify(&ctx, &root_s, task) {
        Err(WorktreeError::WorktreeMissing(_)) => {}
        other => panic!("want WorktreeMissing after rm -rf, got {other:?}"),
    }

    // (b) A dir present at the path but NOT a checkout of this repo → Stranded.
    let task2 = "ask-bbb222";
    let path2 = path_for(&root_s, task2).unwrap();
    std::fs::create_dir_all(&path2).unwrap();
    // Make it its own unrelated git repo so git_common_dir resolves but differs.
    let p2 = Path::new(&path2);
    git(&["init", "-q"], p2);
    match mgr.verify(&ctx, &root_s, task2) {
        Err(WorktreeError::WorktreeStranded(_)) => {}
        other => panic!("want WorktreeStranded for a foreign repo at the path, got {other:?}"),
    }
}

/// `ensure()` refuses to clobber an occupied path: a plain (non-worktree)
/// directory already sitting at the target path yields PathOccupied.
#[test]
fn ensure_path_occupied() {
    if Command::new("git").arg("--version").output().is_err() {
        eprintln!("skip: git not available");
        return;
    }
    let _home_lock = lock_home();
    let home = tempfile::tempdir().unwrap();
    std::env::set_var("HOME", home.path());
    let proj = tempfile::tempdir().unwrap();
    let root = proj.path();
    git(&["init", "-q"], root);
    std::fs::write(root.join("README"), "x").unwrap();
    git(&["add", "."], root);
    git(&["commit", "-q", "-m", "init"], root);
    let root_s = root.to_string_lossy().to_string();

    let task = "ask-ccc333";
    let path = path_for(&root_s, task).unwrap();
    // Pre-create a plain directory (not a registered worktree) at the path.
    std::fs::create_dir_all(&path).unwrap();
    std::fs::write(Path::new(&path).join("stray"), "x").unwrap();

    let mgr = Manager::new();
    match mgr.ensure(&Ctx::background(), &root_s, task, "") {
        Err(WorktreeError::PathOccupied(_)) => {}
        other => panic!("want PathOccupied for a pre-occupied path, got {other:?}"),
    }
}
