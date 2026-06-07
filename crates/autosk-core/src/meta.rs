//! Typed accessors for the reserved `step_visits` section of a task's
//! free-form `metadata` JSON blob — the Rust port of `internal/meta`.
//!
//! The engine reserves the top-level key `step_visits` (a map `step_id ->
//! count`). Going through these helpers keeps a JSON round-trip from
//! corrupting the shape (Go widens int→float64; serde_json stores numbers
//! as [`serde_json::Number`], so the same defensive read applies).

use serde_json::{Map, Value};

/// Reserved top-level metadata key whose value is a map `step_id -> visit
/// count` (>= 0). Mirrors `meta.StepVisitsKey`.
pub const STEP_VISITS_KEY: &str = "step_visits";

/// Returns a typed copy of the reserved `step_visits` map. Tolerates the
/// wire shape (integers may decode as floats); malformed leaves are
/// dropped, matching `meta.GetStepVisits`. Always returns a (possibly
/// empty) map.
pub fn get_step_visits(m: &Map<String, Value>) -> std::collections::BTreeMap<String, i64> {
    let mut out = std::collections::BTreeMap::new();
    let Some(Value::Object(raw)) = m.get(STEP_VISITS_KEY) else {
        return out;
    };
    for (k, v) in raw {
        if let Some(n) = as_count(v) {
            out.insert(k.clone(), n);
        }
    }
    out
}

/// Replaces the reserved `step_visits` sub-object on `m` with `sv`. An
/// empty `sv` deletes the key entirely so an empty metadata blob
/// round-trips back to SQL NULL. Mirrors `meta.SetStepVisits`.
pub fn set_step_visits(m: &mut Map<String, Value>, sv: &std::collections::BTreeMap<String, i64>) {
    if sv.is_empty() {
        m.remove(STEP_VISITS_KEY);
        return;
    }
    let mut obj = Map::new();
    for (k, v) in sv {
        obj.insert(k.clone(), Value::from(*v));
    }
    m.insert(STEP_VISITS_KEY.to_string(), Value::Object(obj));
}

/// Read-modify-write helper used by the engine: hands `f` the typed
/// counters, then writes the result back (deleting the key when empty).
/// Mirrors `meta.MutateStepVisits`.
pub fn mutate_step_visits<F>(m: &mut Map<String, Value>, f: F)
where
    F: FnOnce(&mut std::collections::BTreeMap<String, i64>),
{
    let mut sv = get_step_visits(m);
    f(&mut sv);
    set_step_visits(m, &sv);
}

/// Interprets a JSON value as a non-negative visit count (whole numbers
/// only), or `None` when it is not a usable counter leaf.
fn as_count(v: &Value) -> Option<i64> {
    match v {
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                Some(i)
            } else {
                n.as_f64().and_then(|f| {
                    if f.fract() == 0.0 {
                        Some(f as i64)
                    } else {
                        None
                    }
                })
            }
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn round_trip_and_mutate() {
        let mut m: Map<String, Value> = json!({"step_visits": {"st-a": 3, "st-b": 1}})
            .as_object()
            .unwrap()
            .clone();
        let sv = get_step_visits(&m);
        assert_eq!(sv["st-a"], 3);
        assert_eq!(sv["st-b"], 1);

        mutate_step_visits(&mut m, |sv| {
            *sv.entry("st-a".to_string()).or_insert(0) += 1;
        });
        assert_eq!(get_step_visits(&m)["st-a"], 4);
    }

    #[test]
    fn empty_deletes_key() {
        let mut m: Map<String, Value> = json!({"step_visits": {"st-x": 1}})
            .as_object()
            .unwrap()
            .clone();
        mutate_step_visits(&mut m, |sv| {
            sv.remove("st-x");
        });
        assert!(!m.contains_key(STEP_VISITS_KEY));
    }

    #[test]
    fn malformed_leaf_dropped() {
        let m: Map<String, Value> = json!({"step_visits": {"st-a": 2, "st-b": "no", "st-c": [1]}})
            .as_object()
            .unwrap()
            .clone();
        let sv = get_step_visits(&m);
        assert_eq!(sv.get("st-a"), Some(&2));
        assert!(!sv.contains_key("st-b"));
        assert!(!sv.contains_key("st-c"));
    }
}
