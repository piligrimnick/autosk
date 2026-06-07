//! Wire projection types (plan §5).
//!
//! These mirror the Go `internal/daemon/api` response structs (job/health/
//! version — they already had JSON tags) and the `internal/lazy/datasource`
//! projection structs (task/workflow/agent/comment/signal — this crate defines
//! their on-disk JSON contract for the first time).

use serde::{Deserialize, Serialize};
use serde_json::Value;

fn is_false(b: &bool) -> bool {
    !*b
}

/// `task.get` / `task.list` / `task.ready` element — the enriched task view the
/// lazy TUI consumes (`datasource.Task`). Derived fields (`*_name`, `blocked`,
/// `blocked_by`, `comment_count`) are resolved server-side so the client never
/// joins by hand.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct TaskView {
    pub id: String,
    pub title: String,
    pub description: String,
    pub status: String,
    pub priority: i64,
    pub author_id: String,
    pub author_name: String,
    pub workflow_id: String,
    pub workflow_name: String,
    pub current_step_id: String,
    pub step_name: String,
    pub agent_name: String,
    pub blocked: bool,
    pub blocked_by: Vec<TaskRef>,
    pub blocks: Vec<TaskRef>,
    pub comment_count: i64,
    /// Raw `tasks.metadata` JSON object; `null` when the column is SQL NULL.
    pub metadata: Option<Value>,
    pub created_at: String,
    pub updated_at: String,
}

/// A lightweight reference to a related task (`datasource.TaskRef`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct TaskRef {
    pub id: String,
    pub status: String,
}

/// `job.get` / `job.list` element. Mirrors `api.JobResponse` JSON tags verbatim
/// plus the three derived label fields from `datasource.Job`.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Job {
    pub job_id: String,
    pub task_id: String,
    pub step_id: String,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub transition_id: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub pi_session_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub session_path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pid: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    pub corrections_used: i64,
    pub max_corrections: i64,
    pub created_at: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub started_at: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub finished_at: Option<String>,
    pub duration_ms: i64,
    pub attach_count: i64,
    pub streaming: bool,
    // Derived labels (datasource.Job).
    pub workflow_name: String,
    pub step_name: String,
    pub agent_name: String,
}

/// `workflow.list` / `workflow.get` element (`datasource.Workflow`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Workflow {
    pub id: String,
    pub name: String,
    pub description: String,
    pub is_synthetic: bool,
    pub first_step: String,
    pub steps: Vec<WorkflowStep>,
    pub task_count: i64,
    pub isolation: String,
    pub non_terminal_task_count: i64,
    pub non_terminal_tasks: Vec<NonTerminalTaskRef>,
}

/// One row of a workflow's step graph (`datasource.WorkflowStep`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct WorkflowStep {
    pub id: String,
    pub name: String,
    pub agent_name: String,
    pub next_steps: Vec<String>,
    pub next_status: Vec<String>,
    pub task_count: i64,
}

/// One non-terminal task referencing a workflow (`datasource.NonTerminalTaskRef`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct NonTerminalTaskRef {
    pub id: String,
    pub status: String,
    pub step_name: String,
}

/// `agent.list` element (`datasource.Agent`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Agent {
    pub id: String,
    pub name: String,
    pub is_human: bool,
    pub source: String,
    pub version: String,
    pub model: String,
    pub thinking: String,
    pub extra_args: Vec<String>,
    pub pi_skills: Vec<String>,
    pub pi_ext: Vec<String>,
    pub tasks_owned: i64,
}

/// `comment.list` element (`datasource.Comment`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Comment {
    pub id: i64,
    pub task_id: String,
    pub author_id: String,
    pub author_name: String,
    pub text: String,
    pub created_at: String,
}

/// `signal.forTask` / `signal.forJob` element (`datasource.Signal`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Signal {
    pub transition_id: i64,
    pub task_id: String,
    pub job_id: String,
    pub step_id: String,
    pub step_name: String,
    pub workflow_id: String,
    pub workflow_name: String,
    pub target: String,
    pub agent_id: String,
    pub agent_name: String,
    pub created_at: String,
}

/// `job.messages` element (`api.MessageEvent`). One projected transcript event.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct MessageEvent {
    pub kind: String,
    /// RFC3339 UTC; omitted when the source entry carried no timestamp (Go
    /// renders a zero `time.Time`; the RPC contract omits instead).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ts: Option<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input: Option<Value>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub is_error: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw: Option<Value>,
}

/// `healthz` result (`api.HealthResponse`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Health {
    pub ok: bool,
    pub workers: i64,
    pub queued: i64,
    pub running: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub db_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_root: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub projects: Vec<HealthProject>,
}

/// One row of the aggregated health view (`api.HealthProject`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct HealthProject {
    pub root: String,
    pub db_path: String,
    pub queued: i64,
    pub running: i64,
    pub opened_at: String,
}

/// `version` result (`api.VersionResponse`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct VersionInfo {
    pub version: String,
    pub commit: String,
}

/// `project.list` / `project.add` element. Backed by the persisted registry at
/// `~/.autosk/projects.json` (plan §7.4).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ProjectInfo {
    pub root: String,
    pub db_path: String,
    pub name: String,
}
