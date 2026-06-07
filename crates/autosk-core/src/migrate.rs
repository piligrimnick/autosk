//! Schema migrator — the Rust port of `internal/migrations` (plan §7.1, §8.3).
//!
//! autoskd is GREENFIELD: it owns only its own doltlite-0.11.8 (container v12)
//! format and never reads a Go-era v11 DB (see the task's planning decision).
//! This migrator runs against a FRESH v12 DB and must yield the **exact**
//! `schema_version`, table DDL and CHECK constraints the Go `001_init.sql`
//! produced — pinned by `tests/schema_golden.rs`.
//!
//! To keep a single source of truth for the schema, the SQL itself is embedded
//! straight from the Go tree (`internal/migrations/001_init.sql`) via
//! `include_str!`; the applier mirrors `migrations.go` exactly:
//!
//!   1. `EnsureTrackingTable` — create `schema_migrations` if missing.
//!   2. strip `--`-to-EOL comments per line, split on `;`, exec each non-empty
//!      statement (the `splitStatements` pipeline), so the text sqlite records
//!      in `sqlite_master` is byte-for-byte what the Go applier would store.
//!   3. record `schema_migrations(version=1)`.
//!   4. `SeedHumanAgent` — insert the canonical `human` agent row.

use rusqlite::Connection;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::error::Result;

/// The v0.1 consolidated schema, embedded from the Go tree so there is exactly
/// one copy of the DDL in the repository.
const INIT_SQL: &str = include_str!("../../../internal/migrations/001_init.sql");

/// The highest schema version this migrator knows how to produce.
pub const LATEST_VERSION: i64 = 1;

/// Applies all pending migrations to `conn` and returns the resulting schema
/// version. Idempotent: a no-op (beyond re-seeding `human`) on an already-
/// migrated DB. Mirrors `migrations.Apply`.
pub fn migrate(conn: &Connection) -> Result<i64> {
    ensure_tracking_table(conn)?;
    if current_version(conn)? < 1 {
        apply_initial_schema(conn)?;
    }
    // 001 creates the agents table; seed the `human` row exactly once
    // (idempotent), matching migrations.SeedHumanAgent.
    seed_human_agent(conn)?;
    current_version(conn)
}

/// Applies the schema (DDL + `schema_migrations` row) WITHOUT seeding the
/// `human` agent. Used by deterministic test fixtures that insert their own
/// fixed-id agents; production code uses [`migrate`].
pub fn apply_schema_only(conn: &Connection) -> Result<()> {
    ensure_tracking_table(conn)?;
    if current_version(conn)? < 1 {
        apply_initial_schema(conn)?;
    }
    Ok(())
}

/// Applies the 001 DDL and records `schema_migrations(version=1)` inside a
/// single transaction, mirroring the Go `applyOne` (all statements + the
/// version row commit atomically). On any failure the transaction rolls back,
/// so a fresh DB is never left half-applied with no version row — which would
/// make a retry re-run 001 and hit “table already exists.” Uses
/// `unchecked_transaction` because callers hold the connection behind the
/// writer mutex (shared `&Connection`), not `&mut`.
fn apply_initial_schema(conn: &Connection) -> Result<()> {
    let tx = conn.unchecked_transaction()?;
    apply_statements(&tx, INIT_SQL)?;
    record_version(&tx, 1)?;
    tx.commit()?;
    Ok(())
}

/// Creates the `schema_migrations` tracking table if it does not exist.
/// Mirrors `migrations.EnsureTrackingTable` (kept out of `001_init.sql` so the
/// system can record 001's own application — chicken-and-egg).
pub fn ensure_tracking_table(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS schema_migrations (\n\t\t\tversion    INTEGER PRIMARY KEY,\n\t\t\tapplied_at INTEGER NOT NULL\n\t\t)",
    )?;
    Ok(())
}

/// Returns the highest applied migration version, or 0 if none. Mirrors
/// `migrations.CurrentVersion`.
pub fn current_version(conn: &Connection) -> Result<i64> {
    ensure_tracking_table(conn)?;
    let v: Option<i64> = conn.query_row("SELECT MAX(version) FROM schema_migrations", [], |r| {
        r.get(0)
    })?;
    Ok(v.unwrap_or(0))
}

fn record_version(conn: &Connection, version: i64) -> Result<()> {
    let now = now_unix();
    conn.execute(
        "INSERT INTO schema_migrations(version, applied_at) VALUES(?1, ?2)",
        rusqlite::params![version, now],
    )?;
    Ok(())
}

/// Executes the statements in `sql` using the same pipeline as the Go applier:
/// strip `--`-to-EOL comments per line, split on `;`, run each non-empty
/// statement. This guarantees the CREATE text sqlite records is identical to
/// what the Go applier would have stored.
fn apply_statements(conn: &Connection, sql: &str) -> Result<()> {
    for stmt in split_statements(sql) {
        if stmt.trim().is_empty() {
            continue;
        }
        conn.execute_batch(&stmt)?;
    }
    Ok(())
}

/// Port of `migrations.splitStatements`: drop everything from the first `--` on
/// each line, rejoin with `\n`, split on `;`. Our migrations never embed `;`
/// inside string literals, so the naive split is safe.
pub fn split_statements(sql: &str) -> Vec<String> {
    let mut lines = Vec::new();
    for line in sql.split('\n') {
        if let Some(idx) = line.find("--") {
            lines.push(&line[..idx]);
        } else {
            lines.push(line);
        }
    }
    let cleaned = lines.join("\n");
    cleaned.split(';').map(|s| s.to_string()).collect()
}

/// Inserts the canonical `human` agent if absent. Idempotent. Mirrors
/// `migrations.SeedHumanAgent` (random `ag-XXXX` id, `is_human=1`).
pub fn seed_human_agent(conn: &Connection) -> Result<()> {
    let exists: Option<i64> = conn
        .query_row("SELECT 1 FROM agents WHERE name = 'human'", [], |r| {
            r.get(0)
        })
        .ok();
    if exists.is_some() {
        return Ok(());
    }
    let id = new_agent_id(conn)?;
    conn.execute(
        "INSERT INTO agents(id, name, is_human, created_at) VALUES (?1, 'human', 1, ?2)",
        rusqlite::params![id, now_unix()],
    )?;
    Ok(())
}

/// Generates a fresh `ag-XXXX` id not already present. The Go side uses
/// `crypto/rand`; the id value is not load-bearing (it is never compared), so
/// here we draw 16 bits from the wall clock and retry on the (vanishingly
/// unlikely) collision.
fn new_agent_id(conn: &Connection) -> Result<String> {
    for salt in 0..64u32 {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0);
        let v = (nanos.wrapping_add(salt.wrapping_mul(2_654_435_761))) & 0xffff;
        let id = format!("ag-{v:04x}");
        let taken: Option<i64> = conn
            .query_row("SELECT 1 FROM agents WHERE id = ?1", [&id], |r| r.get(0))
            .ok();
        if taken.is_none() {
            return Ok(id);
        }
    }
    // 64 collisions on a 16-bit space is statistically impossible on a fresh
    // DB; surface a clear error rather than loop forever.
    Err(crate::error::Error::Migration(
        "exhausted agent id attempts".to_string(),
    ))
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}
