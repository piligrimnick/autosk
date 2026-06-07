//! `agents` writes — the Rust port of `internal/agent.Store` (Create /
//! EnsureByName). An agent row is `id + name + is_human + created_at`. The
//! optional `resolver` gate mirrors `WithResolver`: when present, names other
//! than `human` must be installed (used by create/enroll/workflow paths; the
//! comment path passes no resolver so any author auto-creates).

use rusqlite::{params, Connection, OptionalExtension};

use crate::error::{Error, Result};
use crate::ids;
use crate::timefmt;

/// Reserved human-agent name (resolver-exempt).
pub const HUMAN_AGENT_NAME: &str = "human";

/// One `agents` row.
#[derive(Debug, Clone, PartialEq)]
pub struct Agent {
    pub id: String,
    pub name: String,
    pub is_human: bool,
}

/// A package-installed gate: `Some(f)` rejects a non-human name when
/// `f(name)` is false (mirror of `agent.PackageResolver`).
pub type Resolver<'a> = Option<&'a dyn Fn(&str) -> bool>;

/// `GetByName` — one agent row, or `Ok(None)`.
pub fn get_by_name(conn: &Connection, name: &str) -> Result<Option<Agent>> {
    Ok(conn
        .query_row(
            "SELECT id, name, is_human FROM agents WHERE name = ?1",
            params![name],
            scan,
        )
        .optional()?)
}

/// `Create` — inserts a new agent. `ErrAlreadyExist` → [`Error::Conflict`];
/// resolver miss → [`Error::Invalid`] (the `agent_not_installed` hint).
pub fn create(conn: &Connection, name: &str, is_human: bool, resolver: Resolver) -> Result<Agent> {
    let name = name.trim();
    if name.is_empty() {
        return Err(Error::Invalid("invalid agent name: empty".into()));
    }
    if name.contains([' ', '\t', '\n', '\r']) {
        return Err(Error::Invalid(
            "invalid agent name: contains whitespace".into(),
        ));
    }
    if let Some(has) = resolver {
        if name != HUMAN_AGENT_NAME && !has(name) {
            return Err(Error::Invalid(format!(
                "agent_not_installed: {name} (run: autosk agent install {name})"
            )));
        }
    }
    let id = ids::mint_unique(conn, ids::AGENT_PREFIX, "agents", "id")?;
    let now = timefmt::now_unix();
    let human = if is_human { 1 } else { 0 };
    let res = conn.execute(
        "INSERT INTO agents(id, name, is_human, created_at) VALUES (?1, ?2, ?3, ?4)",
        params![id, name, human, now],
    );
    if let Err(e) = res {
        if e.to_string()
            .contains("UNIQUE constraint failed: agents.name")
        {
            return Err(Error::Conflict(format!("agent already exists: {name}")));
        }
        return Err(Error::Sqlite(e));
    }
    Ok(Agent {
        id,
        name: name.to_string(),
        is_human,
    })
}

/// `EnsureByName` — returns the agent, creating it if absent. `is_human` is set
/// only for the literal name `human`. Mirror of `agent.EnsureByName`.
pub fn ensure_by_name(conn: &Connection, name: &str, resolver: Resolver) -> Result<Agent> {
    let name = name.trim();
    if name.is_empty() {
        return Err(Error::Invalid("invalid agent name: empty".into()));
    }
    if let Some(a) = get_by_name(conn, name)? {
        return Ok(a);
    }
    let is_human = name == HUMAN_AGENT_NAME;
    create(conn, name, is_human, resolver)
}

// NB: there is deliberately NO `ensure_caller` helper that reads the daemon's
// process env. The caller agent (`$AUTOSK_AGENT`, else `human`) is ALWAYS
// resolved client-side and threaded through as `CreateParams::caller` /
// `comment_add`'s `author` — the daemon must never attribute authorship from
// its OWN environment (it would mis-stamp `author_id` for every client).

fn scan(row: &rusqlite::Row) -> rusqlite::Result<Agent> {
    let human: i64 = row.get(2)?;
    Ok(Agent {
        id: row.get(0)?,
        name: row.get(1)?,
        is_human: human != 0,
    })
}
