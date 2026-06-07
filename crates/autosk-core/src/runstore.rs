//! `daemon_runs` CRUD — the Rust port of `internal/daemon/runstore`.
//!
//! One row per STEP execution; `step_id` is always set (single-agent runs
//! go through the synthetic `single:<agent>` workflow). Functions take a
//! borrowed `&Connection`; [`crate::store::Db`] wraps them on the single
//! writer connection.

use rusqlite::{params, Connection, OptionalExtension, Row};

use crate::error::{Error, Result};

/// Lifecycle state of a job (mirrors the SQL CHECK enum + `RunStatus`).
pub const ST_QUEUED: &str = "queued";
pub const ST_RUNNING: &str = "running";
pub const ST_DONE: &str = "done";
pub const ST_FAILED: &str = "failed";
pub const ST_CANCEL: &str = "cancel";

/// `job-` id prefix; 3 random bytes → 6 hex chars (mirrors `IDPrefix`/`IDBytes`).
const ID_PREFIX: &str = "job";

/// Reports whether `s` is a sticky terminal status.
pub fn is_terminal(s: &str) -> bool {
    matches!(s, ST_DONE | ST_FAILED | ST_CANCEL)
}

/// In-memory `daemon_runs` row (mirror of `runstore.Run`).
#[derive(Debug, Clone)]
pub struct Run {
    pub job_id: String,
    pub task_id: String,
    pub step_id: String,
    pub status: String,
    pub transition_id: Option<i64>,
    pub exit_code: Option<i64>,
    pub pid: Option<i64>,
    pub pi_session_id: String,
    pub session_path: String,
    pub error: String,
    pub max_corrections: i64,
    pub corrections_used: i64,
    pub created_at: i64,
    pub started_at: Option<i64>,
    pub finished_at: Option<i64>,
}

/// Input for [`create_run`] (mirror of `runstore.NewRun`).
#[derive(Debug, Default, Clone)]
pub struct NewRun {
    pub task_id: String,
    pub step_id: String,
    pub max_corrections: i64,
}

/// Narrows [`list_runs`] (mirror of `runstore.RunFilter`).
#[derive(Debug, Default, Clone)]
pub struct RunFilter {
    pub statuses: Vec<String>,
    pub task_id: String,
    pub limit: i64,
}

const RUN_COLS: &str = "job_id, task_id, step_id, status, transition_id, \
     exit_code, pid, pi_session_id, session_path, error, \
     max_corrections, corrections_used, created_at, started_at, finished_at";

fn scan_run(row: &Row) -> rusqlite::Result<Run> {
    Ok(Run {
        job_id: row.get(0)?,
        task_id: row.get(1)?,
        step_id: row.get(2)?,
        status: row.get(3)?,
        transition_id: row.get(4)?,
        exit_code: row.get(5)?,
        pid: row.get(6)?,
        pi_session_id: row.get::<_, Option<String>>(7)?.unwrap_or_default(),
        session_path: row.get::<_, Option<String>>(8)?.unwrap_or_default(),
        error: row.get::<_, Option<String>>(9)?.unwrap_or_default(),
        max_corrections: row.get(10)?,
        corrections_used: row.get(11)?,
        created_at: row.get(12)?,
        started_at: row.get(13)?,
        finished_at: row.get(14)?,
    })
}

/// Inserts a fresh `queued` run and returns the materialised [`Run`].
pub fn create_run(conn: &Connection, nr: &NewRun) -> Result<Run> {
    if nr.task_id.is_empty() {
        return Err(Error::Migration("create_run: task_id required".into()));
    }
    if nr.step_id.is_empty() {
        return Err(Error::Migration("create_run: step_id required".into()));
    }
    let job_id = new_job_id(conn)?;
    let now = crate::timefmt::now_unix();
    let max = if nr.max_corrections <= 0 {
        3
    } else {
        nr.max_corrections
    };
    conn.execute(
        "INSERT INTO daemon_runs(job_id, task_id, step_id, status, max_corrections, corrections_used, created_at) \
         VALUES (?1,?2,?3,'queued',?4,0,?5)",
        params![job_id, nr.task_id, nr.step_id, max, now],
    )?;
    get_run(conn, &job_id)
}

/// `GetRun` — the row, or [`Error::NotFound`].
pub fn get_run(conn: &Connection, job_id: &str) -> Result<Run> {
    let q = format!("SELECT {RUN_COLS} FROM daemon_runs WHERE job_id = ?1");
    conn.query_row(&q, params![job_id], scan_run)
        .optional()?
        .ok_or(Error::NotFound)
}

/// `ListRuns` — matching rows, ordered `created_at DESC, job_id DESC`.
pub fn list_runs(conn: &Connection, f: &RunFilter) -> Result<Vec<Run>> {
    let mut where_clauses: Vec<String> = Vec::new();
    let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
    if !f.statuses.is_empty() {
        let ph = vec!["?"; f.statuses.len()].join(",");
        where_clauses.push(format!("status IN ({ph})"));
        for s in &f.statuses {
            args.push(Box::new(s.clone()));
        }
    }
    if !f.task_id.is_empty() {
        where_clauses.push("task_id = ?".to_string());
        args.push(Box::new(f.task_id.clone()));
    }
    let mut q = format!("SELECT {RUN_COLS} FROM daemon_runs");
    if !where_clauses.is_empty() {
        q.push_str(" WHERE ");
        q.push_str(&where_clauses.join(" AND "));
    }
    q.push_str(" ORDER BY created_at DESC, job_id DESC");
    if f.limit > 0 {
        q.push_str(&format!(" LIMIT {}", f.limit));
    }
    let mut stmt = conn.prepare(&q)?;
    let rows = stmt.query_map(
        rusqlite::params_from_iter(args.iter().map(|b| b.as_ref())),
        scan_run,
    )?;
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

/// `MarkRunning` — `queued → running`, stamping `pid`/`started_at`.
/// RowsAffected==0 is fine (idempotent re-claim); returns the canonical state.
pub fn mark_running(conn: &Connection, job_id: &str, pid: i64) -> Result<Run> {
    let now = crate::timefmt::now_unix();
    conn.execute(
        "UPDATE daemon_runs SET status='running', pid=?1, started_at=?2 \
         WHERE job_id=?3 AND status='queued'",
        params![pid, now, job_id],
    )?;
    get_run(conn, job_id)
}

/// `MarkDone` — `running → done` with exit code + the agent-picked transition.
pub fn mark_done(
    conn: &Connection,
    job_id: &str,
    exit_code: i64,
    transition_id: Option<i64>,
) -> Result<Run> {
    mark_terminal(conn, job_id, ST_DONE, Some(exit_code), "", transition_id)
}

/// `MarkFailed` — `→ failed` with an error message.
pub fn mark_failed(
    conn: &Connection,
    job_id: &str,
    exit_code: Option<i64>,
    err_msg: &str,
) -> Result<Run> {
    mark_terminal(conn, job_id, ST_FAILED, exit_code, err_msg, None)
}

/// `MarkCancelled` — `→ cancel`.
pub fn mark_cancelled(conn: &Connection, job_id: &str, exit_code: Option<i64>) -> Result<Run> {
    mark_terminal(conn, job_id, ST_CANCEL, exit_code, "", None)
}

fn mark_terminal(
    conn: &Connection,
    job_id: &str,
    target: &str,
    exit_code: Option<i64>,
    err_msg: &str,
    transition_id: Option<i64>,
) -> Result<Run> {
    if !is_terminal(target) {
        return Err(Error::Migration(format!(
            "invalid terminal status {target}"
        )));
    }
    let now = crate::timefmt::now_unix();
    let n = conn.execute(
        "UPDATE daemon_runs \
           SET status        = ?1, \
               exit_code     = ?2, \
               error         = CASE WHEN ?3='' THEN error ELSE ?3 END, \
               transition_id = COALESCE(?4, transition_id), \
               finished_at   = ?5, \
               pid           = NULL \
         WHERE job_id = ?6",
        params![target, exit_code, err_msg, transition_id, now, job_id],
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    get_run(conn, job_id)
}

/// `SetPID` — updates the pid column.
pub fn set_pid(conn: &Connection, job_id: &str, pid: i64) -> Result<()> {
    let n = conn.execute(
        "UPDATE daemon_runs SET pid = ?1 WHERE job_id = ?2",
        params![pid, job_id],
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    Ok(())
}

/// `SetPISession` — records the pi session id + `session.jsonl` path.
pub fn set_pi_session(
    conn: &Connection,
    job_id: &str,
    session_id: &str,
    session_path: &str,
) -> Result<()> {
    let sid = if session_id.is_empty() {
        None
    } else {
        Some(session_id)
    };
    let sp = if session_path.is_empty() {
        None
    } else {
        Some(session_path)
    };
    let n = conn.execute(
        "UPDATE daemon_runs SET pi_session_id = ?1, session_path = ?2 WHERE job_id = ?3",
        params![sid, sp, job_id],
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    Ok(())
}

/// `IncCorrections` — atomically bumps `corrections_used`; returns the new value.
pub fn inc_corrections(conn: &Connection, job_id: &str) -> Result<i64> {
    let n = conn.execute(
        "UPDATE daemon_runs SET corrections_used = corrections_used + 1 WHERE job_id = ?1",
        params![job_id],
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    let used: i64 = conn.query_row(
        "SELECT corrections_used FROM daemon_runs WHERE job_id = ?1",
        params![job_id],
        |r| r.get(0),
    )?;
    Ok(used)
}

/// Mints a fresh unique `job-XXXXXX` id using SQLite's `randomblob` (no rand
/// crate needed). Mirrors `id.NewUniqueN(IDPrefix, 3, …)`.
fn new_job_id(conn: &Connection) -> Result<String> {
    for _ in 0..64 {
        let hex: String = conn.query_row("SELECT lower(hex(randomblob(3)))", [], |r| r.get(0))?;
        let id = format!("{ID_PREFIX}-{hex}");
        let taken: Option<i64> = conn
            .query_row(
                "SELECT 1 FROM daemon_runs WHERE job_id = ?1",
                params![id],
                |r| r.get(0),
            )
            .optional()?;
        if taken.is_none() {
            return Ok(id);
        }
    }
    Err(Error::Migration("job id space exhausted".into()))
}
