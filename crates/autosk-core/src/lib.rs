//! `autosk-core` — the Rust port that becomes the sole owner of `.autosk/db`
//! (plan §3, §7.2).
//!
//! Phase 1 delivered the **read core**: the migrator (port of
//! `internal/migrations`), the read paths for tasks/deps/comments/agents/
//! workflows/steps/daemon_runs/step_signals (port of the
//! `internal/lazy/datasource` offline read surface), the transcript reader (for
//! `job.messages`), the per-daemon project manager (cwd walk-up + lazy open +
//! stale-`running` sweep), and the persisted project registry.
//!
//! Phase 2 adds the **executor**: the executor's own writes ([`runstore`],
//! [`signals`], [`tasks`]/[`wfengine`] task-pointer + `step_visits`), the pi
//! JSON-Lines wire ([`pi`]) + custom Node runner ([`agentnode`]), worktree
//! isolation ([`worktree`]), agent-package resolution ([`pkg`]), the live
//! runner registry + attach counter ([`pirunners`]), and the
//! [`poller`]/[`scheduler`]/[`compactor`] mesh that drives `feature-dev-generic`
//! end to end. General task/workflow/comment CRUD verbs land in Phase 3.
//!
//! doltlite is linked at **0.11.8** (container v12). autoskd is greenfield: it
//! owns only its own v12 format and never reads a Go-era v11 DB (see the task's
//! planning decision); the GC race is closed by the in-process RwLock
//! discipline documented in [`store`].

pub mod agentnode;
pub mod agents_write;
pub mod bootstrap;
pub mod comments_write;
pub mod compactor;
pub mod ctx;
pub mod deps;
pub mod error;
pub mod executor;
pub mod ids;
pub mod meta;
pub mod metaverbs;
pub mod migrate;
pub mod pi;
pub mod pirunners;
pub mod pkg;
pub mod poller;
pub mod projectmgr;
pub mod read;
pub mod registry;
pub mod runner;
pub mod runstore;
pub mod scheduler;
pub mod signals;
pub mod store;
pub mod tasks;
pub mod tasksvc;
pub mod timefmt;
pub mod transcript;
pub mod verbs;
pub mod wfcrud;
pub mod wfengine;
pub mod worktree;

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
