//! Per-daemon project cache — the Rust port of `internal/daemon/projectmgr`
//! (plan §7.1).
//!
//! Each RPC carries a project selector (`{cwd}` or `{db_path}`, mirroring the
//! old `X-Autosk-Cwd` / `X-Autosk-DB` headers). The manager resolves it to a
//! canonical project root, opens the project's `.autosk/db` lazily on first
//! sight (running migrations + the stale-`running` sweep), and caches the
//! [`Db`] keyed by canonical root so concurrent resolves of the same project
//! share one handle.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use crate::error::{Error, Result};
use crate::store::Db;

/// `.autosk` directory name and the db file within it (mirror `projectdb`).
const DIR: &str = ".autosk";
const FILE: &str = "db";

/// A resolved, opened project.
pub struct Project {
    /// Canonical absolute project root (parent of `.autosk/`).
    pub root: String,
    /// Absolute path to `.autosk/db`.
    pub db_path: String,
    /// The shared doltlite handle (sole owner of the file).
    pub db: Arc<Db>,
    /// RFC3339 UTC time the project was opened (for `healthz` `opened_at`).
    pub opened_at: String,
}

/// The per-daemon project cache.
pub struct Manager {
    projects: Mutex<HashMap<String, Arc<Project>>>,
    /// Per-root open serialisation. The first resolver of a root holds the
    /// root's lock across open + migrate + sweep + publish, so two concurrent
    /// first-resolvers of the same project cannot both open it (and thus cannot
    /// double-run `migrate` or the `human` seed). Distinct roots hold distinct
    /// locks and open in parallel. This is the in-process analogue of the Go
    /// `projectmgr`'s per-entry `readyCh` (review R6).
    open_locks: Mutex<HashMap<String, Arc<Mutex<()>>>>,
}

impl Default for Manager {
    fn default() -> Self {
        Self::new()
    }
}

impl Manager {
    /// Constructs an empty manager.
    pub fn new() -> Manager {
        Manager {
            projects: Mutex::new(HashMap::new()),
            open_locks: Mutex::new(HashMap::new()),
        }
    }

    /// Resolves the project for a selector, opening it on first sight.
    ///
    /// `cwd` is walked up to find `.autosk/db`; `db_override` (when non-empty)
    /// wins and is used verbatim. The db file must already exist (the daemon
    /// never auto-creates here — use [`Manager::init`] for greenfield init),
    /// mirroring the Go `projectmgr.Resolve` stat guard.
    pub fn resolve(&self, cwd: &str, db_override: &str) -> Result<Arc<Project>> {
        let db_path = resolve_db_path(cwd, db_override)?;
        let db_path = absolutize(&db_path)?;
        if !Path::new(&db_path).exists() {
            return Err(Error::ProjectNotFound(format!(
                "db file missing at {db_path}"
            )));
        }
        let root = canonical_root(&db_path);

        // Fast path: already loaded.
        if let Some(p) = self.cached(&root)? {
            return Ok(p);
        }

        // Serialise first-open per root: grab (or create) the root's open lock,
        // then hold it across open+migrate+sweep+publish. Concurrent
        // first-resolvers of the SAME root queue here; other roots are
        // unaffected (distinct locks).
        let open_lock = {
            let mut locks = self
                .open_locks
                .lock()
                .map_err(|_| Error::LockPoisoned("open_locks"))?;
            locks.entry(root.clone()).or_default().clone()
        };
        let _open_guard = open_lock.lock().map_err(|_| Error::LockPoisoned("open"))?;

        // Re-check under the open lock: another first-resolver may have
        // published the project while we waited.
        if let Some(p) = self.cached(&root)? {
            return Ok(p);
        }

        // We are the sole opener. Open outside the projects map lock so other
        // roots resolve in parallel; on error nothing is published, so the next
        // caller retries with fresh state (matching Go's entry-removal-on-error).
        let db = Db::open(&db_path)?;
        db.migrate()?;
        db.sweep_running_on_startup()?;
        let opened_at = crate::timefmt::rfc3339_utc(
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_secs() as i64)
                .unwrap_or(0),
        );
        let project = Arc::new(Project {
            root: root.clone(),
            db_path,
            db: Arc::new(db),
            opened_at,
        });
        self.projects
            .lock()
            .map_err(|_| Error::LockPoisoned("projects"))?
            .insert(root, project.clone());
        Ok(project)
    }

    /// Returns the already-loaded project for `root`, if any.
    fn cached(&self, root: &str) -> Result<Option<Arc<Project>>> {
        Ok(self
            .projects
            .lock()
            .map_err(|_| Error::LockPoisoned("projects"))?
            .get(root)
            .cloned())
    }

    /// Returns a snapshot of currently-loaded projects (order unspecified).
    pub fn loaded(&self) -> Vec<Arc<Project>> {
        self.projects
            .lock()
            .map(|m| m.values().cloned().collect())
            .unwrap_or_default()
    }

    /// Creates (greenfield) and migrates `<dir>/.autosk/db`, returning the
    /// canonical root + db path. Used by the `autoskd init` affordance so a
    /// fresh v12 DB exists for the read surface to serve.
    pub fn init(dir: &str) -> Result<(String, String)> {
        let dir_abs = absolutize(dir)?;
        let autosk_dir = Path::new(&dir_abs).join(DIR);
        std::fs::create_dir_all(&autosk_dir)?;
        // Canonicalise the root now that it exists so the db path the daemon
        // later resolves (which canonicalises too) matches what init returns.
        let root = match std::fs::canonicalize(&dir_abs) {
            Ok(p) => p.to_string_lossy().to_string(),
            Err(_) => clean(Path::new(&dir_abs)).to_string_lossy().to_string(),
        };
        let db_path = Path::new(&root).join(DIR).join(FILE);
        let db_path_str = db_path.to_string_lossy().to_string();
        let db = Db::open_or_create(&db_path)?;
        db.migrate()?;
        Ok((root, db_path_str))
    }
}

/// Walk-up resolution (port of `projectdb.ResolveNoEnv`): `override` wins;
/// otherwise walk up from `cwd` looking for `.autosk/db`.
pub fn resolve_db_path(cwd: &str, db_override: &str) -> Result<String> {
    if !db_override.is_empty() {
        return Ok(db_override.to_string());
    }
    let cwd = Path::new(cwd);
    let cwd = clean(cwd);
    if cwd.as_os_str().is_empty() || cwd == Path::new(".") || !cwd.is_absolute() {
        return Err(Error::InvalidProject(format!(
            "{} (must be absolute)",
            cwd.display()
        )));
    }
    let mut dir = cwd.as_path();
    loop {
        let candidate = dir.join(DIR).join(FILE);
        if candidate.is_file() {
            return Ok(candidate.to_string_lossy().to_string());
        }
        match dir.parent() {
            Some(p) if p != dir => dir = p,
            _ => break,
        }
    }
    Err(Error::ProjectNotFound(format!("from {}", cwd.display())))
}

/// Canonical root = `EvalSymlinks(dir(dir(db_path)))`, falling back to the
/// lexical clean when the path can't be canonicalised (mirrors the Go logic).
fn canonical_root(db_path: &str) -> String {
    let raw_root = Path::new(db_path)
        .parent()
        .and_then(Path::parent)
        .unwrap_or_else(|| Path::new("/"));
    match std::fs::canonicalize(raw_root) {
        Ok(p) => p.to_string_lossy().to_string(),
        Err(_) => clean(raw_root).to_string_lossy().to_string(),
    }
}

fn absolutize(p: &str) -> Result<String> {
    let path = Path::new(p);
    if path.is_absolute() {
        return Ok(clean(path).to_string_lossy().to_string());
    }
    let cwd = std::env::current_dir()?;
    Ok(clean(&cwd.join(path)).to_string_lossy().to_string())
}

/// Lexical path clean (drops `.` and resolves `..` syntactically), the
/// `filepath.Clean` analogue std doesn't provide directly.
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::thread;

    /// Concurrent first-resolvers of one project open it exactly once — so
    /// `migrate()` + the `human` seed run once, not once per racing thread
    /// (review R6). The DB is created fresh and UNMIGRATED so the first opener
    /// is the one that applies the schema and seeds.
    #[test]
    fn concurrent_first_resolve_opens_once() {
        let dir = tempfile::tempdir().unwrap();
        let autosk = dir.path().join(DIR);
        std::fs::create_dir_all(&autosk).unwrap();
        // Create (but do not migrate) the db file so resolve()'s existence
        // guard passes; drop the handle so the first resolver can open it.
        drop(Db::open_or_create(autosk.join(FILE)).unwrap());
        let root = dir.path().to_string_lossy().to_string();

        let mgr = Arc::new(Manager::new());
        let handles: Vec<_> = (0..8)
            .map(|_| {
                let mgr = Arc::clone(&mgr);
                let cwd = root.clone();
                thread::spawn(move || mgr.resolve(&cwd, "").expect("resolve").root.clone())
            })
            .collect();
        let roots: Vec<_> = handles.into_iter().map(|h| h.join().unwrap()).collect();

        // Every resolver observed the same single opened project.
        assert!(roots.iter().all(|r| *r == roots[0]));
        assert_eq!(mgr.loaded().len(), 1, "exactly one project opened");

        // migrate() + seed ran once across the race → exactly one human agent.
        let proj = mgr.resolve(&root, "").unwrap();
        let humans: i64 = proj
            .db
            .with_read(|c| {
                Ok(
                    c.query_row("SELECT COUNT(*) FROM agents WHERE name='human'", [], |r| {
                        r.get(0)
                    })?,
                )
            })
            .unwrap();
        assert_eq!(
            humans, 1,
            "human seeded exactly once across concurrent opens"
        );
    }
}
