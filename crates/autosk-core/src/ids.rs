//! Short id minting — the Rust port of `internal/id` (plan §7.1).
//!
//! Ids are `<prefix>-<6 lowercase hex chars>` (3 bytes of entropy). The Go
//! side draws from `crypto/rand`; here we use SQLite's `randomblob` (same
//! source [`crate::runstore::new_job_id`] already uses) so no `rand` crate is
//! pulled in. The id VALUE is never compared against a golden (timestamps in
//! the same rows already differ run-to-run), only its SHAPE matters.

use rusqlite::{params, Connection, OptionalExtension};

use crate::error::{Error, Result};

/// Task-id prefix (`ask-XXXXXX`).
pub const TASK_PREFIX: &str = "ask";
/// Workflow-id prefix (`wf-XXXXXX`).
pub const WORKFLOW_PREFIX: &str = "wf";
/// Step-id prefix (`st-XXXXXX`).
pub const STEP_PREFIX: &str = "st";
/// Agent-id prefix (`ag-XXXXXX`).
pub const AGENT_PREFIX: &str = "ag";

/// Mints a fresh `<prefix>-<6hex>` id not present in `table`.`col`. Mirrors
/// `id.NewUnique(prefix, …)`: up to 16 attempts before [`Error::Migration`].
///
/// `table` and `col` are interpolated into the existence probe; callers pass
/// only crate-internal constants, never user input.
pub fn mint_unique(conn: &Connection, prefix: &str, table: &str, col: &str) -> Result<String> {
    let probe = format!("SELECT 1 FROM {table} WHERE {col} = ?1");
    for _ in 0..16 {
        let hex: String = conn.query_row("SELECT lower(hex(randomblob(3)))", [], |r| r.get(0))?;
        let id = format!("{prefix}-{hex}");
        let taken: Option<i64> = conn
            .query_row(&probe, params![id], |r| r.get(0))
            .optional()?;
        if taken.is_none() {
            return Ok(id);
        }
    }
    Err(Error::Migration(format!("{prefix} id space exhausted")))
}
