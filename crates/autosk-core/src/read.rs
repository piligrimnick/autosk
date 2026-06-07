//! Read paths (plan §5) — the Rust port of the `internal/lazy/datasource`
//! offline read surface, projecting doltlite rows into `autosk-proto` wire
//! types. Every function takes a borrowed `&Connection` and is invoked by `Db`
//! under the GC read guard.
//!
//! Behavioural parity notes (mirrored verbatim from Go so behaviour cannot
//! drift, see the task's kickoff comment):
//!   * `is_blocked` queries `b.status IN ('new','claimed')` — the legacy
//!     `claimed` status that no longer exists in the enum — exactly as
//!     `store.IsBlocked` does, while `task_ready` uses `IN ('new','work',
//!     'human')` exactly as `store.Ready` does.
//!   * `task_list` orders by `priority ASC, created_at ASC`; `job_list` orders
//!     by `created_at DESC, job_id DESC`; comments by `created_at ASC, id ASC`.
//!
//! Two deliberate contract differences from the Go `Offline` source (recorded
//! here so they read as intentional, not as porting bugs — review R3/R4):
//!
//!   * **Enrichment errors fail the call (fail-fast), unlike Go Offline which
//!     degrades.** Go's `projectTask` / `projectWorkflow` / `Workflows`
//!     deliberately swallow per-row sub-query failures (`if ...; err == nil`,
//!     early-return-leaving-zero, `continue`) so one bad row never blanks a
//!     panel — a defence against the cross-process GC race in the Go driver.
//!     Here every sub-query propagates with `?`, so a transient enrichment
//!     error fails the whole `task.list`/`workflow.list` call. This is
//!     intentional: autoskd is the **sole** DB owner and the RwLock GC
//!     discipline (see [`crate::store`]) removes that race, so surfacing the
//!     error is preferable to silently returning a half-enriched row.
//!
//!   * **Agent package metadata is a known Phase-1 limitation.** [`agent_list`]
//!     resolves non-human agents to `source="db_only"` with empty
//!     version/model/thinking/extra_args/pi_ext/pi_skills. The Go `autosk lazy`
//!     offline path constructs `Offline` WITH a real `pkgregistry`, so an
//!     INSTALLED agent there reports `source="installed"` + its package
//!     metadata. Porting `pkgregistry` (npm package manifest resolution off
//!     disk) is out of scope for the read core; under `--rpc` an installed
//!     agent renders as `db_only` with blank metadata until a later phase wires
//!     registry resolution into autoskd. This is the only deliberate divergence
//!     from the "equal to the Go Offline contract" acceptance bar.

use rusqlite::{params, params_from_iter, Connection, OptionalExtension, Row};
use serde_json::Value;

use crate::error::{Error, Result};
use crate::timefmt::rfc3339_utc;
use autosk_proto::wire;

/// Open statuses — the `task.list` default (mirrors `store.OpenStatuses`).
const OPEN_STATUSES: [&str; 3] = ["new", "work", "human"];

/// Narrows `task.list` (mirrors `datasource.TaskFilter`). Empty strings mean
/// "no filter on that axis".
#[derive(Debug, Default, Clone)]
pub struct TaskFilter {
    /// `None` → open statuses; `Some(empty)` → all; `Some(list)` → those.
    pub statuses: Option<Vec<String>>,
    pub priority: Option<i64>,
    pub workflow_id: String,
    pub agent_name: String,
    pub author_name: String,
    pub step_agent_name: String,
    pub search: String,
}

/// Narrows `job.list` (mirrors `datasource.JobFilter`).
#[derive(Debug, Default, Clone)]
pub struct JobFilter {
    pub task_id: String,
    pub workflow_id: String,
    pub statuses: Vec<String>,
    pub limit: i64,
}

// ---- tasks ----------------------------------------------------------------

/// A narrow `tasks` row before enrichment.
struct RawTask {
    id: String,
    title: String,
    description: String,
    status: String,
    priority: i64,
    author_id: String,
    workflow_id: String,
    current_step_id: String,
    metadata: Option<Value>,
    created_at: i64,
    updated_at: i64,
}

const RAW_TASK_COLS: &str = "id, title, description, status, priority, \
     author_id, workflow_id, current_step_id, metadata, created_at, updated_at";

fn scan_raw_task(row: &Row) -> rusqlite::Result<RawTask> {
    let meta_raw: Option<String> = row.get(8)?;
    Ok(RawTask {
        id: row.get(0)?,
        title: row.get(1)?,
        description: row.get(2)?,
        status: row.get(3)?,
        priority: row.get(4)?,
        author_id: row.get::<_, Option<String>>(5)?.unwrap_or_default(),
        workflow_id: row.get::<_, Option<String>>(6)?.unwrap_or_default(),
        current_step_id: row.get::<_, Option<String>>(7)?.unwrap_or_default(),
        metadata: parse_metadata(meta_raw),
        created_at: row.get(9)?,
        updated_at: row.get(10)?,
    })
}

/// Parses `tasks.metadata`; NULL / empty / whitespace → `None`.
fn parse_metadata(raw: Option<String>) -> Option<Value> {
    let s = raw?;
    if s.trim().is_empty() {
        return None;
    }
    serde_json::from_str::<Value>(&s).ok()
}

/// `task.list` — mirrors `datasource.Offline.Tasks`.
pub fn task_list(conn: &Connection, f: &TaskFilter) -> Result<Vec<wire::TaskView>> {
    // Status default: None → open statuses; Some(empty) → all; Some(v) → v.
    let statuses: Vec<String> = match &f.statuses {
        None => OPEN_STATUSES.iter().map(|s| s.to_string()).collect(),
        Some(v) => v.clone(),
    };

    let mut where_clauses: Vec<String> = Vec::new();
    let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
    if !statuses.is_empty() {
        let ph = vec!["?"; statuses.len()].join(",");
        where_clauses.push(format!("status IN ({ph})"));
        for s in &statuses {
            args.push(Box::new(s.clone()));
        }
    }
    if let Some(p) = f.priority {
        where_clauses.push("priority = ?".to_string());
        args.push(Box::new(p));
    }
    let mut q = format!("SELECT {RAW_TASK_COLS} FROM tasks");
    if !where_clauses.is_empty() {
        q.push_str(" WHERE ");
        q.push_str(&where_clauses.join(" AND "));
    }
    q.push_str(" ORDER BY priority ASC, created_at ASC");

    let raws = scan_raw_tasks(conn, &q, args)?;
    let mut out = Vec::with_capacity(raws.len());
    for r in raws {
        let t = project_task(conn, r)?;
        if !f.workflow_id.is_empty() && t.workflow_id != f.workflow_id {
            continue;
        }
        if !f.agent_name.is_empty()
            && !eq_fold(&t.author_name, &f.agent_name)
            && !eq_fold(&t.agent_name, &f.agent_name)
        {
            continue;
        }
        if !f.author_name.is_empty() && !eq_fold(&t.author_name, &f.author_name) {
            continue;
        }
        if !f.step_agent_name.is_empty() && !eq_fold(&t.agent_name, &f.step_agent_name) {
            continue;
        }
        if !f.search.is_empty() {
            let needle = f.search.to_lowercase();
            if !t.id.to_lowercase().contains(&needle) && !t.title.to_lowercase().contains(&needle) {
                continue;
            }
        }
        out.push(t);
    }
    Ok(out)
}

/// `task.get` — mirrors `datasource.Offline.GetTask`.
pub fn task_get(conn: &Connection, id: &str) -> Result<wire::TaskView> {
    let raw = get_raw_task(conn, id)?.ok_or(Error::NotFound)?;
    project_task(conn, raw)
}

/// `task.ready` — `store.Ready` (status='new', no open blocker) + enrichment.
pub fn task_ready(conn: &Connection, limit: i64) -> Result<Vec<wire::TaskView>> {
    let mut q = format!(
        "SELECT {RAW_TASK_COLS} FROM tasks t \
         WHERE t.status = 'new' \
           AND NOT EXISTS ( \
               SELECT 1 FROM task_deps d \
                 JOIN tasks b ON b.id = d.blocker_id \
                WHERE d.blocked_id = t.id \
                  AND b.status IN ('new','work','human')) \
         ORDER BY t.priority ASC, t.created_at ASC"
    );
    let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
    if limit > 0 {
        q.push_str(" LIMIT ?");
        args.push(Box::new(limit));
    }
    let raws = scan_raw_tasks(conn, &q, args)?;
    let mut out = Vec::with_capacity(raws.len());
    for r in raws {
        out.push(project_task(conn, r)?);
    }
    Ok(out)
}

fn scan_raw_tasks(
    conn: &Connection,
    q: &str,
    args: Vec<Box<dyn rusqlite::ToSql>>,
) -> Result<Vec<RawTask>> {
    let mut stmt = conn.prepare(q)?;
    let rows = stmt.query_map(
        params_from_iter(args.iter().map(|b| b.as_ref())),
        scan_raw_task,
    )?;
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

fn get_raw_task(conn: &Connection, id: &str) -> Result<Option<RawTask>> {
    let q = format!("SELECT {RAW_TASK_COLS} FROM tasks WHERE id = ?1");
    let r = conn.query_row(&q, params![id], scan_raw_task).optional()?;
    Ok(r)
}

/// Enriches a raw task into the wire view (mirrors `Offline.projectTask`).
fn project_task(conn: &Connection, r: RawTask) -> Result<wire::TaskView> {
    let author_name = if r.author_id.is_empty() {
        String::new()
    } else {
        opt_string(conn, "SELECT name FROM agents WHERE id = ?1", &r.author_id)?.unwrap_or_default()
    };
    let workflow_name = if r.workflow_id.is_empty() {
        String::new()
    } else {
        opt_string(
            conn,
            "SELECT name FROM workflows WHERE id = ?1",
            &r.workflow_id,
        )?
        .unwrap_or_default()
    };
    let (step_name, agent_name) = if r.current_step_id.is_empty() {
        (String::new(), String::new())
    } else {
        step_label(conn, &r.current_step_id)?.unwrap_or_default()
    };
    let blocked = is_blocked(conn, &r.id)?;
    let (incoming, outgoing) = deps(conn, &r.id)?;
    let blocked_by = resolve_task_refs(conn, &incoming)?;
    let blocks = resolve_task_refs(conn, &outgoing)?;
    let comment_count: i64 = conn.query_row(
        "SELECT COUNT(*) FROM comments WHERE task_id = ?1",
        params![r.id],
        |row| row.get(0),
    )?;
    Ok(wire::TaskView {
        id: r.id,
        title: r.title,
        description: r.description,
        status: r.status,
        priority: r.priority,
        author_id: r.author_id,
        author_name,
        workflow_id: r.workflow_id,
        workflow_name,
        current_step_id: r.current_step_id,
        step_name,
        agent_name,
        blocked,
        blocked_by,
        blocks,
        comment_count,
        metadata: r.metadata,
        created_at: rfc3339_utc(r.created_at),
        updated_at: rfc3339_utc(r.updated_at),
    })
}

/// `store.IsBlocked`: open blocker in `{new, claimed}` (legacy enum, mirrored
/// verbatim — see module note).
fn is_blocked(conn: &Connection, id: &str) -> Result<bool> {
    let found: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM task_deps d \
               JOIN tasks b ON b.id = d.blocker_id \
              WHERE d.blocked_id = ?1 AND d.kind = 'blocks' \
                AND b.status IN ('new','claimed') LIMIT 1",
            params![id],
            |row| row.get(0),
        )
        .optional()?;
    Ok(found.is_some())
}

/// `store.Deps`: (incoming blockers, outgoing blocked), each id-sorted.
fn deps(conn: &Connection, id: &str) -> Result<(Vec<String>, Vec<String>)> {
    let incoming = query_ids(
        conn,
        "SELECT blocker_id FROM task_deps WHERE blocked_id = ?1 AND kind='blocks' ORDER BY blocker_id",
        id,
    )?;
    let outgoing = query_ids(
        conn,
        "SELECT blocked_id FROM task_deps WHERE blocker_id = ?1 AND kind='blocks' ORDER BY blocked_id",
        id,
    )?;
    Ok((incoming, outgoing))
}

fn resolve_task_refs(conn: &Connection, ids: &[String]) -> Result<Vec<wire::TaskRef>> {
    let mut out = Vec::with_capacity(ids.len());
    for id in ids {
        let status =
            opt_string(conn, "SELECT status FROM tasks WHERE id = ?1", id)?.unwrap_or_default();
        out.push(wire::TaskRef {
            id: id.clone(),
            status,
        });
    }
    Ok(out)
}

/// Resolves a step id to `(step_name, agent_name)` (mirrors `FindStepByID` for
/// the two fields `projectTask` needs).
fn step_label(conn: &Connection, step_id: &str) -> Result<Option<(String, String)>> {
    let r = conn
        .query_row(
            "SELECT s.name, a.name FROM steps s JOIN agents a ON s.agent_id = a.id WHERE s.id = ?1",
            params![step_id],
            |row| Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?)),
        )
        .optional()?;
    Ok(r)
}

// ---- comments -------------------------------------------------------------

/// `comment.list` — a task's thread, oldest first (mirrors `comments.ListByTask`).
pub fn comment_list(conn: &Connection, task_id: &str) -> Result<Vec<wire::Comment>> {
    let mut stmt = conn.prepare(
        "SELECT c.id, c.task_id, c.author_id, a.name, c.text, c.created_at \
           FROM comments c JOIN agents a ON c.author_id = a.id \
          WHERE c.task_id = ?1 ORDER BY c.created_at ASC, c.id ASC",
    )?;
    let rows = stmt.query_map(params![task_id], |row| {
        Ok(wire::Comment {
            id: row.get(0)?,
            task_id: row.get(1)?,
            author_id: row.get(2)?,
            author_name: row.get(3)?,
            text: row.get(4)?,
            created_at: rfc3339_utc(row.get::<_, i64>(5)?),
        })
    })?;
    collect(rows)
}

// ---- agents ---------------------------------------------------------------

/// `agent.list` — agents + tasks-owned counts. Human → `source="builtin"`;
/// every other agent → `source="db_only"` with empty package metadata.
///
/// This matches `Offline.Agents` only when its `pkgregistry` is nil. `autosk
/// lazy` passes a real registry, so installed agents differ under `--rpc` — a
/// known Phase-1 limitation documented at the module level (review R3).
pub fn agent_list(conn: &Connection) -> Result<Vec<wire::Agent>> {
    let mut stmt = conn.prepare("SELECT id, name, is_human FROM agents ORDER BY name ASC")?;
    let base = stmt.query_map([], |row| {
        let is_human: i64 = row.get(2)?;
        Ok((
            row.get::<_, String>(0)?,
            row.get::<_, String>(1)?,
            is_human != 0,
        ))
    })?;
    let mut agents: Vec<(String, String, bool)> = Vec::new();
    for r in base {
        agents.push(r?);
    }
    let mut out = Vec::with_capacity(agents.len());
    for (id, name, is_human) in agents {
        let tasks_owned: i64 = conn.query_row(
            "SELECT COUNT(*) FROM tasks t \
               LEFT JOIN steps s ON s.id = t.current_step_id \
              WHERE t.author_id = ?1 OR s.agent_id = ?1",
            params![id],
            |row| row.get(0),
        )?;
        out.push(wire::Agent {
            id,
            name,
            is_human,
            source: if is_human { "builtin" } else { "db_only" }.to_string(),
            version: String::new(),
            model: String::new(),
            thinking: String::new(),
            extra_args: Vec::new(),
            pi_skills: Vec::new(),
            pi_ext: Vec::new(),
            tasks_owned,
        });
    }
    Ok(out)
}

// ---- workflows ------------------------------------------------------------

/// `workflow.list` — mirrors `Offline.Workflows`.
pub fn workflow_list(conn: &Connection, include_synthetic: bool) -> Result<Vec<wire::Workflow>> {
    let mut q = String::from(
        "SELECT id, name, description, first_step_id, is_synthetic, isolation FROM workflows",
    );
    if !include_synthetic {
        q.push_str(" WHERE is_synthetic = 0");
    }
    q.push_str(" ORDER BY name ASC");
    let mut stmt = conn.prepare(&q)?;
    let rows = stmt.query_map([], scan_workflow_head)?;
    let mut heads = Vec::new();
    for r in rows {
        heads.push(r?);
    }
    let mut out = Vec::with_capacity(heads.len());
    for h in heads {
        out.push(project_workflow(conn, h)?);
    }
    Ok(out)
}

/// `workflow.get` — one workflow by name (mirrors `workflow.GetByName` + the
/// datasource projection).
pub fn workflow_get(conn: &Connection, name: &str) -> Result<wire::Workflow> {
    let head = conn
        .query_row(
            "SELECT id, name, description, first_step_id, is_synthetic, isolation \
               FROM workflows WHERE name = ?1",
            params![name],
            scan_workflow_head,
        )
        .optional()?
        .ok_or(Error::NotFound)?;
    project_workflow(conn, head)
}

struct WorkflowHead {
    id: String,
    name: String,
    description: String,
    first_step_id: String,
    is_synthetic: bool,
    isolation: String,
}

fn scan_workflow_head(row: &Row) -> rusqlite::Result<WorkflowHead> {
    let synth: i64 = row.get(4)?;
    let iso: Option<String> = row.get(5)?;
    Ok(WorkflowHead {
        id: row.get(0)?,
        name: row.get(1)?,
        description: row.get(2)?,
        first_step_id: row.get(3)?,
        is_synthetic: synth != 0,
        isolation: normalize_isolation(iso),
    })
}

/// Mirrors `workflow.IsolationMode.Normalize`: empty/unknown → "none".
fn normalize_isolation(iso: Option<String>) -> String {
    match iso {
        Some(s) if s.trim() == "worktree" => "worktree".to_string(),
        _ => "none".to_string(),
    }
}

fn project_workflow(conn: &Connection, h: WorkflowHead) -> Result<wire::Workflow> {
    // Steps ordered by seq, joined to agent name.
    let mut stmt = conn.prepare(
        "SELECT s.id, s.name, a.name FROM steps s JOIN agents a ON s.agent_id = a.id \
          WHERE s.workflow_id = ?1 ORDER BY s.seq ASC",
    )?;
    let step_rows = stmt.query_map(params![h.id], |row| {
        Ok((
            row.get::<_, String>(0)?,
            row.get::<_, String>(1)?,
            row.get::<_, String>(2)?,
        ))
    })?;
    let mut raw_steps: Vec<(String, String, String)> = Vec::new();
    for r in step_rows {
        raw_steps.push(r?);
    }

    let mut step_names: std::collections::HashMap<String, String> =
        std::collections::HashMap::new();
    let mut first_step = String::new();
    for (sid, sname, _) in &raw_steps {
        step_names.insert(sid.clone(), sname.clone());
        if *sid == h.first_step_id {
            first_step = sname.clone();
        }
    }

    let mut steps = Vec::with_capacity(raw_steps.len());
    let mut total_task_count: i64 = 0;
    for (sid, sname, agent_name) in &raw_steps {
        let (next_steps, next_status) = step_transitions(conn, sid)?;
        let task_count: i64 = conn.query_row(
            "SELECT COUNT(*) FROM tasks WHERE current_step_id = ?1",
            params![sid],
            |row| row.get(0),
        )?;
        total_task_count += task_count;
        steps.push(wire::WorkflowStep {
            id: sid.clone(),
            name: sname.clone(),
            agent_name: agent_name.clone(),
            next_steps,
            next_status,
            task_count,
        });
    }

    let (non_terminal_count, non_terminal_tasks) = non_terminal_sample(conn, &h.id, &step_names)?;

    Ok(wire::Workflow {
        id: h.id,
        name: h.name,
        description: h.description,
        is_synthetic: h.is_synthetic,
        first_step,
        steps,
        task_count: total_task_count,
        isolation: h.isolation,
        non_terminal_task_count: non_terminal_count,
        non_terminal_tasks,
    })
}

/// Returns `(next_steps, next_status)` for a step, transitions ordered by id
/// (mirrors `loadTransitions` + the datasource split).
fn step_transitions(conn: &Connection, step_id: &str) -> Result<(Vec<String>, Vec<String>)> {
    let mut stmt = conn.prepare(
        "SELECT t.task_status, (SELECT name FROM steps WHERE id = t.next_step_id) AS next_name \
           FROM step_transitions t WHERE t.step_id = ?1 ORDER BY t.id ASC",
    )?;
    let rows = stmt.query_map(params![step_id], |row| {
        Ok((
            row.get::<_, Option<String>>(0)?.unwrap_or_default(),
            row.get::<_, Option<String>>(1)?.unwrap_or_default(),
        ))
    })?;
    let mut next_steps = Vec::new();
    let mut next_status = Vec::new();
    for r in rows {
        let (status, next_name) = r?;
        if !status.is_empty() {
            next_status.push(status);
        } else if !next_name.is_empty() {
            next_steps.push(next_name);
        }
    }
    Ok((next_steps, next_status))
}

/// Mirrors `loadNonTerminalSample`: total count + first
/// `NonTerminalTaskSampleSize` (10) rows by id ASC.
fn non_terminal_sample(
    conn: &Connection,
    workflow_id: &str,
    step_names: &std::collections::HashMap<String, String>,
) -> Result<(i64, Vec<wire::NonTerminalTaskRef>)> {
    const SAMPLE_SIZE: usize = 10;
    let mut stmt = conn.prepare(
        "SELECT id, status, COALESCE(current_step_id, '') FROM tasks \
          WHERE workflow_id = ?1 AND status IN ('new','work','human') ORDER BY id ASC",
    )?;
    let rows = stmt.query_map(params![workflow_id], |row| {
        Ok((
            row.get::<_, String>(0)?,
            row.get::<_, String>(1)?,
            row.get::<_, String>(2)?,
        ))
    })?;
    let mut total: i64 = 0;
    let mut sample = Vec::new();
    for r in rows {
        let (id, status, step_id) = r?;
        total += 1;
        if sample.len() < SAMPLE_SIZE {
            sample.push(wire::NonTerminalTaskRef {
                id,
                status,
                step_name: step_names.get(&step_id).cloned().unwrap_or_default(),
            });
        }
    }
    Ok((total, sample))
}

// ---- jobs -----------------------------------------------------------------

struct RawRun {
    job_id: String,
    task_id: String,
    step_id: String,
    status: String,
    transition_id: Option<i64>,
    exit_code: Option<i64>,
    pid: Option<i64>,
    pi_session_id: String,
    session_path: String,
    error: String,
    max_corrections: i64,
    corrections_used: i64,
    created_at: i64,
    started_at: Option<i64>,
    finished_at: Option<i64>,
}

const RAW_RUN_COLS: &str = "job_id, task_id, step_id, status, transition_id, \
     exit_code, pid, pi_session_id, session_path, error, \
     max_corrections, corrections_used, created_at, started_at, finished_at";

fn scan_raw_run(row: &Row) -> rusqlite::Result<RawRun> {
    Ok(RawRun {
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

/// One decorated label row (mirrors `Offline.lookupStepLabels`).
#[derive(Default, Clone)]
struct StepLabel {
    workflow_id: String,
    workflow_name: String,
    step_name: String,
    agent_name: String,
}

/// `job.list` — mirrors `Offline.Jobs` (decorate + workflow_id filter).
pub fn job_list(conn: &Connection, f: &JobFilter) -> Result<Vec<wire::Job>> {
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
    let mut q = format!("SELECT {RAW_RUN_COLS} FROM daemon_runs");
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
        params_from_iter(args.iter().map(|b| b.as_ref())),
        scan_raw_run,
    )?;
    let mut runs = Vec::new();
    for r in rows {
        runs.push(r?);
    }

    let step_ids: Vec<String> = runs
        .iter()
        .filter(|r| !r.step_id.is_empty())
        .map(|r| r.step_id.clone())
        .collect();
    let labels = lookup_step_labels(conn, &step_ids)?;

    let mut out = Vec::with_capacity(runs.len());
    for r in runs {
        let label = if r.step_id.is_empty() {
            None
        } else {
            labels.get(&r.step_id).cloned()
        };
        if !f.workflow_id.is_empty() {
            match &label {
                Some(l) if l.workflow_id == f.workflow_id => {}
                _ => continue,
            }
        }
        out.push(run_to_job(r, label.unwrap_or_default()));
    }
    Ok(out)
}

/// `job.get` — mirrors `Offline.GetJob`.
pub fn job_get(conn: &Connection, id: &str) -> Result<wire::Job> {
    let q = format!("SELECT {RAW_RUN_COLS} FROM daemon_runs WHERE job_id = ?1");
    let run = conn
        .query_row(&q, params![id], scan_raw_run)
        .optional()?
        .ok_or(Error::NotFound)?;
    let label = if run.step_id.is_empty() {
        StepLabel::default()
    } else {
        lookup_step_labels(conn, std::slice::from_ref(&run.step_id))?
            .get(&run.step_id)
            .cloned()
            .unwrap_or_default()
    };
    Ok(run_to_job(run, label))
}

/// Returns a run's `session_path` for `job.messages`, or `None` if unset;
/// errors with [`Error::NotFound`] when the run is unknown.
pub fn job_session_path(conn: &Connection, job_id: &str) -> Result<Option<String>> {
    let sp = conn
        .query_row(
            "SELECT session_path FROM daemon_runs WHERE job_id = ?1",
            params![job_id],
            |row| row.get::<_, Option<String>>(0),
        )
        .optional()?
        .ok_or(Error::NotFound)?;
    Ok(sp.filter(|s| !s.is_empty()))
}

fn lookup_step_labels(
    conn: &Connection,
    step_ids: &[String],
) -> Result<std::collections::HashMap<String, StepLabel>> {
    let mut out = std::collections::HashMap::new();
    if step_ids.is_empty() {
        return Ok(out);
    }
    let ph = vec!["?"; step_ids.len()].join(",");
    let q = format!(
        "SELECT s.id, s.name, COALESCE(a.name, ''), s.workflow_id, COALESCE(w.name, '') \
           FROM steps s \
           LEFT JOIN agents a ON a.id = s.agent_id \
           LEFT JOIN workflows w ON w.id = s.workflow_id \
          WHERE s.id IN ({ph})"
    );
    let mut stmt = conn.prepare(&q)?;
    let rows = stmt.query_map(params_from_iter(step_ids.iter()), |row| {
        Ok((
            row.get::<_, String>(0)?,
            StepLabel {
                step_name: row.get(1)?,
                agent_name: row.get(2)?,
                workflow_id: row.get(3)?,
                workflow_name: row.get(4)?,
            },
        ))
    })?;
    for r in rows {
        let (id, l) = r?;
        out.insert(id, l);
    }
    Ok(out)
}

/// Maps a run row + labels to the wire Job (mirrors `api.FromRun` + datasource
/// label decoration; archive-mode `attach_count`/`streaming` are 0/false).
fn run_to_job(r: RawRun, label: StepLabel) -> wire::Job {
    let duration_ms = match (r.started_at, r.finished_at) {
        (Some(s), Some(f)) => (f - s) * 1000,
        _ => 0,
    };
    wire::Job {
        job_id: r.job_id,
        task_id: r.task_id,
        step_id: r.step_id,
        status: r.status,
        transition_id: r.transition_id,
        pi_session_id: r.pi_session_id,
        session_path: r.session_path,
        pid: r.pid,
        exit_code: r.exit_code,
        error: r.error,
        corrections_used: r.corrections_used,
        max_corrections: r.max_corrections,
        created_at: rfc3339_utc(r.created_at),
        started_at: r.started_at.map(rfc3339_utc),
        finished_at: r.finished_at.map(rfc3339_utc),
        duration_ms,
        attach_count: 0,
        streaming: false,
        workflow_name: label.workflow_name,
        step_name: label.step_name,
        agent_name: label.agent_name,
    }
}

// ---- signals --------------------------------------------------------------

/// Shared projection for the two signal verbs (mirrors `signalsBaseQuery`).
const SIGNALS_BASE_QUERY: &str = "\
    SELECT ss.transition_id, ss.task_id, ss.run_id, ss.created_at, \
           dr.step_id, st.name, \
           st.workflow_id, w.name, \
           COALESCE(t.next_step_id, ''), COALESCE(t.task_status, ''), \
           COALESCE(ns.name, ''), \
           st.agent_id, a.name \
      FROM step_signals ss \
      JOIN daemon_runs dr      ON dr.job_id = ss.run_id \
      JOIN steps st            ON st.id = dr.step_id \
      JOIN workflows w         ON w.id = st.workflow_id \
      JOIN agents a            ON a.id = st.agent_id \
      LEFT JOIN step_transitions t  ON t.id = ss.transition_id \
      LEFT JOIN steps ns       ON ns.id = t.next_step_id";

/// `signal.forTask` — every step_signals row for a task, newest first.
pub fn signal_for_task(conn: &Connection, task_id: &str) -> Result<Vec<wire::Signal>> {
    let q = format!(
        "{SIGNALS_BASE_QUERY} WHERE ss.task_id = ?1 ORDER BY ss.created_at DESC, ss.transition_id DESC"
    );
    scan_signals(conn, &q, task_id)
}

/// `signal.forJob` — step_signals rows for one run, newest first.
pub fn signal_for_job(conn: &Connection, job_id: &str) -> Result<Vec<wire::Signal>> {
    let q = format!(
        "{SIGNALS_BASE_QUERY} WHERE ss.run_id = ?1 ORDER BY ss.created_at DESC, ss.transition_id DESC"
    );
    scan_signals(conn, &q, job_id)
}

fn scan_signals(conn: &Connection, q: &str, arg: &str) -> Result<Vec<wire::Signal>> {
    let mut stmt = conn.prepare(q)?;
    let rows = stmt.query_map(params![arg], |row| {
        let created: i64 = row.get(3)?;
        let status: String = row.get(9)?;
        let next_name: String = row.get(10)?;
        let target = if !status.is_empty() {
            status
        } else if !next_name.is_empty() {
            next_name
        } else {
            "(unknown)".to_string()
        };
        Ok(wire::Signal {
            transition_id: row.get(0)?,
            task_id: row.get(1)?,
            job_id: row.get(2)?,
            created_at: rfc3339_utc(created),
            step_id: row.get(4)?,
            step_name: row.get(5)?,
            workflow_id: row.get(6)?,
            workflow_name: row.get(7)?,
            target,
            agent_id: row.get(11)?,
            agent_name: row.get(12)?,
        })
    })?;
    collect(rows)
}

// ---- small helpers --------------------------------------------------------

fn opt_string(conn: &Connection, q: &str, arg: &str) -> Result<Option<String>> {
    Ok(conn
        .query_row(q, params![arg], |row| row.get::<_, String>(0))
        .optional()?)
}

fn query_ids(conn: &Connection, q: &str, arg: &str) -> Result<Vec<String>> {
    let mut stmt = conn.prepare(q)?;
    let rows = stmt.query_map(params![arg], |row| row.get::<_, String>(0))?;
    collect(rows)
}

fn collect<T>(rows: impl Iterator<Item = rusqlite::Result<T>>) -> Result<Vec<T>> {
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

/// Case-insensitive equality (mirrors Go `strings.EqualFold` for ASCII).
fn eq_fold(a: &str, b: &str) -> bool {
    a.eq_ignore_ascii_case(b)
}
