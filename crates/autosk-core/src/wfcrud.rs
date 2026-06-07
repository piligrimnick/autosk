//! Workflow definition parse/validate/persist + delete + isolation update —
//! the Rust port of `internal/workflow/{parse,validate,store,synthetic}.go`
//! (plan §7.1). The executor-side reads + `enter_step` live in
//! [`crate::wfengine`]; this is the CRUD half that Phase 3 adds.

use std::collections::HashSet;

use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use crate::ctx::Ctx;
use crate::error::{Error, Result};
use crate::ids;
use crate::worktree::{self, WorktreeManager};
use autosk_proto::wire;

/// Reserved prefix for auto-generated `single:<agent>` workflows.
pub const SYNTHETIC_PREFIX: &str = "single:";

/// Allowed keys inside a step's `agent.params` block (closed set; mirror of
/// `allowedParamKeys`).
const ALLOWED_PARAM_KEYS: [&str; 7] = [
    "extra_args",
    "first_message",
    "first_message_file",
    "model",
    "pi_extensions",
    "pi_skills",
    "thinking",
];

/// Valid `task_status` transition targets (mirror of `ValidTaskStatuses`).
const VALID_TASK_STATUSES: [&str; 3] = ["human", "done", "cancel"];

/// Valid thinking levels (mirror of `ValidThinkingLevels`).
const VALID_THINKING: [&str; 7] = ["", "off", "minimal", "low", "medium", "high", "xhigh"];

/// Per-step agent overrides (mirror of `workflow.AgentParams`). `first_message_file`
/// is parse-time only; it is inlined into `first_message` and never persisted.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct AgentParams {
    pub model: Option<String>,
    pub thinking: Option<String>,
    pub first_message: Option<String>,
    pub first_message_file: String,
    pub extra_args: Option<Vec<String>>,
    pub pi_extensions: Option<Vec<String>>,
    pub pi_skills: Option<Vec<String>>,
}

impl AgentParams {
    /// True when the block carries no overrides (mirror of `AgentParams.IsZero`).
    pub fn is_zero(&self) -> bool {
        self.model.is_none()
            && self.thinking.is_none()
            && self.first_message.is_none()
            && self.first_message_file.is_empty()
            && self.extra_args.is_none()
            && self.pi_extensions.is_none()
            && self.pi_skills.is_none()
    }
}

/// One transition under `steps.<name>.next_steps` (mirror of `TransitionDef`).
#[derive(Debug, Clone)]
pub struct TransitionDef {
    pub step: String,
    pub task_status: String,
    pub prompt_rule: String,
}

impl TransitionDef {
    pub fn is_task_status(&self) -> bool {
        !self.task_status.is_empty()
    }
}

/// One step (mirror of `StepDef`).
#[derive(Debug, Clone)]
pub struct StepDef {
    pub agent_name: String,
    pub agent_params: Option<AgentParams>,
    pub next_steps: Vec<TransitionDef>,
    pub max_visits: i64,
}

/// Parsed workflow JSON (mirror of `Definition`). Steps preserve source order.
#[derive(Debug, Clone)]
pub struct Definition {
    pub name: String,
    pub description: String,
    pub first_step: String,
    pub isolation: String,
    /// `(step name, StepDef)` in source-file order.
    pub steps: Vec<(String, StepDef)>,
}

impl Definition {
    fn has_step(&self, name: &str) -> bool {
        self.steps.iter().any(|(n, _)| n == name)
    }
}

/// Normalises the isolation string (`""` → `"none"`).
pub fn normalize_isolation(s: &str) -> String {
    let t = s.trim();
    if t.is_empty() {
        "none".to_string()
    } else {
        t.to_string()
    }
}

fn isolation_valid(s: &str) -> bool {
    matches!(s, "none" | "worktree")
}

// ---- parse ----------------------------------------------------------------

#[derive(Deserialize)]
struct RawDoc {
    #[serde(default)]
    name: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    first_step: String,
    #[serde(default)]
    isolation: String,
    #[serde(default)]
    steps: Map<String, Value>,
}

/// Parses a JSON workflow definition from a string, rejecting any per-step
/// `first_message_file` (no base dir to resolve against). Mirror of
/// `workflow.ParseReader`.
pub fn parse_reader(body: &str) -> Result<Definition> {
    parse_inner(body, true)
}

/// Parses a workflow JSON file, resolving per-step `first_message_file` paths
/// relative to the file's directory. Mirror of `workflow.ParseFile`.
pub fn parse_file(path: &str) -> Result<Definition> {
    let body =
        std::fs::read_to_string(path).map_err(|e| Error::Invalid(format!("open {path}: {e}")))?;
    let mut def = parse_inner(&body, false)?;
    let base_dir = std::path::Path::new(path)
        .parent()
        .map(|p| p.to_path_buf())
        .unwrap_or_default();
    resolve_agent_param_files(&mut def, &base_dir)?;
    Ok(def)
}

fn parse_inner(body: &str, reject_fmf: bool) -> Result<Definition> {
    let raw: RawDoc =
        serde_json::from_str(body).map_err(|e| Error::Invalid(format!("decode workflow: {e}")))?;
    let iso_raw = raw.isolation.trim().to_string();
    let isolation = normalize_isolation(&iso_raw);
    if !isolation_valid(&isolation) {
        return Err(Error::Invalid(format!(
            "unknown isolation mode: {iso_raw:?} (want none|worktree)"
        )));
    }
    // Preserve source order: serde_json::Map is sorted (BTreeMap) by default,
    // which loses author order — recover it from the raw bytes like Go's
    // recoverStepOrder. We re-scan for the textual key order under "steps".
    let order = recover_step_order(body)?;
    let mut steps: Vec<(String, StepDef)> = Vec::with_capacity(raw.steps.len());
    let names: Vec<String> = if order.is_empty() {
        raw.steps.keys().cloned().collect()
    } else {
        order
    };
    for step_name in names {
        let Some(sv) = raw.steps.get(&step_name) else {
            continue;
        };
        let raw_step: RawStep = serde_json::from_value(sv.clone())
            .map_err(|e| Error::Invalid(format!("step {step_name:?}: {e}")))?;
        let (agent_name, params) = parse_agent_ref(&step_name, &raw_step.agent)?;
        if reject_fmf {
            if let Some(p) = &params {
                if !p.first_message_file.is_empty() {
                    return Err(Error::Invalid(format!(
                        "step {step_name:?}: agent.params.first_message_file requires a workflow file path; use ParseFile or move the prompt into `first_message`"
                    )));
                }
            }
        }
        let mut sd = StepDef {
            agent_name,
            agent_params: params,
            next_steps: Vec::new(),
            max_visits: raw_step.max_visits,
        };
        for (i, tr) in raw_step.next_steps.iter().enumerate() {
            let step = tr.step.trim().to_string();
            let status = tr.task_status.trim().to_string();
            let rule = tr.prompt_rule.trim().to_string();
            if step.is_empty() == status.is_empty() {
                return Err(Error::Invalid(format!(
                    "step {step_name:?} transition {i}: exactly one of `step` or `task_status` must be set"
                )));
            }
            sd.next_steps.push(TransitionDef {
                step,
                task_status: status,
                prompt_rule: rule,
            });
        }
        steps.push((step_name, sd));
    }
    Ok(Definition {
        name: raw.name.trim().to_string(),
        description: raw.description.trim().to_string(),
        first_step: raw.first_step.trim().to_string(),
        isolation,
        steps,
    })
}

#[derive(Deserialize)]
struct RawStep {
    #[serde(default)]
    agent: Value,
    #[serde(default)]
    next_steps: Vec<RawTransition>,
    #[serde(default)]
    max_visits: i64,
}

#[derive(Deserialize)]
struct RawTransition {
    #[serde(default)]
    step: String,
    #[serde(default)]
    task_status: String,
    #[serde(default)]
    prompt_rule: String,
}

#[derive(Deserialize)]
struct RawAgentRef {
    #[serde(default)]
    name: String,
    #[serde(default)]
    params: Value,
}

fn parse_agent_ref(step_name: &str, raw: &Value) -> Result<(String, Option<AgentParams>)> {
    match raw {
        Value::Null => Err(Error::Invalid(format!("step {step_name:?}: `agent` is required"))),
        Value::String(_) => Err(Error::Invalid(format!(
            "step {step_name:?}: `agent` must be an object `{{ \"name\": \"...\", \"params\": {{...}} }}`; the bare-string form is no longer supported"
        ))),
        Value::Object(_) => {
            let r: RawAgentRef = serde_json::from_value(raw.clone())
                .map_err(|e| Error::Invalid(format!("step {step_name:?}: parse agent object: {e}")))?;
            let name = r.name.trim().to_string();
            if name.is_empty() {
                return Err(Error::Invalid(format!("step {step_name:?}: agent.name is required")));
            }
            if r.params.is_null() {
                return Ok((name, None));
            }
            let params = parse_agent_params(step_name, &r.params)?;
            Ok((name, params))
        }
        other => Err(Error::Invalid(format!(
            "step {step_name:?}: `agent` must be an object, got {}",
            json_kind(other)
        ))),
    }
}

fn parse_agent_params(step_name: &str, raw: &Value) -> Result<Option<AgentParams>> {
    let Value::Object(obj) = raw else {
        return Err(Error::Invalid(format!(
            "step {step_name:?}: agent.params must be an object: {}",
            json_kind(raw)
        )));
    };
    for k in obj.keys() {
        if !ALLOWED_PARAM_KEYS.contains(&k.as_str()) {
            return Err(Error::Invalid(format!(
                "step {step_name:?}: agent.params has unknown key {k:?} (allowed: {})",
                ALLOWED_PARAM_KEYS.join(", ")
            )));
        }
    }
    let mut p = AgentParams::default();
    if let Some(v) = obj.get("model") {
        p.model = Some(as_str_field(step_name, "model", v)?);
    }
    if let Some(v) = obj.get("thinking") {
        p.thinking = Some(as_str_field(step_name, "thinking", v)?);
    }
    if let Some(v) = obj.get("first_message") {
        p.first_message = Some(as_str_field(step_name, "first_message", v)?);
    }
    if let Some(v) = obj.get("first_message_file") {
        p.first_message_file = as_str_field(step_name, "first_message_file", v)?;
    }
    p.extra_args = opt_str_vec(step_name, "extra_args", obj.get("extra_args"))?;
    p.pi_extensions = opt_str_vec(step_name, "pi_extensions", obj.get("pi_extensions"))?;
    p.pi_skills = opt_str_vec(step_name, "pi_skills", obj.get("pi_skills"))?;
    if p.is_zero() {
        Ok(None)
    } else {
        Ok(Some(p))
    }
}

fn as_str_field(step_name: &str, key: &str, v: &Value) -> Result<String> {
    v.as_str().map(|s| s.to_string()).ok_or_else(|| {
        Error::Invalid(format!(
            "step {step_name:?}: parse agent.params: {key} must be a string"
        ))
    })
}

fn opt_str_vec(step_name: &str, key: &str, v: Option<&Value>) -> Result<Option<Vec<String>>> {
    let Some(v) = v else { return Ok(None) };
    if v.is_null() {
        return Ok(None);
    }
    let arr = v.as_array().ok_or_else(|| {
        Error::Invalid(format!(
            "step {step_name:?}: parse agent.params: {key} must be an array"
        ))
    })?;
    if arr.is_empty() {
        // Go marshals an empty slice away via omitempty; collapse to absent so
        // the persisted blob byte-matches.
        return Ok(None);
    }
    let mut out = Vec::with_capacity(arr.len());
    for e in arr {
        out.push(e.as_str().map(str::to_string).ok_or_else(|| {
            Error::Invalid(format!(
                "step {step_name:?}: parse agent.params: {key} elements must be strings"
            ))
        })?);
    }
    Ok(Some(out))
}

/// Recovers the textual order of step names from the raw JSON (mirror of
/// `recoverStepOrder`). Returns an empty Vec when `steps` is missing.
fn recover_step_order(body: &str) -> Result<Vec<String>> {
    let v: Value = serde_json::from_str(body)
        .map_err(|e| Error::Invalid(format!("decode steps for order: {e}")))?;
    let Some(steps) = v.get("steps") else {
        return Ok(Vec::new());
    };
    // Re-parse just the "steps" object preserving key order via a streaming
    // scan of the raw bytes. serde_json::Value is sorted, so instead find the
    // "steps" substring and walk keys at depth 1.
    if !steps.is_object() {
        return Err(Error::Invalid("`steps` must be an object".into()));
    }
    Ok(scan_object_key_order(body, "steps"))
}

/// Minimal key-order scanner: finds the top-level `"<field>"` object and
/// returns its immediate keys in textual order. Tolerant — on any parse
/// surprise it returns what it has (callers fall back to sorted order).
fn scan_object_key_order(body: &str, field: &str) -> Vec<String> {
    let bytes = body.as_bytes();
    // Locate `"field"` then the following `{`.
    let needle = format!("\"{field}\"");
    let Some(mut i) = body.find(&needle) else {
        return Vec::new();
    };
    i += needle.len();
    while i < bytes.len() && bytes[i] != b'{' {
        if bytes[i] == b'"' || bytes[i] == b'[' {
            return Vec::new();
        }
        i += 1;
    }
    if i >= bytes.len() {
        return Vec::new();
    }
    i += 1; // past '{'
    let mut keys = Vec::new();
    let mut depth = 0i32; // depth INSIDE the steps object's values
    let mut j = i;
    while j < bytes.len() {
        let c = bytes[j];
        if depth == 0 {
            match c {
                b'"' => {
                    // Read a key string.
                    let (s, end) = read_json_string(bytes, j);
                    keys.push(s);
                    j = end;
                    // Skip to the ':' then the value; track value via depth.
                    while j < bytes.len() && bytes[j] != b':' {
                        j += 1;
                    }
                    j += 1;
                    // Skip whitespace.
                    while j < bytes.len() && (bytes[j] as char).is_whitespace() {
                        j += 1;
                    }
                    if j < bytes.len() && (bytes[j] == b'{' || bytes[j] == b'[') {
                        depth += 1;
                        j += 1;
                    }
                    continue;
                }
                b'}' => break,
                _ => {
                    j += 1;
                }
            }
        } else {
            match c {
                b'"' => {
                    let (_s, end) = read_json_string(bytes, j);
                    j = end;
                    continue;
                }
                b'{' | b'[' => depth += 1,
                b'}' | b']' => depth -= 1,
                _ => {}
            }
            j += 1;
        }
    }
    keys
}

/// Reads a JSON string literal starting at `start` (which must be `"`),
/// returning the unescaped contents and the index just past the closing quote.
fn read_json_string(bytes: &[u8], start: usize) -> (String, usize) {
    let mut j = start + 1;
    let mut out = String::new();
    while j < bytes.len() {
        match bytes[j] {
            b'\\' => {
                if j + 1 < bytes.len() {
                    let e = bytes[j + 1];
                    match e {
                        b'n' => out.push('\n'),
                        b't' => out.push('\t'),
                        b'r' => out.push('\r'),
                        b'"' => out.push('"'),
                        b'\\' => out.push('\\'),
                        b'/' => out.push('/'),
                        _ => out.push(e as char),
                    }
                    j += 2;
                    continue;
                }
                j += 1;
            }
            b'"' => return (out, j + 1),
            c => {
                out.push(c as char);
                j += 1;
            }
        }
    }
    (out, j)
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

fn resolve_agent_param_files(def: &mut Definition, base_dir: &std::path::Path) -> Result<()> {
    for (step_name, sd) in def.steps.iter_mut() {
        let Some(p) = sd.agent_params.as_mut() else {
            continue;
        };
        if p.first_message_file.is_empty() {
            continue;
        }
        if p.first_message.is_some() {
            return Err(Error::Invalid(format!(
                "step {step_name:?}: agent.params has both first_message and first_message_file (pick one)"
            )));
        }
        let rel = &p.first_message_file;
        if std::path::Path::new(rel).is_absolute() {
            return Err(Error::Invalid(format!(
                "step {step_name:?}: agent.params.first_message_file must be a relative path inside the workflow file's directory, got {rel:?}"
            )));
        }
        let abs = base_dir.join(rel);
        let body = std::fs::read_to_string(&abs).map_err(|e| {
            Error::Invalid(format!(
                "step {step_name:?}: read first_message_file {:?}: {e}",
                abs.display()
            ))
        })?;
        p.first_message = Some(body);
        p.first_message_file = String::new();
    }
    Ok(())
}

// ---- validate -------------------------------------------------------------

/// Options for [`validate`] (mirror of `ValidateOpts`).
#[derive(Debug, Clone, Copy, Default)]
pub struct ValidateOpts {
    pub allow_synthetic_name: bool,
}

/// Structural + referential validation (mirror of `workflow.Validate`). Agent
/// existence is checked when `agent_exists` is `Some`. All problems are joined
/// into one error.
pub fn validate(
    def: &Definition,
    opts: ValidateOpts,
    agent_exists: Option<&dyn Fn(&str) -> bool>,
) -> Result<()> {
    let mut problems: Vec<String> = Vec::new();
    if def.name.is_empty() {
        problems.push("`name` is required".to_string());
    }
    if !opts.allow_synthetic_name && def.name.starts_with(SYNTHETIC_PREFIX) {
        problems.push(format!(
            "`name` starts with reserved prefix {SYNTHETIC_PREFIX:?} (used for synthetic single-agent workflows)"
        ));
    }
    let iso = normalize_isolation(&def.isolation);
    if !isolation_valid(&iso) {
        problems.push(format!(
            "`isolation` has unknown value {:?} (want none|worktree)",
            def.isolation
        ));
    }
    if def.name.starts_with(SYNTHETIC_PREFIX) && iso == "worktree" {
        problems.push(
            "synthetic `single:<agent>` workflows cannot use `isolation: worktree` (synthetic workflows must remain `none`)"
                .to_string(),
        );
    }
    if def.first_step.is_empty() {
        problems.push("`first_step` is required".to_string());
    }
    if def.steps.is_empty() {
        problems.push("`steps` is empty".to_string());
    } else if !def.first_step.is_empty() && !def.has_step(&def.first_step) {
        problems.push(format!(
            "`first_step` {:?} is not defined under `steps`",
            def.first_step
        ));
    }

    for (step_name, s) in &def.steps {
        if step_name.is_empty() {
            problems.push("step has empty name".to_string());
        }
        if s.agent_name.is_empty() {
            problems.push(format!("step {step_name:?}: `agent.name` is required"));
        }
        if s.next_steps.is_empty() {
            problems.push(format!(
                "step {step_name:?}: needs at least one transition in `next_steps`"
            ));
        }
        if s.max_visits < 0 {
            problems.push(format!(
                "step {step_name:?}: max_visits must be >= 0 (0 = unlimited)"
            ));
        }
        if let Some(p) = &s.agent_params {
            if let Some(th) = &p.thinking {
                if !VALID_THINKING.contains(&th.as_str()) {
                    problems.push(format!(
                        "step {step_name:?}: agent.params.thinking={th:?} (want one of off|minimal|low|medium|high|xhigh)"
                    ));
                }
            }
            if p.first_message.is_some() && !p.first_message_file.is_empty() {
                problems.push(format!(
                    "step {step_name:?}: agent.params declares both first_message and first_message_file (pick one)"
                ));
            }
        }
        for (i, tr) in s.next_steps.iter().enumerate() {
            if tr.prompt_rule.is_empty() {
                problems.push(format!(
                    "step {step_name:?} transition {i}: `prompt_rule` is required"
                ));
            }
            if tr.is_task_status() {
                if !VALID_TASK_STATUSES.contains(&tr.task_status.as_str()) {
                    problems.push(format!(
                        "step {step_name:?} transition {i}: task_status {:?} is not in {{human,done,cancel}}",
                        tr.task_status
                    ));
                }
                continue;
            }
            if !def.has_step(&tr.step) {
                problems.push(format!(
                    "step {step_name:?} transition {i}: target step {:?} is not defined",
                    tr.step
                ));
            }
        }
    }

    if let Some(exists) = agent_exists {
        let mut seen: HashSet<&str> = HashSet::new();
        for (_, s) in &def.steps {
            if s.agent_name.is_empty() || !seen.insert(s.agent_name.as_str()) {
                continue;
            }
            if !exists(&s.agent_name) {
                problems.push(format!(
                    "agent {0:?} is referenced by a step but is not installed (run `autosk agent install {0}`)",
                    s.agent_name
                ));
            }
        }
    }

    if problems.is_empty() {
        return Ok(());
    }
    Err(Error::Invalid(format!(
        "workflow {:?} has {} problem(s):\n  - {}",
        def.name,
        problems.len(),
        problems.join("\n  - ")
    )))
}

// ---- create ---------------------------------------------------------------

/// Persists a parsed [`Definition`] transactionally (mirror of `Store.Create`).
/// `is_synthetic` bypasses the reserved-name check and pins isolation=none.
/// Returns the workflow name on success. Validation + agent resolution happen
/// up front. On a duplicate name returns [`Error::Conflict`].
pub fn create(conn: &Connection, def: &Definition, is_synthetic: bool) -> Result<String> {
    let opts = ValidateOpts {
        allow_synthetic_name: is_synthetic,
    };
    let exists_fn = |name: &str| agent_id_by_name(conn, name).ok().flatten().is_some();
    validate(def, opts, Some(&exists_fn))?;

    // Resolve agent names → ids up front.
    let mut step_agents: Vec<(String, String)> = Vec::with_capacity(def.steps.len());
    for (step_name, sd) in &def.steps {
        let aid = agent_id_by_name(conn, &sd.agent_name)?.ok_or_else(|| {
            Error::Invalid(format!(
                "resolve agent {:?} for step {step_name:?}: agent not found",
                sd.agent_name
            ))
        })?;
        step_agents.push((step_name.clone(), aid));
    }

    let isolation = normalize_isolation(&def.isolation);
    if is_synthetic && isolation != "none" {
        return Err(Error::Invalid(format!(
            "synthetic workflow {:?} cannot use isolation={isolation:?}",
            def.name
        )));
    }

    // Mint ids before the tx (mirror of Go's pre-BeginTx minting).
    let wf_id = ids::mint_unique(conn, ids::WORKFLOW_PREFIX, "workflows", "id")?;
    let mut step_ids: Vec<(String, String)> = Vec::with_capacity(def.steps.len());
    for (step_name, _) in &def.steps {
        let sid = ids::mint_unique(conn, ids::STEP_PREFIX, "steps", "id")?;
        step_ids.push((step_name.clone(), sid));
    }
    let id_of = |name: &str| -> Option<&str> {
        step_ids
            .iter()
            .find(|(n, _)| n == name)
            .map(|(_, i)| i.as_str())
    };
    let agent_of = |name: &str| -> Option<&str> {
        step_agents
            .iter()
            .find(|(n, _)| n == name)
            .map(|(_, i)| i.as_str())
    };

    let first_step_id = id_of(&def.first_step)
        .ok_or_else(|| {
            Error::Migration(format!(
                "internal: first_step {:?} has no id",
                def.first_step
            ))
        })?
        .to_string();

    let tx = conn.unchecked_transaction()?;
    let now = crate::timefmt::now_unix();
    let synthetic = if is_synthetic { 1 } else { 0 };

    let inserted = tx.execute(
        "INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, isolation, created_at) \
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
        params![wf_id, def.name, def.description, first_step_id, synthetic, isolation, now],
    );
    if let Err(e) = inserted {
        if is_unique_err(&e, "workflows.name") {
            return Err(Error::Conflict(format!(
                "workflow already exists: {}",
                def.name
            )));
        }
        return Err(Error::Sqlite(e));
    }

    // Pass 1: all steps (so forward-referencing transitions don't trip FK).
    for (seq, (step_name, sd)) in def.steps.iter().enumerate() {
        let params_json = marshal_agent_params(sd.agent_params.as_ref());
        tx.execute(
            "INSERT INTO steps(id, workflow_id, name, agent_id, seq, agent_params, max_visits) \
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            params![
                id_of(step_name).unwrap(),
                wf_id,
                step_name,
                agent_of(step_name).unwrap(),
                seq as i64,
                params_json,
                sd.max_visits
            ],
        )?;
    }
    // Pass 2: transitions.
    for (step_name, sd) in &def.steps {
        for tr in &sd.next_steps {
            let (next_id, status): (Option<&str>, Option<&str>) = if tr.is_task_status() {
                (None, Some(tr.task_status.as_str()))
            } else {
                (
                    Some(id_of(&tr.step).ok_or_else(|| {
                        Error::Migration(format!(
                            "internal: transition in step {step_name:?} targets unknown step {:?}",
                            tr.step
                        ))
                    })?),
                    None,
                )
            };
            tx.execute(
                "INSERT INTO step_transitions(step_id, next_step_id, task_status, prompt_rule) \
                 VALUES (?1, ?2, ?3, ?4)",
                params![id_of(step_name).unwrap(), next_id, status, tr.prompt_rule],
            )?;
        }
    }
    tx.commit()?;
    Ok(def.name.clone())
}

/// `Delete` — removes a workflow by name; refuses with [`Error::Conflict`] when
/// any task references it. Replays the doltlite `REINDEX workflows` workaround.
pub fn delete(conn: &Connection, name: &str) -> Result<()> {
    let wf_id: Option<String> = conn
        .query_row(
            "SELECT id FROM workflows WHERE name = ?1",
            params![name],
            |r| r.get(0),
        )
        .optional()?;
    let wf_id = wf_id.ok_or_else(|| Error::Conflict(format!("workflow not found: {name}")))?;
    let refs: i64 = conn.query_row(
        "SELECT COUNT(*) FROM tasks WHERE workflow_id = ?1",
        params![wf_id],
        |r| r.get(0),
    )?;
    if refs > 0 {
        return Err(Error::Conflict(format!(
            "workflow has tasks pointing at it; refuse delete: {refs} task(s) reference {name:?}"
        )));
    }
    conn.execute("DELETE FROM workflows WHERE id = ?1", params![wf_id])?;
    // doltlite 0.10.x phantom-UNIQUE workaround (kept for forward-compat parity).
    conn.execute_batch("REINDEX workflows")?;
    Ok(())
}

/// Looks up an agent id by name, or `Ok(None)` when absent.
pub fn agent_id_by_name(conn: &Connection, name: &str) -> Result<Option<String>> {
    Ok(conn
        .query_row(
            "SELECT id FROM agents WHERE name = ?1",
            params![name],
            |r| r.get::<_, String>(0),
        )
        .optional()?)
}

// ---- agent_params (de)serialization for persistence -----------------------

#[derive(Serialize)]
struct SerParams<'a> {
    #[serde(skip_serializing_if = "Option::is_none")]
    model: &'a Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    thinking: &'a Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    first_message: &'a Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    extra_args: &'a Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pi_extensions: &'a Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pi_skills: &'a Option<Vec<String>>,
}

/// Serialises [`AgentParams`] to the SQL `steps.agent_params` blob; `None`/zero
/// → SQL NULL (mirror of `marshalAgentParams`). `first_message_file` is never
/// persisted (it is inlined into `first_message` at parse time).
pub fn marshal_agent_params(p: Option<&AgentParams>) -> Option<String> {
    let p = p?;
    if p.is_zero() {
        return None;
    }
    let ser = SerParams {
        model: &p.model,
        thinking: &p.thinking,
        first_message: &p.first_message,
        extra_args: &p.extra_args,
        pi_extensions: &p.pi_extensions,
        pi_skills: &p.pi_skills,
    };
    serde_json::to_string(&ser).ok()
}

fn is_unique_err(e: &rusqlite::Error, key: &str) -> bool {
    e.to_string()
        .contains(&format!("UNIQUE constraint failed: {key}"))
}

// ---- synthetic single:<agent> ---------------------------------------------

/// `SyntheticName` — `single:<agent>`.
pub fn synthetic_name(agent_name: &str) -> String {
    format!("{SYNTHETIC_PREFIX}{agent_name}")
}

/// `EnsureSingle` — returns the `single:<agent>` workflow name, creating it if
/// absent (mirror of `Store.EnsureSingle`). Idempotent under a UNIQUE race.
pub fn ensure_single(conn: &Connection, agent_name: &str) -> Result<String> {
    if agent_name.is_empty() {
        return Err(Error::Invalid("agent name is empty".into()));
    }
    let name = synthetic_name(agent_name);
    let exists: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM workflows WHERE name = ?1",
            params![name],
            |r| r.get(0),
        )
        .optional()?;
    if exists.is_some() {
        return Ok(name);
    }
    let def = Definition {
        name: name.clone(),
        description: format!("Auto-generated single-agent workflow for {agent_name}."),
        first_step: "do".to_string(),
        isolation: "none".to_string(),
        steps: vec![(
            "do".to_string(),
            StepDef {
                agent_name: agent_name.to_string(),
                agent_params: None,
                next_steps: vec![
                    TransitionDef {
                        step: String::new(),
                        task_status: "done".into(),
                        prompt_rule: "When the work is complete.".into(),
                    },
                    TransitionDef {
                        step: String::new(),
                        task_status: "cancel".into(),
                        prompt_rule: "When the task cannot be completed.".into(),
                    },
                    TransitionDef {
                        step: String::new(),
                        task_status: "human".into(),
                        prompt_rule: "When you need a human decision or input.".into(),
                    },
                ],
                max_visits: 0,
            },
        )],
    };
    match create(conn, &def, true) {
        Ok(_) => Ok(name),
        Err(_) => {
            // Race: another writer won UNIQUE(name). Read back.
            let exists: Option<i64> = conn
                .query_row(
                    "SELECT 1 FROM workflows WHERE name = ?1",
                    params![name],
                    |r| r.get(0),
                )
                .optional()?;
            if exists.is_some() {
                Ok(name)
            } else {
                Err(Error::Migration(format!(
                    "ensure_single {name}: create failed"
                )))
            }
        }
    }
}

// ---- update isolation -----------------------------------------------------

/// `UpdateIsolation` — flips `workflows.isolation` on the named workflow,
/// applying the `none↔worktree` force safety matrix (mirror of
/// `Store.UpdateIsolation`). Returns a populated report even on error so the
/// caller can render partial rollbacks; the DB commit is the caller's job.
#[allow(clippy::too_many_arguments)]
pub fn update_isolation(
    conn: &Connection,
    ctx: &Ctx,
    name: &str,
    target: &str,
    force: bool,
    dry_run: bool,
    project_root: &str,
    worktrees: &dyn WorktreeManager,
) -> (wire::UpdateIsolationReport, Result<()>) {
    let target = normalize_isolation(target);
    let mut report = wire::UpdateIsolationReport {
        workflow: name.to_string(),
        ..Default::default()
    };
    if !isolation_valid(&target) {
        return (
            report,
            Err(Error::Invalid(format!(
                "invalid isolation mode {target:?} (want none|worktree)"
            ))),
        );
    }
    // Load the workflow header.
    let row: Option<(String, String, i64, Option<String>)> = match conn
        .query_row(
            "SELECT id, name, is_synthetic, isolation FROM workflows WHERE name = ?1",
            params![name],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .optional()
    {
        Ok(v) => v,
        Err(e) => return (report, Err(Error::Sqlite(e))),
    };
    let Some((wf_id, wf_name, synth, iso_raw)) = row else {
        return (
            report,
            Err(Error::Conflict(format!("workflow not found: {name}"))),
        );
    };
    let from = normalize_isolation(&iso_raw.unwrap_or_default());
    report.workflow = wf_name;
    report.from = from.clone();
    report.to = target.clone();
    report.dry_run = dry_run;
    if synth != 0 {
        return (
            report,
            Err(Error::Conflict(format!(
                "cannot update synthetic workflow: {name}"
            ))),
        );
    }
    if from == target {
        report.noop = true;
        return (report, Ok(()));
    }

    let non_terminal = match list_non_terminal_task_ids(conn, &wf_id) {
        Ok(v) => v,
        Err(e) => return (report, Err(e)),
    };
    report.non_terminal_tasks = non_terminal.clone();
    if !non_terminal.is_empty() && !force {
        return (
            report,
            Err(Error::Conflict(format!(
                "workflow has non-terminal tasks; pass --force to update ({} task(s))",
                non_terminal.len()
            ))),
        );
    }
    if !non_terminal.is_empty() && project_root.trim().is_empty() {
        return (
            report,
            Err(Error::Invalid(format!(
                "isolation flip requires a non-empty ProjectRoot when {} non-terminal task(s) reference the workflow",
                non_terminal.len()
            ))),
        );
    }

    // Plan per-task side effects.
    let mut planned_ensures: Vec<wire::EnsureRecord> = Vec::new();
    let mut planned_leftovers: Vec<wire::LeftoverWorktree> = Vec::new();
    if from == "none" && target == "worktree" && !non_terminal.is_empty() {
        for tid in &non_terminal {
            match worktree::path_for(project_root, tid) {
                Ok(path) => planned_ensures.push(wire::EnsureRecord {
                    task_id: tid.clone(),
                    path,
                    branch: worktree::branch_for(tid),
                    existing: false,
                }),
                Err(e) => {
                    return (
                        report,
                        Err(Error::Invalid(format!(
                            "plan worktree path for task {tid}: {e}"
                        ))),
                    )
                }
            }
        }
    } else if from == "worktree" && target == "none" && !non_terminal.is_empty() {
        for tid in &non_terminal {
            match worktree::path_for(project_root, tid) {
                Ok(path) => planned_leftovers.push(wire::LeftoverWorktree {
                    task_id: tid.clone(),
                    path,
                }),
                Err(e) => {
                    return (
                        report,
                        Err(Error::Invalid(format!(
                            "plan worktree path for task {tid}: {e}"
                        ))),
                    )
                }
            }
        }
    }
    report.ensured_tasks = planned_ensures.clone();
    report.leftover_worktrees = planned_leftovers.clone();

    if dry_run {
        return (report, Ok(()));
    }

    // Execute Ensures BEFORE the column flip (mirror of Go).
    if from == "none" && target == "worktree" && !planned_ensures.is_empty() {
        let mut applied: Vec<wire::EnsureRecord> = Vec::new();
        for rec in &planned_ensures {
            match worktrees.ensure(ctx, project_root, &rec.task_id, "") {
                Ok(res) => applied.push(wire::EnsureRecord {
                    task_id: rec.task_id.clone(),
                    path: res.path,
                    branch: res.branch,
                    existing: res.existing,
                }),
                Err(e) => {
                    for prev in &applied {
                        let _ = worktrees.on_terminal(ctx, project_root, &prev.task_id);
                    }
                    report.failed_task = rec.task_id.clone();
                    report.rolled_back_ensures = applied;
                    report.ensured_tasks = Vec::new();
                    return (
                        report,
                        Err(Error::Conflict(format!(
                            "worktree allocation failed; isolation column not updated: task {}: {e}",
                            rec.task_id
                        ))),
                    );
                }
            }
        }
        report.ensured_tasks = applied;
    }

    if let Err(e) = conn.execute(
        "UPDATE workflows SET isolation = ?1 WHERE id = ?2",
        params![target, wf_id],
    ) {
        if from == "none" && target == "worktree" {
            for prev in &report.ensured_tasks {
                let _ = worktrees.on_terminal(ctx, project_root, &prev.task_id);
            }
            report.rolled_back_ensures = std::mem::take(&mut report.ensured_tasks);
        }
        return (report, Err(Error::Sqlite(e)));
    }
    (report, Ok(()))
}

fn list_non_terminal_task_ids(conn: &Connection, workflow_id: &str) -> Result<Vec<String>> {
    let mut stmt = conn.prepare(
        "SELECT id FROM tasks WHERE workflow_id = ?1 AND status IN ('new','work','human') ORDER BY id ASC",
    )?;
    let rows = stmt.query_map(params![workflow_id], |r| r.get::<_, String>(0))?;
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

// ---- marshal helper used by metadata.set / update (compact, sorted) -------

/// Serialises a metadata-style map to compact JSON (sorted keys via serde_json's
/// default `BTreeMap`-backed `Map`); empty → `None` (SQL NULL). Mirror of
/// `marshalMetadata`.
pub fn marshal_metadata_map(m: &Map<String, Value>) -> Option<String> {
    if m.is_empty() {
        return None;
    }
    serde_json::to_string(m).ok()
}
