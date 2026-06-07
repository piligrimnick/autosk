//! `task_deps` writes — the Rust port of `internal/store/doltlite/deps.go`
//! (block / unblock / unblock-all + cycle detection).
//!
//! `block` adds `blocker → id` edges (each blocker must exist; `id` must
//! exist; self-block and cycles are rejected; re-adding an edge is a no-op).
//! `unblock` removes specific edges (missing edges ignored); `unblock_all`
//! drops every incoming blocker edge. All three run in one transaction on the
//! single writer connection.

use std::collections::HashSet;

use rusqlite::{params, Connection, OptionalExtension};

use crate::error::{Error, Result};

/// Typed block/unblock failures (mirror of the Go `store.Err*` sentinels). The
/// verb layer translates these to the CLI-parity user message.
#[derive(Debug)]
pub enum DepsError {
    /// `id` (the task being blocked) does not exist.
    TaskNotFound,
    /// A blocker id does not exist.
    BlockerNotFound,
    /// A blocker equals the blocked id.
    SelfBlock,
    /// Adding the edge would create a cycle.
    Cycle,
    /// A lower-level store error.
    Core(Error),
}

impl From<Error> for DepsError {
    fn from(e: Error) -> Self {
        DepsError::Core(e)
    }
}

impl From<rusqlite::Error> for DepsError {
    fn from(e: rusqlite::Error) -> Self {
        DepsError::Core(Error::Sqlite(e))
    }
}

/// `Block` — adds `blocker → id` for each blocker, transactionally. Empty
/// blocker list is a no-op. Mirrors `Store.Block`/`blockOnce`.
pub fn block(
    conn: &Connection,
    id: &str,
    blockers: &[String],
) -> std::result::Result<(), DepsError> {
    if blockers.is_empty() {
        return Ok(());
    }
    let tx = conn.unchecked_transaction()?;
    assert_task_exists(&tx, id, DepsError::TaskNotFound)?;
    for b in blockers {
        if b == id {
            return Err(DepsError::SelfBlock);
        }
        assert_task_exists(&tx, b, DepsError::BlockerNotFound)?;
        // Cycle check: if `id` already transitively blocks `b` (id→…→b), then
        // adding b→id closes a cycle. Caller passes from=id, to=b.
        if reachable(&tx, id, b)? {
            return Err(DepsError::Cycle);
        }
        tx.execute(
            "INSERT OR IGNORE INTO task_deps(blocker_id, blocked_id, kind) VALUES (?1, ?2, 'blocks')",
            params![b, id],
        )?;
    }
    tx.commit()?;
    Ok(())
}

/// `Unblock` — removes specific `blocker → id` edges; missing edges ignored.
pub fn unblock(conn: &Connection, id: &str, blockers: &[String]) -> Result<()> {
    if blockers.is_empty() {
        return Ok(());
    }
    let tx = conn.unchecked_transaction()?;
    for b in blockers {
        tx.execute(
            "DELETE FROM task_deps WHERE blocker_id = ?1 AND blocked_id = ?2 AND kind = 'blocks'",
            params![b, id],
        )?;
    }
    tx.commit()?;
    Ok(())
}

/// `UnblockAll` — drops every incoming blocker edge for `id`; returns the
/// number of rows deleted.
pub fn unblock_all(conn: &Connection, id: &str) -> Result<i64> {
    let n = conn.execute(
        "DELETE FROM task_deps WHERE blocked_id = ?1 AND kind = 'blocks'",
        params![id],
    )?;
    Ok(n as i64)
}

fn assert_task_exists(
    conn: &Connection,
    id: &str,
    on_missing: DepsError,
) -> std::result::Result<(), DepsError> {
    let found: Option<i64> = conn
        .query_row("SELECT 1 FROM tasks WHERE id = ?1", params![id], |r| {
            r.get(0)
        })
        .optional()?;
    match found {
        Some(_) => Ok(()),
        None => Err(on_missing),
    }
}

/// Reports whether `to` is reachable from `from` via outgoing blocker edges
/// (`from → … → to`). Iterative BFS (mirror of `reachableTx`).
fn reachable(conn: &Connection, from: &str, to: &str) -> std::result::Result<bool, DepsError> {
    if from == to {
        return Ok(true);
    }
    let mut visited: HashSet<String> = HashSet::new();
    visited.insert(from.to_string());
    let mut frontier: Vec<String> = vec![from.to_string()];
    while !frontier.is_empty() {
        let placeholders = vec!["?"; frontier.len()].join(",");
        let q = format!(
            "SELECT blocked_id FROM task_deps WHERE kind='blocks' AND blocker_id IN ({placeholders})"
        );
        let mut stmt = conn.prepare(&q)?;
        let rows = stmt.query_map(rusqlite::params_from_iter(frontier.iter()), |r| {
            r.get::<_, String>(0)
        })?;
        let mut next: Vec<String> = Vec::new();
        for r in rows {
            let bd = r.map_err(|e| DepsError::Core(Error::Sqlite(e)))?;
            if bd == to {
                return Ok(true);
            }
            if visited.insert(bd.clone()) {
                next.push(bd);
            }
        }
        frontier = next;
    }
    Ok(false)
}
