//! Minimal doltlite-backed store for the Phase 0 spike.
//!
//! This is **not** the full store port (that is Phase 1). It exists only to
//! exercise the load-bearing unknowns called out in
//! `docs/plans/20260607-Rust-Daemon-Tauri-GUI.md` §9 Phase 0:
//!
//!   * open an existing `.autosk/db` produced by the Go binary and read `tasks`
//!     (forward-compat: Go-0.10.8 file under Rust-0.11.8);
//!   * run `dolt_commit` / `dolt_gc` and stay readable afterwards;
//!   * demonstrate the **RwLock GC discipline** — a writer running `dolt_gc()`
//!     under a write lock while N reader connections run SELECTs under read
//!     locks, with no errors and no corruption.
//!
//! ## RwLock GC discipline (the race-closing mechanism)
//!
//! `dolt_gc()` rewrites the database via write-to-sidecar + atomic rename, so
//! the on-disk inode rotates on every successful GC. The Go driver only
//! *best-effort* revalidated the inode per pool checkout, leaving a ~1e-7
//! mid-statement race (see `internal/store/doltlite/driver.go`). Because the
//! Rust daemon is the **sole owner** of the DB, we replace that with a hard
//! in-process rule:
//!
//!   * every read holds a shared [`RwLock`] read guard for the full statement;
//!   * GC takes the exclusive write guard, so it can only run once all
//!     in-flight reads have drained, and no read can begin mid-rename;
//!   * after GC completes (still holding the write guard) we drop every pooled
//!     reader connection, because their file descriptors now point at the
//!     orphaned pre-GC inode. The next checkout reopens at the current path.
//!
//! The race is gone, not merely shrunk.
//!
//! ## RwLock fairness assumption
//!
//! The discipline's *correctness* (no read ever overlaps the rename) does not
//! depend on fairness, but its *liveness* does: GC must eventually acquire the
//! write guard. `std::sync::RwLock` fairness is unspecified and platform-
//! dependent — a strictly reader-preferring implementation under sustained read
//! load could starve the writer. We rely on readers releasing the guard between
//! every statement (so the writer can wedge in) and on std's implementations
//! admitting a waiting writer in practice. The stress test
//! (`tests/gc_stress.rs`) guards this with a watchdog timeout so a future
//! starvation regression surfaces as a test failure rather than a hang.

use std::path::{Path, PathBuf};
use std::sync::{Mutex, RwLock};
use std::time::Duration;

use rusqlite::{Connection, OpenFlags};

/// Busy timeout applied to every connection, matching the Go store
/// (`internal/store/doltlite/store.go`): concurrent writers queue on the
/// doltlite write lock instead of returning `SQLITE_BUSY` immediately.
const BUSY_TIMEOUT: Duration = Duration::from_secs(30);

/// Result alias for the spike store.
pub type Result<T> = std::result::Result<T, Error>;

/// Errors surfaced by the spike store.
#[derive(Debug)]
pub enum Error {
    /// A SQLite/doltlite-level error.
    Sqlite(rusqlite::Error),
    /// An internal lock was poisoned by a panicking thread.
    LockPoisoned(&'static str),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Sqlite(e) => write!(f, "doltlite: {e}"),
            Error::LockPoisoned(what) => write!(f, "doltlite: {what} lock poisoned"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Sqlite(e) => Some(e),
            Error::LockPoisoned(_) => None,
        }
    }
}

impl From<rusqlite::Error> for Error {
    fn from(e: rusqlite::Error) -> Self {
        Error::Sqlite(e)
    }
}

/// A single row from `tasks`, narrowed to the columns the Phase 0 golden
/// snapshot pins. Ordered/compared deterministically for forward-compat tests.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskRow {
    pub id: String,
    pub title: String,
    pub status: String,
    pub priority: i64,
}

/// Parsed result of `SELECT dolt_gc()`. Mirrors the Go `CompactResult`
/// (`internal/store/doltlite/maint.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GcStats {
    pub chunks_removed: i64,
    pub chunks_kept: i64,
    pub raw: String,
}

/// A doltlite-backed database handle for the spike.
///
/// Holds a dedicated single writer connection (doltlite is single-writer) and a
/// pool of reader connections, both governed by one [`RwLock`] that implements
/// the GC discipline documented at the module level.
pub struct Db {
    path: PathBuf,
    /// The GC discipline lock: readers take `.read()`, GC takes `.write()`.
    gc_lock: RwLock<()>,
    /// Single writer connection (commits + GC). doltlite serialises writes, so
    /// one writer is both necessary and sufficient.
    writer: Mutex<Connection>,
    /// Idle reader connections, reused across reads and dropped on GC.
    idle_readers: Mutex<Vec<Connection>>,
}

impl Db {
    /// Opens an existing `.autosk/db` (read-write, never creating it).
    pub fn open(path: impl AsRef<Path>) -> Result<Db> {
        let path = path.as_ref().to_path_buf();
        let writer = open_conn(&path, OpenFlags::SQLITE_OPEN_READ_WRITE)?;
        Ok(Db {
            path,
            gc_lock: RwLock::new(()),
            writer: Mutex::new(writer),
            idle_readers: Mutex::new(Vec::new()),
        })
    }

    /// The on-disk path this handle was opened against.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Reads every row of `tasks`, ordered by `id`, narrowed to [`TaskRow`].
    /// Taken under a read guard so it never overlaps a GC.
    pub fn list_tasks(&self) -> Result<Vec<TaskRow>> {
        self.with_read(list_tasks)
    }

    /// Runs `SELECT dolt_commit('-A','-m', msg)` and returns the commit hash. A
    /// commit appends chunks but does not rotate the inode, so it only needs
    /// the writer mutex.
    ///
    /// **Every** doltlite/sqlite error is propagated — including the post-GC
    /// corruption/loss signals (`SQLITE_BUSY`, `SQLITE_IOERR`, `SQLITE_CORRUPT`,
    /// `SQLITE_NOTADB`) this de-risking spike exists to catch. The spike never
    /// commits an empty changeset (seed + churn always have rows), so there is
    /// no empty-commit case to special-case away here.
    ///
    /// Note on Go parity: it is the *callers* in the Go store (e.g.
    /// `internal/lazy/datasource/offline.go`, `cmd/autosk/comment.go`,
    /// `cmd/autosk/agent.go`) that swallow empty-changeset errors via
    /// `_ = ...DoltCommit(...)`; `DoltCommit` itself returns the wrapped error
    /// (`return fmt.Errorf("dolt_commit: %w", err)`). This `commit` matches
    /// `DoltCommit`, not its callers.
    pub fn commit(&self, msg: &str) -> Result<String> {
        let writer = self
            .writer
            .lock()
            .map_err(|_| Error::LockPoisoned("writer"))?;
        let hash = writer.query_row("SELECT dolt_commit('-A', '-m', ?1)", [msg], |row| {
            row.get::<_, String>(0)
        })?;
        Ok(hash)
    }

    /// Runs `SELECT dolt_gc()` under the exclusive GC write guard, then drops
    /// every pooled reader connection (their fds now point at the orphaned
    /// pre-GC inode). See the module-level discipline notes.
    pub fn gc(&self) -> Result<GcStats> {
        let _guard = self
            .gc_lock
            .write()
            .map_err(|_| Error::LockPoisoned("gc"))?;
        let writer = self
            .writer
            .lock()
            .map_err(|_| Error::LockPoisoned("writer"))?;
        let raw: String = writer.query_row("SELECT dolt_gc()", [], |row| row.get(0))?;
        // Retire stale reader connections while we still hold the write guard,
        // so no reader can pick one up after the rename.
        self.idle_readers
            .lock()
            .map_err(|_| Error::LockPoisoned("idle_readers"))?
            .clear();
        Ok(parse_gc(raw))
    }

    /// Runs `f` against the single writer connection. Used for commits, DDL and
    /// churn inserts. A normal write does not rotate the inode, so this takes
    /// only the writer mutex (not the GC write guard) and may overlap reads;
    /// doltlite serialises the actual write via its file lock + busy timeout.
    pub fn with_write<T>(&self, f: impl FnOnce(&Connection) -> Result<T>) -> Result<T> {
        let writer = self
            .writer
            .lock()
            .map_err(|_| Error::LockPoisoned("writer"))?;
        f(&writer)
    }

    /// Runs `f` against a reader connection while holding a shared read guard,
    /// so the statement can never overlap a GC. The connection is checked out
    /// from the pool (or freshly opened) and returned to the pool on success.
    pub fn with_read<T>(&self, f: impl FnOnce(&Connection) -> Result<T>) -> Result<T> {
        let _guard = self.gc_lock.read().map_err(|_| Error::LockPoisoned("gc"))?;
        let conn = self.checkout_reader()?;
        match f(&conn) {
            Ok(out) => {
                self.checkin_reader(conn)?;
                Ok(out)
            }
            // Drop (do not recycle) a connection that errored.
            Err(e) => Err(e),
        }
    }

    fn checkout_reader(&self) -> Result<Connection> {
        if let Some(conn) = self
            .idle_readers
            .lock()
            .map_err(|_| Error::LockPoisoned("idle_readers"))?
            .pop()
        {
            return Ok(conn);
        }
        open_conn(&self.path, OpenFlags::SQLITE_OPEN_READ_ONLY)
    }

    fn checkin_reader(&self, conn: Connection) -> Result<()> {
        self.idle_readers
            .lock()
            .map_err(|_| Error::LockPoisoned("idle_readers"))?
            .push(conn);
        Ok(())
    }
}

/// Reads `tasks` (narrowed) ordered by id.
fn list_tasks(conn: &Connection) -> Result<Vec<TaskRow>> {
    let mut stmt = conn.prepare("SELECT id, title, status, priority FROM tasks ORDER BY id")?;
    let rows = stmt.query_map([], |row| {
        Ok(TaskRow {
            id: row.get(0)?,
            title: row.get(1)?,
            status: row.get(2)?,
            priority: row.get(3)?,
        })
    })?;
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

/// Opens a doltlite connection with the shared pragmas (busy timeout, foreign
/// keys), mirroring the Go store's `Open`.
fn open_conn(path: &Path, flags: OpenFlags) -> Result<Connection> {
    // SQLITE_OPEN_NO_MUTEX: each connection is owned by one thread at a time
    // (the pool hands them out serially), so per-connection mutexing is wasted.
    let conn = Connection::open_with_flags(path, flags | OpenFlags::SQLITE_OPEN_NO_MUTEX)?;
    conn.busy_timeout(BUSY_TIMEOUT)?;
    // foreign_keys is a no-op for read-only connections but harmless; matches
    // the Go DSN (`_foreign_keys=on`).
    conn.pragma_update(None, "foreign_keys", "ON")?;
    Ok(conn)
}

/// Parses `"<removed> chunks removed, <kept> chunks kept"`. Best-effort: a
/// format change leaves the counters at zero but preserves `raw`, matching the
/// Go `parseCounter`.
fn parse_gc(raw: String) -> GcStats {
    GcStats {
        chunks_removed: parse_counter(&raw, "chunks removed"),
        chunks_kept: parse_counter(&raw, "chunks kept"),
        raw,
    }
}

fn parse_counter(s: &str, suffix: &str) -> i64 {
    let Some(idx) = s.find(suffix) else {
        return 0;
    };
    let before = s[..idx].trim_end();
    // Parse the trailing run of ASCII digits in place — no allocation, mirroring
    // the Go `parseCounter` byte walk. `rfind` returns the byte index of the
    // last non-digit char; +1 is the start of the digit run (ASCII digits are
    // one byte, so the slice boundary is always valid), or 0 when `before` is
    // all digits.
    let start = before
        .rfind(|c: char| !c.is_ascii_digit())
        .map_or(0, |i| i + 1);
    before[start..].parse().unwrap_or(0)
}
