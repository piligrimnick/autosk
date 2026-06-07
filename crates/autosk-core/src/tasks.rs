//! Task rows + the executor-side write paths — the Rust port of the
//! `tasks`/`metadata`/`query` slices of `internal/store/doltlite` that the
//! daemon executor needs (get/update/update-metadata-and-patch). General
//! task CRUD verbs stay in Phase 3; this is only what the engine writes.
//!
//! Every function takes a borrowed `&Connection`; [`crate::store::Db`]
//! exposes thin wrappers that hold the single writer mutex for the call so
//! reads observe the executor's own writes (Go's `SetMaxOpenConns(1)`
//! read-your-writes model).

use rusqlite::{params, Connection, OptionalExtension, Row};
use serde_json::{Map, Value};

use crate::error::{Error, Result};

/// Open statuses — the engine's default task filter (mirrors
/// `store.OpenStatuses`).
pub const OPEN_STATUSES: [&str; 3] = ["new", "work", "human"];

/// Task status strings (mirrors `store.Status` constants, all ≤ 7 chars).
pub const STATUS_NEW: &str = "new";
pub const STATUS_WORK: &str = "work";
pub const STATUS_HUMAN: &str = "human";
pub const STATUS_DONE: &str = "done";
pub const STATUS_CANCEL: &str = "cancel";

const MIN_PRIORITY: i64 = 0;
const MAX_PRIORITY: i64 = 3;

fn status_valid(s: &str) -> bool {
    matches!(
        s,
        STATUS_NEW | STATUS_WORK | STATUS_HUMAN | STATUS_DONE | STATUS_CANCEL
    )
}

/// In-memory view of a `tasks` row, narrowed to the engine's needs
/// (mirror of `store.Task`). `metadata` is an empty map when the column is
/// SQL NULL.
#[derive(Debug, Clone, PartialEq)]
pub struct Task {
    pub id: String,
    pub title: String,
    pub description: String,
    pub status: String,
    pub priority: i64,
    pub author_id: String,
    pub workflow_id: String,
    pub current_step_id: String,
    pub metadata: Map<String, Value>,
    pub created_at: i64,
    pub updated_at: i64,
}

/// Sparse update (mirror of `store.TaskPatch`). `Some(x)` sets the column,
/// `None` leaves it unchanged. `metadata` is handled separately by
/// [`update_metadata_and_patch`] and is rejected here when set alongside it.
#[derive(Debug, Default, Clone)]
pub struct TaskPatch {
    pub title: Option<String>,
    pub description: Option<String>,
    pub status: Option<String>,
    pub priority: Option<i64>,
    pub workflow_id: Option<String>,
    pub current_step_id: Option<String>,
    pub metadata: Option<Map<String, Value>>,
}

impl TaskPatch {
    fn is_empty(&self) -> bool {
        self.title.is_none()
            && self.description.is_none()
            && self.status.is_none()
            && self.priority.is_none()
            && self.workflow_id.is_none()
            && self.current_step_id.is_none()
            && self.metadata.is_none()
    }
}

const TASK_COLS: &str = "id, title, description, status, priority, \
     author_id, workflow_id, current_step_id, metadata, created_at, updated_at";

fn scan_task(row: &Row) -> rusqlite::Result<Task> {
    let meta_raw: Option<String> = row.get(8)?;
    Ok(Task {
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

/// Parses `tasks.metadata`; NULL / empty / whitespace → empty map.
fn parse_metadata(raw: Option<String>) -> Map<String, Value> {
    let Some(s) = raw else {
        return Map::new();
    };
    if s.trim().is_empty() {
        return Map::new();
    }
    serde_json::from_str::<Map<String, Value>>(&s).unwrap_or_default()
}

/// Serialises a metadata map to the SQL argument: empty → NULL, mirroring
/// `marshalMetadata`.
fn marshal_metadata(m: &Map<String, Value>) -> Option<String> {
    if m.is_empty() {
        return None;
    }
    serde_json::to_string(m).ok()
}

/// `GetTask` — one row or [`Error::NotFound`].
pub fn get_task(conn: &Connection, id: &str) -> Result<Task> {
    let q = format!("SELECT {TASK_COLS} FROM tasks WHERE id = ?1");
    conn.query_row(&q, params![id], scan_task)
        .optional()?
        .ok_or(Error::NotFound)
}

/// Inserts a task. Used by the bootstrap/enroll path and tests; the id must
/// already be set (the engine never mints task ids). Mirrors `CreateTask`'s
/// invariant checks + stamping.
pub fn create_task(conn: &Connection, mut t: Task) -> Result<Task> {
    t.title = t.title.trim().to_string();
    if t.title.is_empty() {
        return Err(Error::Migration("task title is empty".into()));
    }
    if t.status.is_empty() {
        t.status = STATUS_NEW.to_string();
    }
    if !status_valid(&t.status) {
        return Err(Error::Migration(format!("invalid status {}", t.status)));
    }
    if t.priority < MIN_PRIORITY || t.priority > MAX_PRIORITY {
        return Err(Error::Migration(format!("invalid priority {}", t.priority)));
    }
    if t.id.is_empty() {
        return Err(Error::Migration("create_task: id required".into()));
    }
    let now = crate::timefmt::now_unix();
    t.created_at = now;
    t.updated_at = now;
    let meta_arg = marshal_metadata(&t.metadata);
    conn.execute(
        "INSERT INTO tasks(id, title, description, status, priority, \
             author_id, workflow_id, current_step_id, metadata, created_at, updated_at) \
         VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11)",
        params![
            t.id,
            t.title,
            t.description,
            t.status,
            t.priority,
            null_text(&t.author_id),
            null_text(&t.workflow_id),
            null_text(&t.current_step_id),
            meta_arg,
            now,
            now,
        ],
    )?;
    Ok(t)
}

/// `UpdateTask` — applies a sparse patch + bumps `updated_at`; returns the
/// re-read row. Mirrors the doltlite `UpdateTask`.
pub fn update_task(conn: &Connection, id: &str, p: &TaskPatch) -> Result<Task> {
    if p.is_empty() {
        return get_task(conn, id);
    }
    validate_patch(p)?;
    let (mut sets, mut args) = patch_sets_and_args(p);
    if let Some(m) = &p.metadata {
        sets.push("metadata = ?".to_string());
        args.push(Box::new(marshal_metadata(m)));
    }
    sets.push("updated_at = ?".to_string());
    args.push(Box::new(crate::timefmt::now_unix()));
    args.push(Box::new(id.to_string()));
    let q = format!("UPDATE tasks SET {} WHERE id = ?", sets.join(", "));
    let n = conn.execute(
        &q,
        rusqlite::params_from_iter(args.iter().map(|b| b.as_ref())),
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    get_task(conn, id)
}

/// `UpdateMetadataAndPatch` — reads `tasks.metadata`, hands `f` a mutable
/// copy, then writes the new metadata AND the patch in one transaction.
/// `p.metadata` must be `None`. Mirrors the doltlite engine helper used by
/// [`crate::wfengine::enter_step`]; `f` returning `Err` rolls the tx back.
pub fn update_metadata_and_patch<F>(
    conn: &Connection,
    id: &str,
    f: F,
    p: &TaskPatch,
) -> Result<Task>
where
    F: FnOnce(&mut Map<String, Value>) -> Result<()>,
{
    if p.metadata.is_some() {
        return Err(Error::Migration(
            "update_metadata_and_patch: patch.metadata must be None".into(),
        ));
    }
    validate_patch(p)?;
    let tx = conn.unchecked_transaction()?;
    // 1. Read current metadata under the tx.
    let raw: Option<String> = tx
        .query_row(
            "SELECT metadata FROM tasks WHERE id = ?1",
            params![id],
            |r| r.get::<_, Option<String>>(0),
        )
        .optional()?
        .ok_or(Error::NotFound)?;
    let mut current = parse_metadata(raw);
    // 2. Mutate.
    f(&mut current)?;
    // 3. Build SET clause from patch + the new metadata.
    let (mut sets, mut args) = patch_sets_and_args(p);
    sets.push("metadata = ?".to_string());
    args.push(Box::new(marshal_metadata(&current)));
    sets.push("updated_at = ?".to_string());
    args.push(Box::new(crate::timefmt::now_unix()));
    args.push(Box::new(id.to_string()));
    let q = format!("UPDATE tasks SET {} WHERE id = ?", sets.join(", "));
    let n = tx.execute(
        &q,
        rusqlite::params_from_iter(args.iter().map(|b| b.as_ref())),
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    tx.commit()?;
    get_task(conn, id)
}

fn patch_sets_and_args(p: &TaskPatch) -> (Vec<String>, Vec<Box<dyn rusqlite::ToSql>>) {
    let mut sets: Vec<String> = Vec::new();
    let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
    if let Some(title) = &p.title {
        sets.push("title = ?".to_string());
        args.push(Box::new(title.trim().to_string()));
    }
    if let Some(d) = &p.description {
        sets.push("description = ?".to_string());
        args.push(Box::new(d.clone()));
    }
    if let Some(s) = &p.status {
        sets.push("status = ?".to_string());
        args.push(Box::new(s.clone()));
    }
    if let Some(pr) = &p.priority {
        sets.push("priority = ?".to_string());
        args.push(Box::new(*pr));
    }
    if let Some(w) = &p.workflow_id {
        sets.push("workflow_id = ?".to_string());
        args.push(Box::new(null_text(w)));
    }
    if let Some(c) = &p.current_step_id {
        sets.push("current_step_id = ?".to_string());
        args.push(Box::new(null_text(c)));
    }
    (sets, args)
}

fn validate_patch(p: &TaskPatch) -> Result<()> {
    if let Some(s) = &p.status {
        if !status_valid(s) {
            return Err(Error::Migration(format!("invalid status {s}")));
        }
    }
    if let Some(pr) = &p.priority {
        if *pr < MIN_PRIORITY || *pr > MAX_PRIORITY {
            return Err(Error::Migration(format!("invalid priority {pr}")));
        }
    }
    Ok(())
}

/// Empty string → SQL NULL; non-empty → the text. Mirrors `nullText`.
fn null_text(s: &str) -> Option<String> {
    if s.is_empty() {
        None
    } else {
        Some(s.to_string())
    }
}
