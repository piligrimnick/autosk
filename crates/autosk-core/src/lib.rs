//! `autosk-core` — the Rust port target that will become the sole owner of
//! `.autosk/db` (plan §3, §7.2).
//!
//! Phase 0 scope is deliberately tiny: prove that Rust can link `libdoltlite.a`
//! (doltlite **0.11.8**), open an existing `.autosk/db`, read `tasks`, run
//! `dolt_commit` / `dolt_gc`, and survive a GC running concurrently with reads
//! under an in-process RwLock discipline. None of the domain/engine/executor
//! port lives here yet — that is Phases 1-3.

pub mod store;

pub use store::{Db, Error, GcStats, Result, TaskRow};

use rusqlite::Connection;

/// Returns the storage engine doltlite reports (`"prolly"` when `libdoltlite.a`
/// is correctly linked). Used as the canonical "is the link wired?" probe, the
/// Rust analogue of the Go `TestDoltliteEngine` smoke test.
pub fn doltlite_engine() -> Result<String> {
    let conn = Connection::open_in_memory()?;
    let engine: String = conn.query_row("SELECT doltlite_engine()", [], |row| row.get(0))?;
    Ok(engine)
}
