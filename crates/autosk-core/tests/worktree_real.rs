//! Real git-backed worktree test (acceptance #4): the worktree path/branch are
//! byte-identical to the Go derivation, `Ensure` allocates the dir on a fresh
//! `autosk/<task>` branch, and `OnTerminal` removes the dir while PRESERVING
//! the branch. The slug's SHA-256 is cross-checked against the system
//! `sha256sum`/`shasum` so the dependency-free hash in `worktree.rs` can't drift.

use std::path::Path;
use std::process::Command;

use autosk_core::worktree::{branch_for, path_for, Manager, WorktreeManager};

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
    let home = tempfile::tempdir().unwrap();
    // Hermetic HOME so the worktree lands under the temp tree (path_for reads
    // $HOME). This test file is its own binary, so the set is not racing other
    // tests' HOME reads.
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
    let res = mgr.ensure(&root_s, task, "").expect("ensure");
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

    // ---- OnTerminal removes dir, PRESERVES branch ----
    let out = mgr.on_terminal(&root_s, task).expect("on_terminal");
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
