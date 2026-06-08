//! Canonical `feature-dev-generic` bootstrap — the Rust port of
//! `internal/bootstrap` + `cmd/autosk/init.go`'s `bootstrapDefaultWorkflow` +
//! `autoInstallMissingAgents`.
//!
//! The workflow JSON is embedded from this crate (the sole consumer) so there
//! is exactly one source of truth (mirroring how [`crate::migrate`] embeds the
//! schema).

use std::collections::BTreeSet;

use rusqlite::Connection;

use crate::agents_write;
use crate::error::{Error, Result};
use crate::pkg::Registry;
use crate::wfcrud::{self, Definition};

/// Name of the canonical workflow (mirror of `bootstrap.FeatureDevGenericName`).
pub const FEATURE_DEV_GENERIC_NAME: &str = "feature-dev-generic";

const FEATURE_DEV_GENERIC_JSON: &str = include_str!("feature-dev-generic.json");

/// Parses the embedded `feature-dev-generic` definition.
pub fn feature_dev_generic_def() -> Result<Definition> {
    wfcrud::parse_reader(FEATURE_DEV_GENERIC_JSON)
}

/// Auto-installs scoped (`@scope/name`) agents referenced by `def` that are not
/// yet in the `agents` table (mirror of `autoInstallMissingAgents`). Bare names
/// and `human` are skipped. Installs via npm + ensures the agents row.
pub fn auto_install_missing_agents(
    conn: &Connection,
    registry: &Registry,
    def: &Definition,
) -> Result<()> {
    let mut todo: BTreeSet<String> = BTreeSet::new();
    for (_, s) in &def.steps {
        let name = &s.agent_name;
        if name.is_empty() || name == agents_write::HUMAN_AGENT_NAME {
            continue;
        }
        if wfcrud::agent_id_by_name(conn, name)?.is_some() {
            continue;
        }
        if !looks_like_scoped_npm_name(name) {
            continue;
        }
        todo.insert(name.clone());
    }
    if todo.is_empty() {
        return Ok(());
    }
    registry
        .ensure_prefix()
        .map_err(|e| Error::Invalid(format!("ensure packages prefix: {e}")))?;
    let has = |n: &str| registry.has(n);
    for name in &todo {
        let entry = registry
            .install(name, "")
            .map_err(|e| Error::Invalid(format!(
                "auto-install {name} failed: {e} (install manually with `autosk agent install {name}`)"
            )))?;
        agents_write::ensure_by_name(conn, &entry.name, Some(&has))?;
    }
    Ok(())
}

/// `bootstrapDefaultWorkflow` — seeds `feature-dev-generic` if absent. Returns
/// `Ok(true)` when it created the workflow (caller commits
/// `"init: bootstrap feature-dev-generic workflow"`), `Ok(false)` when it was
/// already present.
pub fn bootstrap_default_workflow(conn: &Connection, registry: &Registry) -> Result<bool> {
    let def = feature_dev_generic_def()?;
    // Idempotent: already present?
    let present: bool = conn
        .query_row(
            "SELECT 1 FROM workflows WHERE name = ?1",
            rusqlite::params![def.name],
            |_| Ok(()),
        )
        .optional_present()?;
    if present {
        return Ok(false);
    }
    auto_install_missing_agents(conn, registry, &def)?;
    match wfcrud::create(conn, &def, false) {
        Ok(_) => Ok(true),
        // TOCTOU: another writer created it; treat as idempotent.
        Err(Error::Conflict(_)) => Ok(false),
        Err(e) => Err(e),
    }
}

fn looks_like_scoped_npm_name(s: &str) -> bool {
    if s.len() < 3 || !s.starts_with('@') {
        return false;
    }
    match s.find('/') {
        Some(slash) => slash > 1 && slash < s.len() - 1,
        None => false,
    }
}

/// Tiny extension so the `SELECT 1 ... present?` probe reads cleanly.
trait OptionalPresent {
    fn optional_present(self) -> Result<bool>;
}
impl OptionalPresent for rusqlite::Result<()> {
    fn optional_present(self) -> Result<bool> {
        match self {
            Ok(()) => Ok(true),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(false),
            Err(e) => Err(Error::Sqlite(e)),
        }
    }
}
