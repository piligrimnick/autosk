//! JSON-RPC server (plan §5) — meta + project + the read surface, plus the
//! Phase-2 live job surface (`job.subscribe`/`unsubscribe` streaming,
//! `job.input`/`job.abort`/`job.cancel`).
//!
//! Transport is a synchronous thread-per-connection accept loop over the
//! [`UnixListener`] from [`crate::uds`]. Each connection reads one request per
//! line; responses and server→client `job-event` notifications share one
//! mutex-guarded writer so a live stream can push frames while requests are
//! still served on the same connection.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::Receiver;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use serde::de::DeserializeOwned;
use serde::Deserialize;
use serde_json::Value;

use autosk_core::pi::{Command, Response as PiResponse};
use autosk_core::pirunners::AttachGuard;
use autosk_core::projectmgr::Project;
use autosk_core::read::{JobFilter, TaskFilter};
use autosk_core::runstore;
use autosk_core::{transcript, Error as CoreError};
use autosk_proto::rpc::{error_codes as codes, ErrorObject, Notification, Request, Response};
use autosk_proto::wire;

use crate::daemon::Daemon;

const VERSION: &str = match option_env!("AUTOSKD_VERSION") {
    Some(v) => v,
    None => "0.2.0-phase2",
};
const COMMIT: &str = match option_env!("AUTOSKD_COMMIT") {
    Some(v) => v,
    None => "",
};

/// The daemon's RPC server over the runtime [`Daemon`].
pub struct Server {
    daemon: Arc<Daemon>,
}

/// A live subscription owned by one connection.
struct Subscription {
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
}

impl Subscription {
    fn cancel(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

impl Server {
    pub fn new(daemon: Arc<Daemon>) -> Server {
        Server { daemon }
    }

    /// Accept loop: one detached thread per connection.
    pub fn serve(self: Arc<Self>, listener: UnixListener) {
        for incoming in listener.incoming() {
            match incoming {
                Ok(stream) => {
                    let me = Arc::clone(&self);
                    std::thread::spawn(move || me.handle_conn(stream));
                }
                Err(e) => eprintln!("autoskd: accept: {e}"),
            }
        }
    }

    fn handle_conn(&self, stream: UnixStream) {
        let writer = match stream.try_clone() {
            Ok(w) => Arc::new(Mutex::new(w)),
            Err(e) => {
                eprintln!("autoskd: clone stream: {e}");
                return;
            }
        };
        let reader = BufReader::new(stream);
        let mut subs: HashMap<String, Subscription> = HashMap::new();
        for line in reader.lines() {
            let line = match line {
                Ok(l) => l,
                Err(_) => break,
            };
            if line.trim().is_empty() {
                continue;
            }
            let req: Request = match serde_json::from_str(&line) {
                Ok(r) => r,
                Err(e) => {
                    let id = serde_json::from_str::<Value>(&line)
                        .ok()
                        .and_then(|v| v.get("id").and_then(Value::as_u64))
                        .unwrap_or(0);
                    write_line(
                        &writer,
                        &Response::err(id, codes::PARSE_ERROR, format!("parse: {e}")),
                    );
                    continue;
                }
            };
            match req.method.as_str() {
                "job.subscribe" => {
                    let resp = self.start_subscription(&req, &writer, &mut subs);
                    write_line(&writer, &resp);
                }
                "job.unsubscribe" => {
                    let resp = self.stop_subscription(&req, &mut subs);
                    write_line(&writer, &resp);
                }
                _ => {
                    let resp = match self.dispatch(&req) {
                        Ok(result) => Response::ok(req.id, result),
                        Err(error) => Response {
                            id: req.id,
                            result: None,
                            error: Some(error),
                        },
                    };
                    write_line(&writer, &resp);
                }
            }
        }
        // Disconnect: tear down every live stream (releases attach guards).
        for (_, mut s) in subs.drain() {
            s.cancel();
        }
    }

    fn dispatch(&self, req: &Request) -> Result<Value, ErrorObject> {
        let params = req.params.clone().unwrap_or(Value::Null);
        match req.method.as_str() {
            "version" => json(&wire::VersionInfo {
                version: VERSION.to_string(),
                commit: COMMIT.to_string(),
            }),
            "healthz" => self.healthz(&params),

            "project.list" => json(&self.daemon.registry.list().map_err(core_err)?),
            "project.add" => self.project_add(&params),
            "project.remove" => self.project_remove(&params),

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

            "comment.list" => {
                let p: TaskIdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.comment_list(&p.task_id).map_err(core_err)?)
            }

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

            "agent.list" => {
                let p: Selector = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                json(&proj.db.agent_list().map_err(core_err)?)
            }

            "job.list" => {
                let p: JobListParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                let mut jobs = proj.db.job_list(&p.into_filter()).map_err(core_err)?;
                for j in &mut jobs {
                    self.decorate(&mut *j);
                }
                json(&jobs)
            }
            "job.get" => {
                let p: IdParams = parse(&params)?;
                let proj = self.resolve(&p.cwd, &p.db_path)?;
                let mut job = proj.db.job_get(&p.id).map_err(core_err)?;
                self.decorate(&mut job);
                json(&job)
            }
            "job.messages" => {
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
            "job.cancel" => self.job_cancel(&params),
            "job.input" => self.job_input(&params),
            "job.abort" => self.job_abort(&params),

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

    /// Fills the live `attach_count` + `streaming` fields on a decorated job.
    fn decorate(&self, job: &mut wire::Job) {
        job.attach_count = self.daemon.attachments.count(&job.job_id);
        job.streaming = self
            .daemon
            .runners
            .get(&job.job_id)
            .map(|h| h.is_streaming())
            .unwrap_or(false);
    }

    fn healthz(&self, params: &Value) -> Result<Value, ErrorObject> {
        let all = params.get("all").and_then(Value::as_bool).unwrap_or(false);
        if all {
            let mut projects = Vec::new();
            for p in self.daemon.mgr.loaded() {
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
                workers: self.daemon_workers(),
                queued: 0,
                running: 0,
                db_path: String::new(),
                project_root: String::new(),
                projects,
            });
        }
        let p: Selector = parse(params)?;
        if p.cwd.is_empty() && p.db_path.is_empty() {
            return json(&wire::Health {
                ok: true,
                workers: self.daemon_workers(),
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
            workers: self.daemon_workers(),
            queued,
            running,
            db_path: proj.db_path.clone(),
            project_root: proj.root.clone(),
            projects: Vec::new(),
        })
    }

    fn daemon_workers(&self) -> i64 {
        // Surfaced for parity with the Go health view; the value is informational.
        0
    }

    fn project_add(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: Selector = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        json(
            &self
                .daemon
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
        let removed = self.daemon.registry.remove(&root).map_err(core_err)?;
        Ok(serde_json::json!({ "removed": removed }))
    }

    // ---- job control ------------------------------------------------------

    fn job_cancel(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: JobIdParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let run = proj.db.run_get(&p.job_id).map_err(core_err)?;
        if runstore::is_terminal(&run.status) {
            // Idempotent: already terminal; return current decorated state.
            let mut job = proj.db.job_get(&p.job_id).map_err(core_err)?;
            self.decorate(&mut job);
            return json(&job);
        }
        let job = autosk_core::scheduler::Job {
            project: proj.root.clone(),
            id: p.job_id.clone(),
        };
        let was_active = self.daemon.scheduler.cancel(&job);
        if !was_active && run.status == runstore::ST_QUEUED {
            // Queued but never picked up: mark cancelled directly so a worker
            // doesn't later pick up a cancelled run.
            let _ = proj.db.run_mark_cancelled(&p.job_id, None);
        }
        let mut decorated = proj.db.job_get(&p.job_id).map_err(core_err)?;
        self.decorate(&mut decorated);
        json(&decorated)
    }

    fn job_abort(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: JobIdParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let run = proj.db.run_get(&p.job_id).map_err(core_err)?;
        if runstore::is_terminal(&run.status) {
            return Err(ErrorObject {
                code: codes::INVALID_PARAMS,
                message: "run is terminal".into(),
                details: Some(serde_json::json!({"status": run.status})),
            });
        }
        let handle = self
            .daemon
            .runners
            .get(&p.job_id)
            .ok_or_else(|| ErrorObject {
                code: codes::INVALID_PARAMS,
                message: "runner not registered for job".into(),
                details: Some(serde_json::json!({"job_id": p.job_id})),
            })?;
        handle
            .abort(&autosk_core::ctx::Ctx::background())
            .map_err(|e| ErrorObject {
                code: codes::INTERNAL_ERROR,
                message: format!("abort: {e}"),
                details: None,
            })?;
        Ok(serde_json::json!({"job_id": p.job_id, "ok": true}))
    }

    fn job_input(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: InputParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        if p.message.is_empty() {
            return Err(ErrorObject {
                code: codes::INVALID_PARAMS,
                message: "message is required".into(),
                details: None,
            });
        }
        match p.streaming_behavior.as_str() {
            "" | "steer" | "follow_up" => {}
            _ => {
                return Err(ErrorObject {
                    code: codes::INVALID_PARAMS,
                    message: "streamingBehavior must be 'steer' or 'follow_up'".into(),
                    details: None,
                })
            }
        }
        let run = proj.db.run_get(&p.job_id).map_err(core_err)?;
        if runstore::is_terminal(&run.status) {
            return Err(ErrorObject {
                code: codes::INVALID_PARAMS,
                message: "run is terminal".into(),
                details: Some(serde_json::json!({"status": run.status})),
            });
        }
        let handle = self
            .daemon
            .runners
            .get(&p.job_id)
            .ok_or_else(|| ErrorObject {
                code: codes::INVALID_PARAMS,
                message: "runner not registered for job".into(),
                details: Some(serde_json::json!({"job_id": p.job_id})),
            })?;
        let dispatched = dispatch_input(&*handle, &p.message, &p.streaming_behavior)?;
        Ok(serde_json::json!({"job_id": p.job_id, "dispatched": dispatched}))
    }

    // ---- streaming --------------------------------------------------------

    fn start_subscription(
        &self,
        req: &Request,
        writer: &Arc<Mutex<UnixStream>>,
        subs: &mut HashMap<String, Subscription>,
    ) -> Response {
        let p: SubscribeParams = match parse(&req.params.clone().unwrap_or(Value::Null)) {
            Ok(p) => p,
            Err(e) => {
                return Response {
                    id: req.id,
                    result: None,
                    error: Some(e),
                }
            }
        };
        let proj = match self.resolve(&p.cwd, &p.db_path) {
            Ok(p) => p,
            Err(e) => {
                return Response {
                    id: req.id,
                    result: None,
                    error: Some(e),
                }
            }
        };
        // Validate the job exists up-front (so a typo gets an error, not silence).
        if proj.db.run_get(&p.job_id).is_err() {
            return Response::err(req.id, codes::NOT_FOUND, "job not found");
        }
        // Replace any existing subscription for the same job on this connection.
        if let Some(mut old) = subs.remove(&p.job_id) {
            old.cancel();
        }
        let stop = Arc::new(AtomicBool::new(false));
        let guard: Option<AttachGuard> = if p.attach {
            Some(self.daemon.attachments.acquire(&p.job_id))
        } else {
            None
        };
        let daemon = Arc::clone(&self.daemon);
        let writer_c = Arc::clone(writer);
        let stop_c = Arc::clone(&stop);
        let job_id = p.job_id.clone();
        let proj_c = Arc::clone(&proj);
        let p_thread = p.clone();
        let handle = std::thread::spawn(move || {
            // The attach guard lives for the stream's lifetime.
            let _guard = guard;
            stream_job(&daemon, &proj_c, &job_id, &p_thread, &writer_c, &stop_c);
        });
        subs.insert(
            p.job_id.clone(),
            Subscription {
                stop,
                handle: Some(handle),
            },
        );
        Response::ok(
            req.id,
            serde_json::json!({"job_id": p.job_id, "subscribed": true}),
        )
    }

    fn stop_subscription(
        &self,
        req: &Request,
        subs: &mut HashMap<String, Subscription>,
    ) -> Response {
        let p: JobIdParams = match parse(&req.params.clone().unwrap_or(Value::Null)) {
            Ok(p) => p,
            Err(e) => {
                return Response {
                    id: req.id,
                    result: None,
                    error: Some(e),
                }
            }
        };
        let existed = if let Some(mut s) = subs.remove(&p.job_id) {
            s.cancel();
            true
        } else {
            false
        };
        Response::ok(
            req.id,
            serde_json::json!({"job_id": p.job_id, "unsubscribed": existed}),
        )
    }

    fn resolve(&self, cwd: &str, db_path: &str) -> Result<Arc<Project>, ErrorObject> {
        self.daemon.resolve(cwd, db_path).map_err(core_err)
    }
}

/// The per-subscription replay-then-tail loop. Emits `job-event` notifications:
/// message (with `event_id`), status, done, error — mirroring the Go SSE frames.
fn stream_job(
    daemon: &Arc<Daemon>,
    proj: &Arc<Project>,
    job_id: &str,
    p: &SubscribeParams,
    writer: &Arc<Mutex<UnixStream>>,
    stop: &Arc<AtomicBool>,
) {
    let run = match proj.db.run_get(job_id) {
        Ok(r) => r,
        Err(_) => return,
    };
    // ---- initial replay ----
    let mut cursor = replay_initial(job_id, &run.session_path, p, writer);
    // status snapshot
    emit_status(daemon, proj, job_id, writer);
    if runstore::is_terminal(&run.status) {
        emit_done(daemon, proj, job_id, writer);
        return;
    }
    // ---- tail loop ----
    let mut prev_status = run.status.clone();
    let mut prev_session = run.session_path.clone();
    let mut prev_corrections = run.corrections_used;
    let mut prev_attach = daemon.attachments.count(job_id);
    let mut prev_streaming = daemon
        .runners
        .get(job_id)
        .map(|h| h.is_streaming())
        .unwrap_or(false);
    loop {
        if stop.load(Ordering::SeqCst) {
            return;
        }
        std::thread::sleep(Duration::from_millis(200));
        let cur = match proj.db.run_get(job_id) {
            Ok(r) => r,
            Err(_) => return,
        };
        if prev_session.is_empty() && !cur.session_path.is_empty() {
            prev_session = cur.session_path.clone();
        }
        if !cur.session_path.is_empty() {
            cursor = pump_transcript(job_id, &cur.session_path, cursor, writer);
        }
        let cur_attach = daemon.attachments.count(job_id);
        let cur_streaming = daemon
            .runners
            .get(job_id)
            .map(|h| h.is_streaming())
            .unwrap_or(false);
        if cur.status != prev_status
            || cur.corrections_used != prev_corrections
            || cur_attach != prev_attach
            || cur_streaming != prev_streaming
        {
            emit_status(daemon, proj, job_id, writer);
            prev_status = cur.status.clone();
            prev_corrections = cur.corrections_used;
            prev_attach = cur_attach;
            prev_streaming = cur_streaming;
        }
        if runstore::is_terminal(&cur.status) {
            emit_done(daemon, proj, job_id, writer);
            return;
        }
    }
}

/// Computes the replay start index (port of `replayStartIndex`) and emits the
/// initial message frames; returns the new cursor (events seen).
fn replay_initial(
    job_id: &str,
    session_path: &str,
    p: &SubscribeParams,
    writer: &Arc<Mutex<UnixStream>>,
) -> usize {
    if session_path.is_empty() {
        return 0;
    }
    let all = transcript::read_messages(session_path, true, 0).unwrap_or_default();
    let total = all.len();
    let start = replay_start_index(p.from_event_id, p.full, p.limit, total);
    for (i, ev) in all.iter().enumerate().skip(start) {
        emit_message(job_id, (i + 1) as i64, ev, writer);
    }
    total
}

fn replay_start_index(skip: usize, full: bool, limit: i64, total: usize) -> usize {
    if skip >= total {
        return total;
    }
    if skip > 0 {
        return skip;
    }
    if full {
        return 0;
    }
    if limit > 0 && (limit as usize) < total {
        return total - limit as usize;
    }
    0
}

fn pump_transcript(
    job_id: &str,
    session_path: &str,
    cursor: usize,
    writer: &Arc<Mutex<UnixStream>>,
) -> usize {
    let all = match transcript::read_messages(session_path, true, 0) {
        Ok(a) => a,
        Err(_) => return cursor,
    };
    if cursor >= all.len() {
        return cursor;
    }
    for (i, ev) in all.iter().enumerate().skip(cursor) {
        emit_message(job_id, (i + 1) as i64, ev, writer);
    }
    all.len()
}

fn emit_message(
    job_id: &str,
    event_id: i64,
    ev: &wire::MessageEvent,
    writer: &Arc<Mutex<UnixStream>>,
) {
    let note = Notification {
        method: "job-event".into(),
        params: serde_json::to_value(wire::JobEvent {
            kind: "message".into(),
            job_id: job_id.into(),
            event_id,
            event: Some(ev.clone()),
            job: None,
            error: String::new(),
        })
        .unwrap_or(Value::Null),
    };
    write_note(writer, &note);
}

fn emit_status(
    daemon: &Arc<Daemon>,
    proj: &Arc<Project>,
    job_id: &str,
    writer: &Arc<Mutex<UnixStream>>,
) {
    emit_job_frame(daemon, proj, job_id, "status", writer);
}
fn emit_done(
    daemon: &Arc<Daemon>,
    proj: &Arc<Project>,
    job_id: &str,
    writer: &Arc<Mutex<UnixStream>>,
) {
    emit_job_frame(daemon, proj, job_id, "done", writer);
}

fn emit_job_frame(
    daemon: &Arc<Daemon>,
    proj: &Arc<Project>,
    job_id: &str,
    kind: &str,
    writer: &Arc<Mutex<UnixStream>>,
) {
    let Ok(mut job) = proj.db.job_get(job_id) else {
        return;
    };
    job.attach_count = daemon.attachments.count(job_id);
    job.streaming = daemon
        .runners
        .get(job_id)
        .map(|h| h.is_streaming())
        .unwrap_or(false);
    let note = Notification {
        method: "job-event".into(),
        params: serde_json::to_value(wire::JobEvent {
            kind: kind.into(),
            job_id: job_id.into(),
            event_id: 0,
            event: None,
            job: Some(job),
            error: String::new(),
        })
        .unwrap_or(Value::Null),
    };
    write_note(writer, &note);
}

// ---- job.input dispatch (state-mismatch retry) ----------------------------

fn dispatch_input(
    handle: &dyn autosk_core::runner::PiRunner,
    message: &str,
    behavior: &str,
) -> Result<String, ErrorObject> {
    let streaming = handle.is_streaming();
    let (cmd, dispatched) = build_input_command(message, behavior, streaming);
    let resp = send_and_wait(handle, cmd)?;
    if resp.success {
        return Ok(dispatched);
    }
    if !is_state_mismatch(&resp.error) {
        return Err(ErrorObject {
            code: codes::INVALID_PARAMS,
            message: format!("pi rejected {dispatched}: {}", resp.error),
            details: Some(
                serde_json::json!({"streaming": streaming, "pi_error": resp.error, "retry": false}),
            ),
        });
    }
    // State-mismatch: retry once with the opposite dispatch shape.
    let (retry_cmd, retry_dispatched) = build_input_command(message, behavior, !streaming);
    let retry = send_and_wait(handle, retry_cmd)?;
    if retry.success {
        return Ok(retry_dispatched);
    }
    Err(ErrorObject {
        code: codes::INVALID_PARAMS,
        message: format!(
            "pi rejected {retry_dispatched} after retry from {dispatched}: {}",
            retry.error
        ),
        details: Some(serde_json::json!({
            "first_attempt": dispatched,
            "first_error": resp.error,
            "retry_attempt": retry_dispatched,
            "retry_error": retry.error,
        })),
    })
}

fn build_input_command(message: &str, behavior: &str, streaming: bool) -> (Command, String) {
    if !streaming {
        return (
            Command {
                typ: "prompt".into(),
                message: message.into(),
                ..Default::default()
            },
            "prompt".into(),
        );
    }
    match behavior {
        "follow_up" => (
            Command {
                typ: "follow_up".into(),
                message: message.into(),
                ..Default::default()
            },
            "follow_up".into(),
        ),
        _ => (
            Command {
                typ: "steer".into(),
                message: message.into(),
                ..Default::default()
            },
            "steer".into(),
        ),
    }
}

fn send_and_wait(
    handle: &dyn autosk_core::runner::PiRunner,
    cmd: Command,
) -> Result<PiResponse, ErrorObject> {
    let rx: Receiver<PiResponse> = handle.send_command(cmd).map_err(|e| ErrorObject {
        code: codes::INTERNAL_ERROR,
        message: format!("send command: {e}"),
        details: None,
    })?;
    match rx.recv_timeout(Duration::from_secs(30)) {
        Ok(r) => Ok(r),
        Err(std::sync::mpsc::RecvTimeoutError::Timeout) => Err(ErrorObject {
            code: codes::INTERNAL_ERROR,
            message: "timed out waiting for pi ack".into(),
            details: None,
        }),
        Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => Err(ErrorObject {
            code: codes::INTERNAL_ERROR,
            message: "runner closed before reply".into(),
            details: None,
        }),
    }
}

/// Conservative state-mismatch detector (port of `isStateMismatchError`).
fn is_state_mismatch(s: &str) -> bool {
    if s.is_empty() {
        return false;
    }
    let lower = s.to_lowercase();
    const TOKENS: [&str; 9] = [
        "not streaming",
        "already streaming",
        "no run",
        "no active run",
        "no_active_run",
        "idle",
        "in_progress",
        "state mismatch",
        "state_mismatch",
    ];
    TOKENS.iter().any(|t| lower.contains(t))
}

// ---- wire helpers ---------------------------------------------------------

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

fn write_line(writer: &Arc<Mutex<UnixStream>>, resp: &Response) {
    let Ok(mut buf) = serde_json::to_vec(resp) else {
        return;
    };
    buf.push(b'\n');
    let mut w = writer.lock().unwrap();
    let _ = w.write_all(&buf).and_then(|()| w.flush());
}

fn write_note(writer: &Arc<Mutex<UnixStream>>, note: &Notification) {
    let Ok(mut buf) = serde_json::to_vec(note) else {
        return;
    };
    buf.push(b'\n');
    let mut w = writer.lock().unwrap();
    let _ = w.write_all(&buf).and_then(|()| w.flush());
}

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

// ---- param structs --------------------------------------------------------

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

#[derive(Debug, Default, Clone, Deserialize)]
struct SubscribeParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    job_id: String,
    #[serde(default)]
    attach: bool,
    #[serde(default)]
    full: bool,
    #[serde(default)]
    limit: i64,
    #[serde(default)]
    from_event_id: usize,
}

#[derive(Debug, Default, Deserialize)]
struct InputParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    job_id: String,
    #[serde(default)]
    message: String,
    #[serde(default)]
    streaming_behavior: String,
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
