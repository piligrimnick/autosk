//! Workflow step graph reads + the atomic `EnterStep` transition — the
//! Rust port of the slices of `internal/workflow` the executor depends on
//! (`FindStepByID`/`FindStepByName`/`GetByID`, `AgentParams`, `EnterStep`).
//!
//! Workflow CREATION (parse/validate/persist) stays in Phase 3; this module
//! is read + the engine's own atomic pointer move.

use rusqlite::{params, Connection, OptionalExtension, Row};
use serde::Deserialize;
use serde_json::{Map, Value};

use crate::error::{Error, Result};
use crate::tasks::{Task, TaskPatch, STATUS_WORK};

/// Per-step agent-config overrides (mirror of `workflow.AgentParams`).
/// Pointer scalars disambiguate "absent" (`None`) from "explicitly set"
/// (`Some`, even empty string); arrays use `None` = absent.
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct AgentParams {
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub thinking: Option<String>,
    #[serde(default)]
    pub first_message: Option<String>,
    #[serde(default)]
    pub extra_args: Option<Vec<String>>,
    #[serde(default)]
    pub pi_extensions: Option<Vec<String>>,
    #[serde(default)]
    pub pi_skills: Option<Vec<String>>,
}

impl AgentParams {
    /// True when the block carries no overrides (mirror of `AgentParams.IsZero`).
    pub fn is_zero(&self) -> bool {
        self.model.is_none()
            && self.thinking.is_none()
            && self.first_message.is_none()
            && self.extra_args.is_none()
            && self.pi_extensions.is_none()
            && self.pi_skills.is_none()
    }
}

/// One `step_transitions` row (mirror of `workflow.Transition`).
#[derive(Debug, Clone)]
pub struct Transition {
    pub id: i64,
    pub next_step_id: String,
    pub task_status: String,
    pub prompt_rule: String,
    pub next_step_name: String,
}

impl Transition {
    /// True when the transition closes/parks the task rather than advancing.
    pub fn is_task_status(&self) -> bool {
        !self.task_status.is_empty()
    }
}

/// One `steps` row + its outgoing transitions (mirror of `workflow.Step`).
#[derive(Debug, Clone)]
pub struct Step {
    pub id: String,
    pub workflow_id: String,
    pub name: String,
    pub agent_id: String,
    pub agent_name: String,
    pub agent_params: Option<AgentParams>,
    pub max_visits: i64,
    pub transitions: Vec<Transition>,
}

/// The workflow header the executor needs (id + name + isolation).
#[derive(Debug, Clone)]
pub struct WorkflowMeta {
    pub id: String,
    pub name: String,
    pub description: String,
    pub isolation: String,
}

const STEP_SELECT: &str =
    "SELECT s.id, s.workflow_id, s.name, s.agent_id, a.name, s.agent_params, s.max_visits \
       FROM steps s JOIN agents a ON s.agent_id = a.id";

fn scan_step(row: &Row) -> Result<Step> {
    let params_raw: Option<String> = row.get(5)?;
    let agent_params = match params_raw {
        Some(s) if !s.trim().is_empty() => {
            let p: AgentParams = serde_json::from_str(&s)
                .map_err(|e| Error::Migration(format!("unmarshal agent_params: {e}")))?;
            if p.is_zero() {
                None
            } else {
                Some(p)
            }
        }
        _ => None,
    };
    Ok(Step {
        id: row.get(0)?,
        workflow_id: row.get(1)?,
        name: row.get(2)?,
        agent_id: row.get(3)?,
        agent_name: row.get(4)?,
        agent_params,
        max_visits: row.get(6)?,
        transitions: Vec::new(),
    })
}

/// `FindStepByID` — the step + its transitions, or [`Error::NotFound`].
pub fn find_step_by_id(conn: &Connection, step_id: &str) -> Result<Step> {
    let q = format!("{STEP_SELECT} WHERE s.id = ?1");
    let mut st = conn
        .query_row(&q, params![step_id], |row| {
            scan_step(row).map_err(|_| rusqlite::Error::QueryReturnedNoRows)
        })
        .optional()?
        .ok_or(Error::NotFound)?;
    st.transitions = load_transitions(conn, &st.id)?;
    Ok(st)
}

/// `FindStepByName` — the single step matching `(workflow_id, name)`.
pub fn find_step_by_name(conn: &Connection, workflow_id: &str, name: &str) -> Result<Step> {
    let q = format!("{STEP_SELECT} WHERE s.workflow_id = ?1 AND s.name = ?2");
    let mut st = conn
        .query_row(&q, params![workflow_id, name], |row| {
            scan_step(row).map_err(|_| rusqlite::Error::QueryReturnedNoRows)
        })
        .optional()?
        .ok_or(Error::NotFound)?;
    st.transitions = load_transitions(conn, &st.id)?;
    Ok(st)
}

/// `GetByID` header — id + name + isolation (the executor's cwd/cleanup needs).
pub fn get_workflow_by_id(conn: &Connection, workflow_id: &str) -> Result<WorkflowMeta> {
    conn.query_row(
        "SELECT id, name, description, isolation FROM workflows WHERE id = ?1",
        params![workflow_id],
        |row| {
            let iso: Option<String> = row.get(3)?;
            Ok(WorkflowMeta {
                id: row.get(0)?,
                name: row.get(1)?,
                description: row.get(2)?,
                isolation: normalize_isolation(iso),
            })
        },
    )
    .optional()?
    .ok_or(Error::NotFound)
}

fn normalize_isolation(iso: Option<String>) -> String {
    match iso {
        Some(s) if s.trim() == "worktree" => "worktree".to_string(),
        _ => "none".to_string(),
    }
}

fn load_transitions(conn: &Connection, step_id: &str) -> Result<Vec<Transition>> {
    let mut stmt = conn.prepare(
        "SELECT t.id, t.next_step_id, t.task_status, t.prompt_rule, \
                (SELECT name FROM steps WHERE id = t.next_step_id) AS next_name \
           FROM step_transitions t WHERE t.step_id = ?1 ORDER BY t.id ASC",
    )?;
    let rows = stmt.query_map(params![step_id], |row| {
        Ok(Transition {
            id: row.get(0)?,
            next_step_id: row.get::<_, Option<String>>(1)?.unwrap_or_default(),
            task_status: row.get::<_, Option<String>>(2)?.unwrap_or_default(),
            prompt_rule: row.get(3)?,
            next_step_name: row.get::<_, Option<String>>(4)?.unwrap_or_default(),
        })
    })?;
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

/// The task-write seam `EnterStep` (and the executor's advanceTask) needs.
/// [`crate::store::Db`] implements it; tests wrap it to inject failures
/// (mirror of the Go `taskStore` interface + `failingTaskStore`).
pub trait TaskWriter: Send + Sync {
    fn get_task(&self, id: &str) -> Result<Task>;
    fn update_task(&self, id: &str, p: &TaskPatch) -> Result<Task>;
    /// Atomic read-metadata → mutate → write metadata + patch. `f` is called
    /// exactly once; an `Err` from it rolls the write back.
    fn update_metadata_and_patch(
        &self,
        id: &str,
        f: &mut dyn FnMut(&mut Map<String, Value>) -> Result<()>,
        p: &TaskPatch,
    ) -> Result<Task>;
}

/// `EnterStep` — the atomic "move a task into a workflow step": bump the
/// `step_visits` counter (enforcing `max_visits`) and move the pointer
/// (`current_step_id`, `status='work'`, optional `workflow_id`) in one write.
/// Returns [`Error::MaxVisitsExceeded`] when the cap would be crossed (the
/// write rolls back, leaving counter + pointer untouched). Mirrors
/// `workflow.EnterStep`.
pub fn enter_step(
    tasks: &dyn TaskWriter,
    task_id: &str,
    step: &Step,
    workflow_id: Option<&str>,
) -> Result<()> {
    let mut patch = TaskPatch {
        status: Some(STATUS_WORK.to_string()),
        current_step_id: Some(step.id.clone()),
        ..Default::default()
    };
    if let Some(w) = workflow_id {
        if !w.is_empty() {
            patch.workflow_id = Some(w.to_string());
        }
    }
    let step_id = step.id.clone();
    let step_name = step.name.clone();
    let max = step.max_visits;
    let mut closure = move |m: &mut Map<String, Value>| -> Result<()> {
        let mut fired: Option<Error> = None;
        crate::meta::mutate_step_visits(m, |sv| {
            let current = *sv.get(&step_id).unwrap_or(&0);
            let next = current + 1;
            if max > 0 && next > max {
                fired = Some(Error::MaxVisitsExceeded {
                    step_id: step_id.clone(),
                    step_name: step_name.clone(),
                    visits: current,
                    max,
                });
                return;
            }
            sv.insert(step_id.clone(), next);
        });
        match fired {
            Some(e) => Err(e),
            None => Ok(()),
        }
    };
    tasks.update_metadata_and_patch(task_id, &mut closure, &patch)?;
    Ok(())
}
