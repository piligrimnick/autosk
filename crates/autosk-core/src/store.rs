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

use crate::error::{Error, Result};
use crate::read::{self, JobFilter, TaskFilter};
use autosk_proto::wire;

/// Busy timeout applied to every connection, matching the Go store
/// (`internal/store/doltlite/store.go`): concurrent writers queue on the
/// doltlite write lock instead of returning `SQLITE_BUSY` immediately.
const BUSY_TIMEOUT: Duration = Duration::from_secs(30);

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

    /// Opens `.autosk/db`, creating an empty doltlite (v12) file if it does not
    /// exist. Used by the greenfield init path; the daemon's read path uses
    /// [`Db::open`] against an already-resolved file.
    pub fn open_or_create(path: impl AsRef<Path>) -> Result<Db> {
        let path = path.as_ref().to_path_buf();
        let writer = open_conn(
            &path,
            OpenFlags::SQLITE_OPEN_READ_WRITE | OpenFlags::SQLITE_OPEN_CREATE,
        )?;
        Ok(Db {
            path,
            gc_lock: RwLock::new(()),
            writer: Mutex::new(writer),
            idle_readers: Mutex::new(Vec::new()),
        })
    }

    /// Runs all pending schema migrations on the writer connection (idempotent),
    /// returning the resulting schema version. Mirrors the Go store's
    /// `Migrate` step run by `projectmgr.openProject`.
    pub fn migrate(&self) -> Result<i64> {
        let writer = self
            .writer
            .lock()
            .map_err(|_| Error::LockPoisoned("writer"))?;
        crate::migrate::migrate(&writer)
    }

    /// Returns the highest applied migration version, or 0.
    pub fn schema_version(&self) -> Result<i64> {
        self.with_read(crate::migrate::current_version)
    }

    /// Restart recovery: rewrite every `daemon_runs` row left in `running` to
    /// `failed` with `error='daemon_restart'`, returning the count swept.
    /// Mirrors `runstore.SweepRunningOnStartup`; run once on project open
    /// before any poller/reader observes the rows.
    pub fn sweep_running_on_startup(&self) -> Result<i64> {
        let writer = self
            .writer
            .lock()
            .map_err(|_| Error::LockPoisoned("writer"))?;
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0);
        let n = writer.execute(
            "UPDATE daemon_runs \
                SET status = 'failed', \
                    error  = COALESCE(NULLIF(error,''), 'daemon_restart'), \
                    finished_at = COALESCE(finished_at, ?1), \
                    pid    = NULL \
              WHERE status = 'running'",
            rusqlite::params![now],
        )?;
        Ok(n as i64)
    }

    // ---- read surface (plan §5). Each runs under the GC read guard. --------

    /// `task.list` — enriched task views matching `filter`.
    pub fn task_list(&self, filter: &TaskFilter) -> Result<Vec<wire::TaskView>> {
        self.with_read(|conn| read::task_list(conn, filter))
    }

    /// `task.get` — one enriched task view, or [`Error::NotFound`].
    pub fn task_get(&self, id: &str) -> Result<wire::TaskView> {
        self.with_read(|conn| read::task_get(conn, id))
    }

    /// `task.ready` — the ready set (status='new', no open blocker).
    pub fn task_ready(&self, limit: i64) -> Result<Vec<wire::TaskView>> {
        self.with_read(|conn| read::task_ready(conn, limit))
    }

    /// `comment.list` — a task's comment thread, oldest first.
    pub fn comment_list(&self, task_id: &str) -> Result<Vec<wire::Comment>> {
        self.with_read(|conn| read::comment_list(conn, task_id))
    }

    /// `workflow.list` — workflows + steps + per-step task counts.
    pub fn workflow_list(&self, include_synthetic: bool) -> Result<Vec<wire::Workflow>> {
        self.with_read(|conn| read::workflow_list(conn, include_synthetic))
    }

    /// `workflow.get` — one workflow by name, or [`Error::NotFound`].
    pub fn workflow_get(&self, name: &str) -> Result<wire::Workflow> {
        self.with_read(|conn| read::workflow_get(conn, name))
    }

    /// `agent.list` — agents + package metadata + tasks-owned counts.
    pub fn agent_list(&self) -> Result<Vec<wire::Agent>> {
        self.with_read(read::agent_list)
    }

    /// `job.list` — daemon_runs decorated with workflow/step/agent labels.
    pub fn job_list(&self, filter: &JobFilter) -> Result<Vec<wire::Job>> {
        self.with_read(|conn| read::job_list(conn, filter))
    }

    /// `job.get` — one decorated job, or [`Error::NotFound`].
    pub fn job_get(&self, id: &str) -> Result<wire::Job> {
        self.with_read(|conn| read::job_get(conn, id))
    }

    /// `signal.forTask` — every step_signals row for a task, newest first.
    pub fn signal_for_task(&self, task_id: &str) -> Result<Vec<wire::Signal>> {
        self.with_read(|conn| read::signal_for_task(conn, task_id))
    }

    /// `signal.forJob` — step_signals rows for a single run, newest first.
    pub fn signal_for_job(&self, job_id: &str) -> Result<Vec<wire::Signal>> {
        self.with_read(|conn| read::signal_for_job(conn, job_id))
    }

    /// Returns the run's `session_path` (for `job.messages`), or
    /// [`Error::NotFound`] if the run is unknown.
    pub fn job_session_path(&self, job_id: &str) -> Result<Option<String>> {
        self.with_read(|conn| read::job_session_path(conn, job_id))
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

    /// Like [`Db::with_write`] but `f` returns a plain value (not a `Result`).
    /// Used by verbs (e.g. `workflow.updateIsolation`) whose helper returns a
    /// report tuple even on error. Only a poisoned writer lock fails.
    pub fn with_writer<T>(&self, f: impl FnOnce(&Connection) -> T) -> Result<T> {
        let writer = self
            .writer
            .lock()
            .map_err(|_| Error::LockPoisoned("writer"))?;
        Ok(f(&writer))
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

// ---- executor-side store surface (Phase 2) --------------------------------
//
// Every op below runs on the SINGLE writer connection via `with_write`, so a
// read observes the executor's own preceding writes — the Rust analogue of
// the Go store's `SetMaxOpenConns(1)` read-your-writes lane. (The Phase-1 RPC
// read surface keeps using the reader pool; doltlite makes the writer's
// autocommitted changes visible there too.)
impl Db {
    // tasks -----------------------------------------------------------------

    /// `GetTask`.
    pub fn task_get_row(&self, id: &str) -> Result<crate::tasks::Task> {
        self.with_write(|conn| crate::tasks::get_task(conn, id))
    }

    /// `CreateTask` (bootstrap/enroll/tests).
    pub fn task_create(&self, t: crate::tasks::Task) -> Result<crate::tasks::Task> {
        self.with_write(|conn| crate::tasks::create_task(conn, t))
    }

    /// `UpdateTask`.
    pub fn task_update(&self, id: &str, p: &crate::tasks::TaskPatch) -> Result<crate::tasks::Task> {
        self.with_write(|conn| crate::tasks::update_task(conn, id, p))
    }

    // daemon_runs -----------------------------------------------------------

    pub fn run_create(&self, nr: &crate::runstore::NewRun) -> Result<crate::runstore::Run> {
        self.with_write(|conn| crate::runstore::create_run(conn, nr))
    }
    pub fn run_get(&self, job_id: &str) -> Result<crate::runstore::Run> {
        self.with_write(|conn| crate::runstore::get_run(conn, job_id))
    }
    pub fn run_list(&self, f: &crate::runstore::RunFilter) -> Result<Vec<crate::runstore::Run>> {
        self.with_write(|conn| crate::runstore::list_runs(conn, f))
    }
    pub fn run_mark_running(&self, job_id: &str, pid: i64) -> Result<crate::runstore::Run> {
        self.with_write(|conn| crate::runstore::mark_running(conn, job_id, pid))
    }
    pub fn run_mark_done(
        &self,
        job_id: &str,
        exit_code: i64,
        transition_id: Option<i64>,
    ) -> Result<crate::runstore::Run> {
        self.with_write(|conn| crate::runstore::mark_done(conn, job_id, exit_code, transition_id))
    }
    pub fn run_mark_failed(
        &self,
        job_id: &str,
        exit_code: Option<i64>,
        err_msg: &str,
    ) -> Result<crate::runstore::Run> {
        self.with_write(|conn| crate::runstore::mark_failed(conn, job_id, exit_code, err_msg))
    }
    pub fn run_mark_cancelled(
        &self,
        job_id: &str,
        exit_code: Option<i64>,
    ) -> Result<crate::runstore::Run> {
        self.with_write(|conn| crate::runstore::mark_cancelled(conn, job_id, exit_code))
    }
    pub fn run_set_pid(&self, job_id: &str, pid: i64) -> Result<()> {
        self.with_write(|conn| crate::runstore::set_pid(conn, job_id, pid))
    }
    pub fn run_set_pi_session(
        &self,
        job_id: &str,
        session_id: &str,
        session_path: &str,
    ) -> Result<()> {
        self.with_write(|conn| {
            crate::runstore::set_pi_session(conn, job_id, session_id, session_path)
        })
    }
    pub fn run_inc_corrections(&self, job_id: &str) -> Result<i64> {
        self.with_write(|conn| crate::runstore::inc_corrections(conn, job_id))
    }

    // step_signals ----------------------------------------------------------

    /// `step.Emit` — used by the agent-side `step next` path (and the
    /// behavioural-parity harness to simulate the agent).
    pub fn signal_emit(
        &self,
        task_id: &str,
        target: &str,
    ) -> std::result::Result<crate::signals::Emitted, crate::signals::SignalError> {
        // with_read/with_write require a Result<_, Error>; emit returns its own
        // error type, so run the closure directly under the writer mutex.
        let writer = self
            .writer
            .lock()
            .map_err(|_| crate::signals::SignalError::Core(Error::LockPoisoned("writer")))?;
        crate::signals::emit(&writer, task_id, target)
    }

    /// `step.GetForRun`.
    pub fn signal_for_run(
        &self,
        run_id: &str,
    ) -> std::result::Result<crate::signals::Emitted, crate::signals::SignalError> {
        let writer = self
            .writer
            .lock()
            .map_err(|_| crate::signals::SignalError::Core(Error::LockPoisoned("writer")))?;
        crate::signals::get_for_run(&writer, run_id)
    }

    // workflow reads --------------------------------------------------------

    pub fn wf_find_step_by_id(&self, step_id: &str) -> Result<crate::wfengine::Step> {
        self.with_write(|conn| crate::wfengine::find_step_by_id(conn, step_id))
    }
    pub fn wf_find_step_by_name(
        &self,
        workflow_id: &str,
        name: &str,
    ) -> Result<crate::wfengine::Step> {
        self.with_write(|conn| crate::wfengine::find_step_by_name(conn, workflow_id, name))
    }
    pub fn wf_get_by_id(&self, workflow_id: &str) -> Result<crate::wfengine::WorkflowMeta> {
        self.with_write(|conn| crate::wfengine::get_workflow_by_id(conn, workflow_id))
    }

    // comments --------------------------------------------------------------

    pub fn comments_render_for_prompt(&self, task_id: &str) -> Result<Vec<String>> {
        self.with_write(|conn| crate::read::render_comments_for_prompt(conn, task_id))
    }
}

impl crate::wfengine::TaskWriter for Db {
    fn get_task(&self, id: &str) -> Result<crate::tasks::Task> {
        self.task_get_row(id)
    }
    fn update_task(&self, id: &str, p: &crate::tasks::TaskPatch) -> Result<crate::tasks::Task> {
        self.task_update(id, p)
    }
    fn update_metadata_and_patch(
        &self,
        id: &str,
        f: &mut dyn FnMut(&mut serde_json::Map<String, serde_json::Value>) -> Result<()>,
        p: &crate::tasks::TaskPatch,
    ) -> Result<crate::tasks::Task> {
        self.with_write(|conn| crate::tasks::update_metadata_and_patch(conn, id, |m| f(m), p))
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
