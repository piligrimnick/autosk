//! Persisted project registry (plan §7.4).
//!
//! `~/.autosk/projects.json` is the GUI/daemon's durable list of known
//! projects (the sidebar source for `project.list/add/remove`). The CLI's
//! walk-up from cwd (see [`crate::projectmgr`]) stays for ergonomics; this
//! registry is the *explicit* list.
//!
//! Writes are atomic (temp file + rename) and the file is mode `0600`, the dir
//! `0700`, matching the daemon socket/token discipline.

use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};
use autosk_proto::wire::ProjectInfo;

/// On-disk file shape: a JSON array of entries.
#[derive(Debug, Default, Serialize, Deserialize)]
struct RegistryFile {
    #[serde(default)]
    projects: Vec<Entry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Entry {
    root: String,
    db_path: String,
    #[serde(default)]
    name: String,
}

impl From<Entry> for ProjectInfo {
    fn from(e: Entry) -> ProjectInfo {
        ProjectInfo {
            root: e.root,
            db_path: e.db_path,
            name: e.name,
        }
    }
}

/// The persisted project registry, keyed by canonical root.
pub struct Registry {
    path: PathBuf,
    /// Serialises read-modify-write mutations so concurrent `add`/`remove`
    /// across daemon connections neither lose updates nor collide on the temp
    /// file.
    write_lock: Mutex<()>,
}

impl Registry {
    /// Opens the registry at the default location (`~/.autosk/projects.json`).
    pub fn open_default() -> Result<Registry> {
        let home = home_dir().ok_or_else(|| {
            Error::Io(std::io::Error::new(
                std::io::ErrorKind::NotFound,
                "HOME not set",
            ))
        })?;
        Ok(Registry {
            path: home.join(".autosk").join("projects.json"),
            write_lock: Mutex::new(()),
        })
    }

    /// Opens the registry at an explicit path (tests).
    pub fn open_at(path: impl Into<PathBuf>) -> Registry {
        Registry {
            path: path.into(),
            write_lock: Mutex::new(()),
        }
    }

    /// Returns every registered project, ordered by root.
    pub fn list(&self) -> Result<Vec<ProjectInfo>> {
        let mut out: Vec<ProjectInfo> = self.load()?.projects.into_iter().map(Into::into).collect();
        out.sort_by(|a, b| a.root.cmp(&b.root));
        Ok(out)
    }

    /// Adds (or refreshes) a project entry, keyed by canonical `root`. Returns
    /// the stored entry. Idempotent: re-adding the same root replaces it.
    pub fn add(&self, root: &str, db_path: &str) -> Result<ProjectInfo> {
        let _guard = self
            .write_lock
            .lock()
            .map_err(|_| Error::Migration("registry write lock poisoned".to_string()))?;
        let name = Path::new(root)
            .file_name()
            .map(|s| s.to_string_lossy().to_string())
            .unwrap_or_default();
        let mut file = self.load()?;
        file.projects.retain(|e| e.root != root);
        let entry = Entry {
            root: root.to_string(),
            db_path: db_path.to_string(),
            name,
        };
        file.projects.push(entry.clone());
        self.save(&file)?;
        Ok(entry.into())
    }

    /// Removes the project with the given root. Returns whether a row matched.
    pub fn remove(&self, root: &str) -> Result<bool> {
        let _guard = self
            .write_lock
            .lock()
            .map_err(|_| Error::Migration("registry write lock poisoned".to_string()))?;
        let mut file = self.load()?;
        let before = file.projects.len();
        file.projects.retain(|e| e.root != root);
        let removed = file.projects.len() != before;
        if removed {
            self.save(&file)?;
        }
        Ok(removed)
    }

    /// Reads and parses the on-disk registry.
    ///
    /// A missing file (or an empty / whitespace-only one) is a fresh, empty
    /// registry. A **non-empty but unparseable** file is a hard error rather
    /// than a silent reset to default: otherwise the next `add()` would
    /// `load(empty) -> push -> save()` and destroy every previously-registered
    /// project (the GUI sidebar source). This mirrors the in-repo precedent of
    /// bailing on a malformed state file instead of clobbering it
    /// (`cmd/autosk/lazy.go` `buildChangelogModal`). The operator can move the
    /// bad file aside (it is reported in the error) and retry.
    fn load(&self) -> Result<RegistryFile> {
        let bytes = match std::fs::read(&self.path) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok(RegistryFile::default())
            }
            Err(e) => return Err(Error::Io(e)),
        };
        if bytes.iter().all(|b| b.is_ascii_whitespace()) {
            // An empty / whitespace-only file is treated as a fresh registry
            // (e.g. a zero-byte file left by an interrupted first write).
            return Ok(RegistryFile::default());
        }
        serde_json::from_slice(&bytes).map_err(|e| {
            Error::Registry(format!(
                "{} is corrupt ({e}); move it aside and retry",
                self.path.display()
            ))
        })
    }

    /// Atomically persists the registry: write a sibling temp file (0600),
    /// fsync, rename over the target; ensure the parent dir is 0700.
    fn save(&self, file: &RegistryFile) -> Result<()> {
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir)?;
            // Best-effort 0700 on the dir (own it the way the socket dir is owned).
            let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700));
        }
        let mut tmp = self.path.clone();
        let pid = std::process::id();
        tmp.set_extension(format!("tmp.{pid}"));
        let bytes = serde_json::to_vec_pretty(file)
            .map_err(|e| Error::Migration(format!("registry: marshal: {e}")))?;
        {
            use std::io::Write;
            let mut f = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)?;
            f.write_all(&bytes)?;
            f.sync_all()?;
        }
        std::fs::rename(&tmp, &self.path)?;
        Ok(())
    }
}

/// Resolves the user's home directory from `$HOME`.
pub fn home_dir() -> Option<PathBuf> {
    std::env::var_os("HOME").map(PathBuf::from)
}
