//! `tasks.metadata` editing verbs — the Rust port of `cmd/autosk/metadata.go`
//! path helpers + `internal/meta` validation + the change-detecting
//! `UpdateMetadata` from `internal/store/doltlite/metadata.go`.
//!
//! `update_metadata` loads the blob, hands a mutable [`Map`] to `f`, and writes
//! it back ONLY when the canonical (sorted) JSON actually changed — so a no-op
//! edit neither bumps `updated_at` nor produces a dolt revision (mirror of the
//! Go `changed` flag that gates the commit).

use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{Map, Value};

use crate::error::{Error, Result};
use crate::meta;
use crate::timefmt;

/// Outcome of [`update_metadata`]: the resulting blob (`None` when empty) and
/// whether anything changed.
pub struct MetaUpdate {
    pub metadata: Option<Value>,
    pub changed: bool,
}

/// Loads `tasks.metadata`, runs `f` on a mutable copy, and writes it back when
/// the canonical JSON changed. Mirror of `Store.UpdateMetadata`.
pub fn update_metadata<F>(conn: &Connection, task_id: &str, f: F) -> Result<MetaUpdate>
where
    F: FnOnce(&mut Map<String, Value>) -> Result<()>,
{
    let tx = conn.unchecked_transaction()?;
    let raw: Option<String> = tx
        .query_row(
            "SELECT metadata FROM tasks WHERE id = ?1",
            params![task_id],
            |r| r.get::<_, Option<String>>(0),
        )
        .optional()?
        .ok_or(Error::NotFound)?;
    let mut current = parse_metadata(raw);
    let pre = serde_json::to_vec(&current).unwrap_or_default();
    f(&mut current)?;
    let post = serde_json::to_vec(&current).unwrap_or_default();
    if pre == post {
        // No-op: roll the tx back (drop), skip the updated_at bump.
        return Ok(MetaUpdate {
            metadata: map_or_none(&current),
            changed: false,
        });
    }
    let arg = marshal_metadata(&current);
    let now = timefmt::now_unix();
    let n = tx.execute(
        "UPDATE tasks SET metadata = ?1, updated_at = ?2 WHERE id = ?3",
        params![arg, now, task_id],
    )?;
    if n == 0 {
        return Err(Error::NotFound);
    }
    tx.commit()?;
    Ok(MetaUpdate {
        metadata: map_or_none(&current),
        changed: true,
    })
}

fn parse_metadata(raw: Option<String>) -> Map<String, Value> {
    match raw {
        Some(s) if !s.trim().is_empty() => {
            serde_json::from_str::<Map<String, Value>>(&s).unwrap_or_default()
        }
        _ => Map::new(),
    }
}

fn marshal_metadata(m: &Map<String, Value>) -> Option<String> {
    if m.is_empty() {
        None
    } else {
        serde_json::to_string(m).ok()
    }
}

fn map_or_none(m: &Map<String, Value>) -> Option<Value> {
    if m.is_empty() {
        None
    } else {
        Some(Value::Object(m.clone()))
    }
}

/// `splitMetadataKey` — splits a dotted path; empty segments are an error.
pub fn split_metadata_key(k: &str) -> Result<Vec<String>> {
    let trimmed = k.trim();
    if trimmed.is_empty() {
        return Err(Error::Invalid("--key cannot be empty".into()));
    }
    let mut out = Vec::new();
    for p in trimmed.split('.') {
        let p = p.trim();
        if p.is_empty() {
            return Err(Error::Invalid(format!(
                "--key {k:?} has an empty dotted segment; leading/trailing/doubled dots are not allowed"
            )));
        }
        out.push(p.to_string());
    }
    Ok(out)
}

/// `setMetadataPath` — stores `value` at the dotted `path`, creating intermediate
/// objects. Errors when an intermediate element is not an object.
pub fn set_metadata_path(m: &mut Map<String, Value>, path: &[String], value: Value) -> Result<()> {
    let (leaf, parents) = path.split_last().expect("path is non-empty");
    let mut cur = m;
    for (i, key) in parents.iter().enumerate() {
        let entry = cur.entry(key.clone()).or_insert(Value::Null);
        if entry.is_null() {
            *entry = Value::Object(Map::new());
        }
        if !entry.is_object() {
            return Err(Error::Invalid(format!(
                "cannot descend into {key:?} at path {}: not an object",
                path[..=i].join(".")
            )));
        }
        cur = entry.as_object_mut().unwrap();
    }
    cur.insert(leaf.clone(), value);
    Ok(())
}

/// `unsetMetadataPath` — removes the value at `path`, pruning empty parents
/// bottom-up. Returns whether a removal happened. A non-object link or an
/// absent leaf is a no-op (returns false).
pub fn unset_metadata_path(m: &mut Map<String, Value>, path: &[String]) -> bool {
    if path.is_empty() {
        return false;
    }
    remove_and_prune(m, path)
}

fn remove_and_prune(m: &mut Map<String, Value>, path: &[String]) -> bool {
    let (leaf, parents) = path.split_last().unwrap();
    if parents.is_empty() {
        return m.remove(leaf).is_some();
    }
    let head = &parents[0];
    let Some(child) = m.get_mut(head).and_then(Value::as_object_mut) else {
        return false;
    };
    let removed = remove_and_prune(child, &path[1..]);
    if removed && child.is_empty() {
        m.remove(head);
    }
    removed
}

/// `validateReservedWrite` — enforces the typed `step_visits` shape (mirror).
pub fn validate_reserved_write(path: &[String], value: &Value) -> Result<()> {
    if path.is_empty() || path[0] != meta::STEP_VISITS_KEY {
        return Ok(());
    }
    match path.len() {
        1 => validate_step_visits_object(value),
        2 => validate_step_visits_leaf(value),
        _ => Err(Error::Invalid(format!(
            "step_visits has a flat shape (step_id → int); cannot nest under {}",
            path.join(".")
        ))),
    }
}

fn validate_step_visits_leaf(v: &Value) -> Result<()> {
    match v {
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                if i < 0 {
                    return Err(Error::Invalid(format!(
                        "step_visits leaves must be >= 0, got {i}"
                    )));
                }
                Ok(())
            } else if let Some(f) = n.as_f64() {
                if f < 0.0 || f.fract() != 0.0 {
                    return Err(Error::Invalid(format!(
                        "step_visits leaves must be non-negative integers, got {f}"
                    )));
                }
                Ok(())
            } else {
                Err(Error::Invalid("step_visits leaves must be integers".into()))
            }
        }
        other => Err(Error::Invalid(format!(
            "step_visits leaves must be integers, got {}",
            json_kind(other)
        ))),
    }
}

fn validate_step_visits_object(v: &Value) -> Result<()> {
    match v {
        Value::Null => Ok(()),
        Value::Object(m) => {
            for (k, leaf) in m {
                validate_step_visits_leaf(leaf)
                    .map_err(|e| Error::Invalid(format!("step_visits[{k:?}]: {e}")))?;
            }
            Ok(())
        }
        other => Err(Error::Invalid(format!(
            "step_visits must be a JSON object, got {}",
            json_kind(other)
        ))),
    }
}

fn json_kind(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}
