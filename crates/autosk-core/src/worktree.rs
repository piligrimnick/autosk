//! Per-task git worktree isolation — the Rust port of `internal/worktree`.
//!
//! [`path_for`] / [`branch_for`] encode the deterministic mapping
//! `(canonical projectRoot, taskID) → on-disk path + branch`. They MUST stay
//! byte-identical to the Go helpers so a worktree allocated by either stack
//! resolves to the same place:
//!
//! ```text
//! ~/.autosk/worktrees/<basename(canonRoot)>-<8hex(sha256(canonRoot))>/<task-id>
//! branch = autosk/<task-id>
//! ```
//!
//! [`Manager`] wraps the mutating verbs (`Ensure`/`OnTerminal`/`Verify`) and
//! owns the per-`(canonRoot, taskID)` lock that serialises racing callers.

use std::collections::HashMap;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::ctx::Ctx;

/// Errors callers test against (mirror of the Go sentinels). Implementations
/// may attach extra context to the message.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WorktreeError {
    NotGitRepo(String),
    GitMissing(String),
    PathOccupied(String),
    WorktreeMissing(String),
    WorktreeStranded(String),
    Other(String),
}

impl std::fmt::Display for WorktreeError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            WorktreeError::NotGitRepo(s) => write!(f, "worktree: not a git repo: {s}"),
            WorktreeError::GitMissing(s) => write!(f, "worktree: git binary not found: {s}"),
            WorktreeError::PathOccupied(s) => write!(f, "worktree: path occupied: {s}"),
            WorktreeError::WorktreeMissing(s) => write!(f, "worktree: directory missing: {s}"),
            WorktreeError::WorktreeStranded(s) => write!(f, "worktree: stranded: {s}"),
            WorktreeError::Other(s) => write!(f, "worktree: {s}"),
        }
    }
}

impl std::error::Error for WorktreeError {}

type WtResult<T> = std::result::Result<T, WorktreeError>;

/// Structured outcome of `ensure`/`on_terminal` (mirror of `worktree.Result`).
#[derive(Debug, Clone, Default)]
pub struct WtOutcome {
    pub path: String,
    pub branch: String,
    pub existing: bool,
    pub base_ref_ignored: bool,
    pub existed: bool,
}

/// The mutating worktree verbs the executor drives. The daemon shares one
/// [`Manager`] across per-project executors so racing calls on the same task
/// serialise. Tests substitute a fake (mirror of the Go `Manager` interface).
///
/// Every verb threads a [`Ctx`] so the underlying `git` invocations can be
/// interrupted on daemon shutdown / task cancellation (mirror of Go's
/// `exec.CommandContext`).
pub trait WorktreeManager: Send + Sync {
    fn ensure(
        &self,
        ctx: &Ctx,
        project_root: &str,
        task_id: &str,
        base_ref: &str,
    ) -> WtResult<WtOutcome>;
    fn on_terminal(&self, ctx: &Ctx, project_root: &str, task_id: &str) -> WtResult<WtOutcome>;
    fn verify(&self, ctx: &Ctx, project_root: &str, task_id: &str) -> WtResult<()>;
}

/// `BranchFor` — canonical branch name `autosk/<taskID>`.
pub fn branch_for(task_id: &str) -> String {
    format!("autosk/{task_id}")
}

/// `PathFor` — absolute on-disk path for the `(projectRoot, taskID)` pair.
/// Byte-identical to the Go `worktree.PathFor`.
pub fn path_for(project_root: &str, task_id: &str) -> WtResult<String> {
    if project_root.trim().is_empty() {
        return Err(WorktreeError::Other("project root is empty".into()));
    }
    if task_id.trim().is_empty() {
        return Err(WorktreeError::Other("task id is empty".into()));
    }
    let canon = canon_root(project_root)?;
    let home = home_dir().ok_or_else(|| WorktreeError::Other("user home dir not set".into()))?;
    Ok(home
        .join(".autosk")
        .join("worktrees")
        .join(slug_for(&canon))
        .join(task_id)
        .to_string_lossy()
        .to_string())
}

/// Default [`WorktreeManager`] shelling `git`, with per-task locking.
pub struct Manager {
    locks: Mutex<HashMap<String, Arc<Mutex<()>>>>,
}

impl Default for Manager {
    fn default() -> Self {
        Self::new()
    }
}

impl Manager {
    pub fn new() -> Manager {
        Manager {
            locks: Mutex::new(HashMap::new()),
        }
    }

    fn lock(&self, canon: &str, task_id: &str) -> Arc<Mutex<()>> {
        let key = format!("{canon}\x00{task_id}");
        let mut locks = self.locks.lock().unwrap();
        locks.entry(key).or_default().clone()
    }
}

impl WorktreeManager for Manager {
    fn ensure(
        &self,
        ctx: &Ctx,
        project_root: &str,
        task_id: &str,
        base_ref: &str,
    ) -> WtResult<WtOutcome> {
        let canon = canon_root(project_root)?;
        let lk = self.lock(&canon, task_id);
        let _g = lk.lock().unwrap();

        verify_git_available()?;
        verify_git_repo(ctx, &canon)?;
        let path = path_for(&canon, task_id)?;
        let branch = branch_for(task_id);
        let mut res = WtOutcome {
            path: path.clone(),
            branch: branch.clone(),
            ..Default::default()
        };

        if worktree_registered_at(ctx, &canon, &path)? {
            res.existing = true;
            if !base_ref.trim().is_empty() {
                res.base_ref_ignored = true;
            }
            return Ok(res);
        }

        // `metadata` follows symlinks (matches Go's os.Stat): a dangling
        // symlink at `path` is treated as absent, not as PathOccupied.
        match std::fs::metadata(&path) {
            Ok(_) => return Err(WorktreeError::PathOccupied(path)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(WorktreeError::Other(format!("stat {path}: {e}"))),
        }
        if let Some(parent) = Path::new(&path).parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| WorktreeError::Other(format!("mkdir {}: {e}", parent.display())))?;
        }

        if branch_exists(ctx, &canon, &branch)? {
            if !base_ref.trim().is_empty() {
                res.base_ref_ignored = true;
            }
            run_git(ctx, &canon, &["worktree", "add", &path, &branch]).map_err(|e| {
                WorktreeError::Other(format!("worktree add (existing branch): {e}"))
            })?;
        } else {
            let mut args: Vec<String> = vec![
                "worktree".into(),
                "add".into(),
                path.clone(),
                "-b".into(),
                branch.clone(),
            ];
            let base = base_ref.trim();
            if !base.is_empty() {
                args.push(base.to_string());
            }
            let argv: Vec<&str> = args.iter().map(String::as_str).collect();
            run_git(ctx, &canon, &argv)
                .map_err(|e| WorktreeError::Other(format!("worktree add (new branch): {e}")))?;
        }
        Ok(res)
    }

    fn on_terminal(&self, ctx: &Ctx, project_root: &str, task_id: &str) -> WtResult<WtOutcome> {
        let canon = canon_root(project_root)?;
        let lk = self.lock(&canon, task_id);
        let _g = lk.lock().unwrap();

        let path = path_for(&canon, task_id)?;
        let mut res = WtOutcome {
            path: path.clone(),
            branch: branch_for(task_id),
            ..Default::default()
        };
        verify_git_available()?;

        // If git itself is broken, still try to reap the on-disk dir.
        if verify_git_repo(ctx, &canon).is_err() {
            match std::fs::metadata(&path) {
                Ok(_) => {
                    std::fs::remove_dir_all(&path).map_err(|e| {
                        WorktreeError::Other(format!("remove {path} after git failure: {e}"))
                    })?;
                    res.existed = true;
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => {
                    return Err(WorktreeError::Other(format!(
                        "stat {path} after git failure: {e}"
                    )))
                }
            }
            return Ok(res);
        }

        if worktree_registered_at(ctx, &canon, &path)? {
            run_git(ctx, &canon, &["worktree", "remove", "--force", &path])
                .map_err(|e| WorktreeError::Other(format!("worktree remove: {e}")))?;
            res.existed = true;
            return Ok(res);
        }

        match std::fs::metadata(&path) {
            Ok(_) => {
                std::fs::remove_dir_all(&path)
                    .map_err(|e| WorktreeError::Other(format!("remove orphan {path}: {e}")))?;
                res.existed = true;
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(WorktreeError::Other(format!("stat {path}: {e}"))),
        }
        let _ = run_git(ctx, &canon, &["worktree", "prune"]);
        Ok(res)
    }

    fn verify(&self, ctx: &Ctx, project_root: &str, task_id: &str) -> WtResult<()> {
        let canon = canon_root(project_root)?;
        let path = path_for(&canon, task_id)?;
        match std::fs::metadata(&path) {
            Ok(_) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Err(WorktreeError::WorktreeMissing(path))
            }
            Err(e) => return Err(WorktreeError::WorktreeStranded(format!("stat {path}: {e}"))),
        }
        let wt_gitdir = git_common_dir_from(ctx, &path)
            .map_err(|e| WorktreeError::WorktreeStranded(format!("{path}: {e}")))?;
        let proj_gitdir = git_common_dir_from(ctx, &canon)
            .map_err(|e| WorktreeError::NotGitRepo(format!("{canon}: {e}")))?;
        if !same_dir(&wt_gitdir, &proj_gitdir) {
            return Err(WorktreeError::WorktreeStranded(format!(
                "worktree gitdir={wt_gitdir}, project gitdir={proj_gitdir}"
            )));
        }
        Ok(())
    }
}

// ---- helpers --------------------------------------------------------------

/// Symlink-resolved, absolutised project root. The load-bearing helper that
/// keeps every caller computing the same slug. Falls back to a lexical clean
/// when the path can't be canonicalised (mirror of the Go `canonRoot`).
fn canon_root(project_root: &str) -> WtResult<String> {
    let abs = absolutize(project_root)
        .map_err(|e| WorktreeError::Other(format!("absolutise {project_root:?}: {e}")))?;
    let canon = std::fs::canonicalize(&abs)
        .map(|p| p.to_string_lossy().to_string())
        .unwrap_or_else(|_| abs);
    Ok(canon)
}

fn absolutize(p: &str) -> std::io::Result<String> {
    let path = Path::new(p);
    if path.is_absolute() {
        return Ok(clean(path).to_string_lossy().to_string());
    }
    let cwd = std::env::current_dir()?;
    Ok(clean(&cwd.join(path)).to_string_lossy().to_string())
}

/// `slugFor` — `basename(canon) + "-" + 8hex(sha256(canon))`. The 8 hex
/// chars are the first 4 bytes of the digest, matching Go's
/// `hex.EncodeToString(sum[:4])`.
fn slug_for(canon: &str) -> String {
    let base = Path::new(canon)
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_default();
    let digest = sha256_first4_hex(canon.as_bytes());
    format!("{base}-{digest}")
}

fn verify_git_available() -> WtResult<()> {
    which_git()
        .map(|_| ())
        .ok_or_else(|| WorktreeError::GitMissing("git not on PATH".into()))
}

fn which_git() -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    for dir in std::env::split_paths(&path) {
        let cand = dir.join("git");
        if cand.is_file() {
            return Some(cand);
        }
    }
    None
}

fn verify_git_repo(ctx: &Ctx, canon: &str) -> WtResult<()> {
    run_git(ctx, canon, &["rev-parse", "--git-dir"])
        .map(|_| ())
        .map_err(|e| WorktreeError::NotGitRepo(format!("{canon}: {e}")))
}

fn branch_exists(ctx: &Ctx, canon: &str, branch: &str) -> WtResult<bool> {
    let refspec = format!("refs/heads/{branch}");
    // show-ref --quiet emits no output, so the cancellable `run_git` (which
    // captures output) is overkill; an early ctx guard + a plain status is
    // enough for this fast local lookup.
    if ctx.is_cancelled() {
        return Err(WorktreeError::Other("show-ref: cancelled".into()));
    }
    let status = Command::new("git")
        .args(["-C", canon, "show-ref", "--verify", "--quiet", &refspec])
        .status()
        .map_err(|e| WorktreeError::Other(format!("show-ref {branch}: {e}")))?;
    if status.success() {
        return Ok(true);
    }
    match status.code() {
        Some(1) => Ok(false),
        other => Err(WorktreeError::Other(format!(
            "show-ref {branch}: exit {other:?}"
        ))),
    }
}

fn worktree_registered_at(ctx: &Ctx, canon: &str, target: &str) -> WtResult<bool> {
    let out = run_git(ctx, canon, &["worktree", "list", "--porcelain"])
        .map_err(|e| WorktreeError::Other(format!("worktree list: {e}")))?;
    let canon_target = std::fs::canonicalize(target)
        .map(|p| p.to_string_lossy().to_string())
        .unwrap_or_else(|_| clean(Path::new(target)).to_string_lossy().to_string());
    for line in out.lines() {
        let line = line.trim();
        if let Some(p) = line.strip_prefix("worktree ") {
            let p = p.trim();
            if same_dir(p, target) || same_dir(p, &canon_target) {
                return Ok(true);
            }
        }
    }
    Ok(false)
}

fn git_common_dir_from(ctx: &Ctx, cwd: &str) -> std::result::Result<String, String> {
    let raw = run_git(ctx, cwd, &["rev-parse", "--git-common-dir"])
        .map_err(|e| format!("rev-parse --git-common-dir at {cwd}: {e}"))?;
    let raw = raw.trim();
    if raw.is_empty() {
        return Err(format!("rev-parse --git-common-dir at {cwd}: empty output"));
    }
    let abs = if Path::new(raw).is_absolute() {
        raw.to_string()
    } else {
        Path::new(cwd).join(raw).to_string_lossy().to_string()
    };
    let abs = std::fs::canonicalize(&abs)
        .map(|p| p.to_string_lossy().to_string())
        .unwrap_or(abs);
    Ok(clean(Path::new(&abs)).to_string_lossy().to_string())
}

fn same_dir(a: &str, b: &str) -> bool {
    clean(Path::new(a)) == clean(Path::new(b))
}

/// Runs `git -C <cwd> <args>`, returning combined stdout+stderr on success
/// and an error carrying the captured output otherwise.
///
/// The child is spawned (not `output()`'d) and polled so a cancelled `ctx`
/// (daemon shutdown / task cancel) SIGKILLs the in-flight git — the analogue
/// of Go's `exec.CommandContext`. stdout/stderr are drained on dedicated
/// threads so a chatty command can't deadlock on a full pipe.
fn run_git(ctx: &Ctx, cwd: &str, args: &[&str]) -> std::result::Result<String, String> {
    let mut argv: Vec<&str> = vec!["-C", cwd];
    argv.extend_from_slice(args);
    let mut child = Command::new("git")
        .args(&argv)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| e.to_string())?;
    let mut out_pipe = child.stdout.take();
    let mut err_pipe = child.stderr.take();
    let out_h = std::thread::spawn(move || {
        let mut b = Vec::new();
        if let Some(p) = out_pipe.as_mut() {
            let _ = p.read_to_end(&mut b);
        }
        b
    });
    let err_h = std::thread::spawn(move || {
        let mut b = Vec::new();
        if let Some(p) = err_pipe.as_mut() {
            let _ = p.read_to_end(&mut b);
        }
        b
    });
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                let stdout = out_h.join().unwrap_or_default();
                let stderr = err_h.join().unwrap_or_default();
                let mut combined = String::from_utf8_lossy(&stdout).to_string();
                combined.push_str(&String::from_utf8_lossy(&stderr));
                if status.success() {
                    return Ok(combined);
                }
                return Err(combined.trim().to_string());
            }
            Ok(None) => {
                if ctx.is_cancelled() {
                    let _ = child.kill();
                    let _ = child.wait();
                    let _ = out_h.join();
                    let _ = err_h.join();
                    return Err(format!("git {}: cancelled", args.join(" ")));
                }
                std::thread::sleep(Duration::from_millis(10));
            }
            Err(e) => return Err(e.to_string()),
        }
    }
}

/// Resolves `$HOME`.
fn home_dir() -> Option<PathBuf> {
    std::env::var_os("HOME").map(PathBuf::from)
}

/// Lexical path clean (drops `.`, resolves `..` syntactically) — the
/// `filepath.Clean` analogue (same impl as `projectmgr::clean`).
fn clean(p: &Path) -> PathBuf {
    let mut out: Vec<std::ffi::OsString> = Vec::new();
    let mut abs = false;
    for comp in p.components() {
        use std::path::Component::*;
        match comp {
            RootDir => {
                abs = true;
                out.clear();
            }
            CurDir => {}
            ParentDir => match out.last() {
                Some(last) if last.to_string_lossy() != ".." => {
                    out.pop();
                }
                _ if !abs => out.push(std::ffi::OsString::from("..")),
                _ => {}
            },
            Prefix(pre) => out.push(pre.as_os_str().to_os_string()),
            Normal(s) => out.push(s.to_os_string()),
        }
    }
    let mut result = PathBuf::new();
    if abs {
        result.push("/");
    }
    for c in out {
        result.push(c);
    }
    if result.as_os_str().is_empty() {
        result.push(".");
    }
    result
}

/// First 4 bytes of SHA-256(`data`) as 8 lowercase hex chars. A minimal,
/// dependency-free SHA-256 (the slug must match Go's `sha256` byte-for-byte).
fn sha256_first4_hex(data: &[u8]) -> String {
    let digest = sha256(data);
    let mut s = String::with_capacity(8);
    for b in &digest[..4] {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

/// Minimal SHA-256 (FIPS 180-4). Dependency-free so the worktree slug stays
/// byte-identical to Go without pulling a hashing crate.
fn sha256(data: &[u8]) -> [u8; 32] {
    const K: [u32; 64] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
        0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
        0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
        0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
        0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
        0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
        0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
        0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
        0xc67178f2,
    ];
    let mut h: [u32; 8] = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
        0x5be0cd19,
    ];
    let mut msg = data.to_vec();
    let bitlen = (data.len() as u64).wrapping_mul(8);
    msg.push(0x80);
    while msg.len() % 64 != 56 {
        msg.push(0);
    }
    msg.extend_from_slice(&bitlen.to_be_bytes());

    for chunk in msg.chunks_exact(64) {
        let mut w = [0u32; 64];
        for (i, word) in w.iter_mut().enumerate().take(16) {
            *word = u32::from_be_bytes([
                chunk[i * 4],
                chunk[i * 4 + 1],
                chunk[i * 4 + 2],
                chunk[i * 4 + 3],
            ]);
        }
        for i in 16..64 {
            let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
            let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }
        let mut a = h;
        for i in 0..64 {
            let s1 = a[4].rotate_right(6) ^ a[4].rotate_right(11) ^ a[4].rotate_right(25);
            let ch = (a[4] & a[5]) ^ ((!a[4]) & a[6]);
            let t1 = a[7]
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(K[i])
                .wrapping_add(w[i]);
            let s0 = a[0].rotate_right(2) ^ a[0].rotate_right(13) ^ a[0].rotate_right(22);
            let maj = (a[0] & a[1]) ^ (a[0] & a[2]) ^ (a[1] & a[2]);
            let t2 = s0.wrapping_add(maj);
            a[7] = a[6];
            a[6] = a[5];
            a[5] = a[4];
            a[4] = a[3].wrapping_add(t1);
            a[3] = a[2];
            a[2] = a[1];
            a[1] = a[0];
            a[0] = t1.wrapping_add(t2);
        }
        for i in 0..8 {
            h[i] = h[i].wrapping_add(a[i]);
        }
    }
    let mut out = [0u8; 32];
    for (i, word) in h.iter().enumerate() {
        out[i * 4..i * 4 + 4].copy_from_slice(&word.to_be_bytes());
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sha256_known_vector() {
        // SHA-256("abc") = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
        let d = sha256(b"abc");
        let hex: String = d.iter().map(|b| format!("{b:02x}")).collect();
        assert_eq!(
            hex,
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn branch_and_slug_shape() {
        assert_eq!(branch_for("ask-abc123"), "autosk/ask-abc123");
        // slug = base + "-" + 8 hex chars
        let slug = slug_for("/tmp/myproj");
        assert!(slug.starts_with("myproj-"));
        assert_eq!(slug.len(), "myproj-".len() + 8);
    }
}
