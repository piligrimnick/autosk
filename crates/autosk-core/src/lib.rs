//! `autosk-core` — the Rust port that becomes the sole owner of `.autosk/db`
//! (plan §3, §7.2).
//!
//! Phase 1 scope: the **read core**. The migrator (port of
//! `internal/migrations`), the read paths for tasks/deps/comments/agents/
//! workflows/steps/daemon_runs/step_signals (port of the
//! `internal/lazy/datasource` offline read surface), the transcript reader (for
//! `job.messages`), the per-daemon project manager (cwd walk-up + lazy open +
//! stale-`running` sweep), and the persisted project registry. Writes,
//! executor and poller arrive in Phases 2-3.
//!
//! doltlite is linked at **0.11.8** (container v12). autoskd is greenfield: it
//! owns only its own v12 format and never reads a Go-era v11 DB (see the task's
//! planning decision); the GC race is closed by the in-process RwLock
//! discipline documented in [`store`].

pub mod error;
pub mod migrate;
pub mod projectmgr;
pub mod read;
pub mod registry;
pub mod store;
pub mod timefmt;
pub mod transcript;

pub use error::{Error, Result};
pub use read::{JobFilter, TaskFilter};
pub use store::{Db, GcStats, TaskRow};

use rusqlite::Connection;

/// Returns the storage engine doltlite reports (`"prolly"` when `libdoltlite.a`
/// is correctly linked). The canonical "is the link wired?" probe, the Rust
/// analogue of the Go `TestDoltliteEngine` smoke test.
pub fn doltlite_engine() -> Result<String> {
    let conn = Connection::open_in_memory()?;
    let engine: String = conn.query_row("SELECT doltlite_engine()", [], |row| row.get(0))?;
    Ok(engine)
}
