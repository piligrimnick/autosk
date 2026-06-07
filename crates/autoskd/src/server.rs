//! JSON-RPC server (plan §5) — meta + project + the read surface, over the
//! line-delimited JSON-RPC envelope.
//!
//! Transport is a synchronous thread-per-connection accept loop over the
//! [`UnixListener`] from [`crate::uds`]; no async runtime. Each connection
//! reads one request per line, dispatches it, and writes one response line.
//! Notifications (live `job-event`/`task-changed`) land in Phase 2; Phase 1 is
//! request/response only, with `job.subscribe` returning the archived
//! transcript.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::Arc;

use serde::de::DeserializeOwned;
use serde::Deserialize;
use serde_json::Value;

use autosk_core::projectmgr::{Manager, Project};
use autosk_core::read::{JobFilter, TaskFilter};
use autosk_core::registry::Registry;
use autosk_core::{transcript, Error as CoreError};
use autosk_proto::rpc::{error_codes as codes, ErrorObject, Request, Response};
use autosk_proto::wire;

/// Reported by `version`/`healthz`. Overridable at build time so a release can
/// stamp the real version/commit; defaults are fine for the Phase 1 daemon.
const VERSION: &str = match option_env!("AUTOSKD_VERSION") {
    Some(v) => v,
    None => "0.1.0-phase1",
};
const COMMIT: &str = match option_env!("AUTOSKD_COMMIT") {
    Some(v) => v,
    None => "",
};

/// The daemon's shared state: the project cache + the persisted registry.
pub struct Server {
    mgr: Arc<Manager>,
    registry: Arc<Registry>,
}

impl Server {
    /// Builds a server over a project manager and registry.
    pub fn new(mgr: Arc<Manager>, registry: Arc<Registry>) -> Server {
        Server { mgr, registry }
    }

    /// Accept loop: one detached thread per connection. Blocks until the
    /// listener is closed.
    pub fn serve(self: Arc<Self>, listener: UnixListener) {
        for incoming in listener.incoming() {
            match incoming {
                Ok(stream) => {
                    let me = Arc::clone(&self);
                    std::thread::spawn(move || me.handle_conn(stream));
                }
                Err(e) => {
                    eprintln!("autoskd: accept: {e}");
                    // Transient accept errors shouldn't kill the daemon.
                }
            }
        }
    }

    fn handle_conn(&self, stream: UnixStream) {
        let mut writer = match stream.try_clone() {
            Ok(w) => w,
            Err(e) => {
                eprintln!("autoskd: clone stream: {e}");
                return;
            }
        };
        let reader = BufReader::new(stream);
        for line in reader.lines() {
            let line = match line {
                Ok(l) => l,
                Err(_) => break, // peer hung up / read error
            };
            if line.trim().is_empty() {
                continue;
            }
            let resp = self.handle_line(&line);
            let mut buf = match serde_json::to_vec(&resp) {
                Ok(b) => b,
                Err(e) => {
                    eprintln!("autoskd: encode response: {e}");
                    continue;
                }
            };
            buf.push(b'\n');
            if writer
                .write_all(&buf)
                .and_then(|()| writer.flush())
                .is_err()
            {
                break; // peer gone
            }
        }
    }

    /// Parses one request line and dispatches it, always producing a Response.
    fn handle_line(&self, line: &str) -> Response {
        let req: Request = match serde_json::from_str(line) {
            Ok(r) => r,
            Err(e) => {
                // Best-effort id recovery so the client can correlate.
                let id = serde_json::from_str::<Value>(line)
                    .ok()
                    .and_then(|v| v.get("id").and_then(Value::as_u64))
                    .unwrap_or(0);
                return Response::err(id, codes::PARSE_ERROR, format!("parse: {e}"));
            }
        };
        match self.dispatch(&req) {
            Ok(result) => Response::ok(req.id, result),
            Err(err) => Response {
                id: req.id,
                result: None,
                error: Some(err),
            },
        }
    }

    fn dispatch(&self, req: &Request) -> Result<Value, ErrorObject> {
        let params = req.params.clone().unwrap_or(Value::Null);
        match req.method.as_str() {
            // ---- meta ----
            "version" => json(&wire::VersionInfo {
                version: VERSION.to_string(),
                commit: COMMIT.to_string(),
            }),
            "healthz" => self.healthz(&params),

            // ---- project ----
            "project.list" => json(&self.registry.list().map_err(core_err)?),
            "project.add" => self.project_add(&params),
            "project.remove" => self.project_remove(&params),

            // ---- tasks ----
            "task.list" => {
                let p: TaskListParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.task_list(&p.into_filter()).map_err(core_err)?)
            }
            "task.get" => {
                let p: IdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.task_get(&p.id).map_err(core_err)?)
            }
            "task.ready" => {
                let p: LimitParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.task_ready(p.limit).map_err(core_err)?)
            }

            // ---- comments ----
            "comment.list" => {
                let p: TaskIdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.comment_list(&p.task_id).map_err(core_err)?)
            }

            // ---- workflows ----
            "workflow.list" => {
                let p: WorkflowListParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(
                    &proj
                        .db
                        .workflow_list(p.include_synthetic)
                        .map_err(core_err)?,
                )
            }
            "workflow.get" => {
                let p: NameParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.workflow_get(&p.name).map_err(core_err)?)
            }

            // ---- agents ----
            "agent.list" => {
                let p: Selector = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.agent_list().map_err(core_err)?)
            }

            // ---- jobs ----
            "job.list" => {
                let p: JobListParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.job_list(&p.into_filter()).map_err(core_err)?)
            }
            "job.get" => {
                let p: IdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.job_get(&p.id).map_err(core_err)?)
            }
            // job.subscribe returns the archived transcript in Phase 1; live
            // tailing (server→client notifications) lands in Phase 2.
            "job.messages" | "job.subscribe" => {
                let p: MessagesParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                let sp = proj.db.job_session_path(&p.job_id).map_err(core_err)?;
                let events = match sp {
                    Some(path) => {
                        transcript::read_messages(&path, p.full, p.limit).map_err(core_err)?
                    }
                    None => Vec::new(),
                };
                json(&events)
            }

            // ---- signals ----
            "signal.forTask" => {
                let p: TaskIdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.signal_for_task(&p.task_id).map_err(core_err)?)
            }
            "signal.forJob" => {
                let p: JobIdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.signal_for_job(&p.job_id).map_err(core_err)?)
            }

            other => Err(ErrorObject {
                code: codes::METHOD_NOT_FOUND,
                message: format!("unknown method: {other}"),
                details: None,
            }),
        }
    }

    fn healthz(&self, params: &Value) -> Result<Value, ErrorObject> {
        let all = params.get("all").and_then(Value::as_bool).unwrap_or(false);
        if all {
            let mut projects = Vec::new();
            for p in self.mgr.loaded() {
                let (queued, running) = count_runs(&p)?;
                projects.push(wire::HealthProject {
                    root: p.root.clone(),
                    db_path: p.db_path.clone(),
                    queued,
                    running,
                    opened_at: p.opened_at.clone(),
                });
            }
            return json(&wire::Health {
                ok: true,
                workers: 0,
                queued: 0,
                running: 0,
                db_path: String::new(),
                project_root: String::new(),
                projects,
            });
        }
        let p: Selector = parse(params)?;
        // No selector → pure liveness probe (what auto-spawn readiness uses).
        if p.cwd.is_empty() && p.db_path.is_empty() {
            return json(&wire::Health {
                ok: true,
                workers: 0,
                queued: 0,
                running: 0,
                db_path: String::new(),
                project_root: String::new(),
                projects: Vec::new(),
            });
        }
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let (queued, running) = count_runs(&proj)?;
        json(&wire::Health {
            ok: true,
            workers: 0,
            queued,
            running,
            db_path: proj.db_path.clone(),
            project_root: proj.root.clone(),
            projects: Vec::new(),
        })
    }

    fn project_add(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: Selector = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        json(
            &self
                .registry
                .add(&proj.root, &proj.db_path)
                .map_err(core_err)?,
        )
    }

    fn project_remove(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: RemoveParams = parse(params)?;
        let root = if !p.root.is_empty() {
            p.root
        } else {
            self.resolve(&p.cwd, &p.db_path)?.root.clone()
        };
        let removed = self.registry.remove(&root).map_err(core_err)?;
        Ok(serde_json::json!({ "removed": removed }))
    }

    fn resolve(&self, cwd: &str, db_path: &str) -> Result<Arc<Project>, ErrorObject> {
        self.mgr.resolve(cwd, db_path).map_err(core_err)
    }
}

/// Counts queued/running daemon_runs for a project (for `healthz`).
fn count_runs(p: &Project) -> Result<(i64, i64), ErrorObject> {
    p.db.with_read(|conn| {
        let queued: i64 = conn.query_row(
            "SELECT COUNT(*) FROM daemon_runs WHERE status = 'queued'",
            [],
            |r| r.get(0),
        )?;
        let running: i64 = conn.query_row(
            "SELECT COUNT(*) FROM daemon_runs WHERE status = 'running'",
            [],
            |r| r.get(0),
        )?;
        Ok((queued, running))
    })
    .map_err(core_err)
}

// ---- param structs (the request side of the wire contract) ----------------

#[derive(Debug, Default, Deserialize)]
struct Selector {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
}

#[derive(Debug, Default, Deserialize)]
struct IdParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
}

#[derive(Debug, Default, Deserialize)]
struct TaskIdParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    task_id: String,
}

#[derive(Debug, Default, Deserialize)]
struct JobIdParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    job_id: String,
}

#[derive(Debug, Default, Deserialize)]
struct NameParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    name: String,
}

#[derive(Debug, Default, Deserialize)]
struct LimitParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    limit: i64,
}

#[derive(Debug, Default, Deserialize)]
struct WorkflowListParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    include_synthetic: bool,
}

#[derive(Debug, Default, Deserialize)]
struct MessagesParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    job_id: String,
    #[serde(default)]
    full: bool,
    #[serde(default)]
    limit: i64,
}

#[derive(Debug, Default, Deserialize)]
struct RemoveParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    root: String,
}

#[derive(Debug, Default, Deserialize)]
struct TaskListParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    statuses: Option<Vec<String>>,
    #[serde(default)]
    priority: Option<i64>,
    #[serde(default)]
    workflow_id: String,
    #[serde(default)]
    agent_name: String,
    #[serde(default)]
    author_name: String,
    #[serde(default)]
    step_agent_name: String,
    #[serde(default)]
    search: String,
}

impl TaskListParams {
    fn into_filter(self) -> TaskFilter {
        TaskFilter {
            statuses: self.statuses,
            priority: self.priority,
            workflow_id: self.workflow_id,
            agent_name: self.agent_name,
            author_name: self.author_name,
            step_agent_name: self.step_agent_name,
            search: self.search,
        }
    }
}

#[derive(Debug, Default, Deserialize)]
struct JobListParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    task_id: String,
    #[serde(default)]
    workflow_id: String,
    #[serde(default)]
    statuses: Vec<String>,
    #[serde(default)]
    limit: i64,
}

impl JobListParams {
    fn into_filter(self) -> JobFilter {
        JobFilter {
            task_id: self.task_id,
            workflow_id: self.workflow_id,
            statuses: self.statuses,
            limit: self.limit,
        }
    }
}

// ---- helpers --------------------------------------------------------------

fn parse<T: DeserializeOwned + Default>(params: &Value) -> Result<T, ErrorObject> {
    if params.is_null() {
        return Ok(T::default());
    }
    serde_json::from_value(params.clone()).map_err(|e| ErrorObject {
        code: codes::INVALID_PARAMS,
        message: format!("invalid params: {e}"),
        details: None,
    })
}

fn json<T: serde::Serialize>(value: &T) -> Result<Value, ErrorObject> {
    serde_json::to_value(value).map_err(|e| ErrorObject {
        code: codes::INTERNAL_ERROR,
        message: format!("encode result: {e}"),
        details: None,
    })
}

/// Maps a core error onto the JSON-RPC error code the Go side mapped to 4xx/5xx.
fn core_err(e: CoreError) -> ErrorObject {
    let (code, message) = match &e {
        CoreError::ProjectNotFound(s) => {
            (codes::PROJECT_NOT_FOUND, format!("project not found: {s}"))
        }
        CoreError::InvalidProject(s) => (codes::INVALID_PROJECT, format!("invalid project: {s}")),
        CoreError::NotFound => (codes::NOT_FOUND, "not found".to_string()),
        other => (codes::INTERNAL_ERROR, other.to_string()),
    };
    ErrorObject {
        code,
        message,
        details: None,
    }
}
