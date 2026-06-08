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
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpListener;
use std::os::unix::net::UnixListener;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::Receiver;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use serde::de::DeserializeOwned;
use serde::Deserialize;
use serde_json::Value;

use autosk_core::ctx::Ctx;
use autosk_core::pi::{Command, Response as PiResponse};
use autosk_core::pirunners::AttachGuard;
use autosk_core::projectmgr::Project;
use autosk_core::read::{JobFilter, TaskFilter};
use autosk_core::runstore;
use autosk_core::verbs::{self, CreateParams, Source};
use autosk_core::worktree::WorktreeManager;
use autosk_core::{transcript, Error as CoreError};
use autosk_proto::rpc::{error_codes as codes, ErrorObject, Notification, Request, Response};
use autosk_proto::wire;

use crate::daemon::Daemon;
use crate::notify::SharedWriter;

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
    /// Expected TCP auth token (`Some` enables the TCP `auth` handshake; UDS is
    /// always exempt). `None` rejects every TCP connection.
    token: Option<String>,
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

impl Drop for Subscription {
    /// Safety net for stream teardown: if `handle_conn` unwinds (e.g. a
    /// poisoned-mutex panic in `write_line`/`write_note`) before the explicit
    /// `subs.drain()`, dropping the map here still stops the stream thread and
    /// releases its [`AttachGuard`] — so the attach count can never get pinned
    /// `> 0` and disarm a job's idle-timeout forever. Mirrors Go's `defer rel()`.
    fn drop(&mut self) {
        self.cancel();
    }
}

impl Server {
    pub fn new(daemon: Arc<Daemon>) -> Server {
        Server {
            daemon,
            token: None,
        }
    }

    /// Sets the TCP auth token (enables the TCP `auth` handshake).
    pub fn with_token(mut self, token: Option<String>) -> Server {
        self.token = token;
        self
    }

    /// Accept loop over the UDS (auth-exempt). One detached thread per conn.
    pub fn serve(self: Arc<Self>, listener: UnixListener) {
        for incoming in listener.incoming() {
            match incoming {
                Ok(stream) => {
                    let me = Arc::clone(&self);
                    std::thread::spawn(move || {
                        let w = match stream.try_clone() {
                            Ok(w) => w,
                            Err(e) => {
                                eprintln!("autoskd: clone stream: {e}");
                                return;
                            }
                        };
                        me.handle_conn(BufReader::new(stream), Box::new(w), false);
                    });
                }
                Err(e) => eprintln!("autoskd: accept: {e}"),
            }
        }
    }

    /// Accept loop over TCP (every connection must `auth{token}` first).
    pub fn serve_tcp(self: Arc<Self>, listener: TcpListener) {
        for incoming in listener.incoming() {
            match incoming {
                Ok(stream) => {
                    let me = Arc::clone(&self);
                    std::thread::spawn(move || {
                        let w = match stream.try_clone() {
                            Ok(w) => w,
                            Err(e) => {
                                eprintln!("autoskd: clone tcp stream: {e}");
                                return;
                            }
                        };
                        me.handle_conn(BufReader::new(stream), Box::new(w), true);
                    });
                }
                Err(e) => eprintln!("autoskd: tcp accept: {e}"),
            }
        }
    }

    fn handle_conn<R: Read>(
        &self,
        reader: BufReader<R>,
        write_half: Box<dyn Write + Send>,
        require_auth: bool,
    ) {
        // Count this connection for the idle-shutdown "no connected clients"
        // predicate (plan §4.2). The guard releases the count on drop — including
        // an unwind — so the daemon can never get pinned awake by a leaked count.
        let _conn_guard = self.daemon.conn_guard();
        let writer: SharedWriter = Arc::new(Mutex::new(write_half));
        // Notifications (`task-changed`/`project-changed`) are OPT-IN: a
        // connection only receives them after `task.subscribe`/`project.subscribe`
        // (plan §5). Plain request/response clients (the CLI) never see
        // unsolicited frames on their connection.
        let mut note_sub: Option<u64> = None;
        let mut authed = !require_auth;
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
            // TCP auth handshake: until authenticated, only `auth` is served.
            if !authed {
                if req.method == "auth" {
                    let resp = self.check_auth(&req);
                    if resp.error.is_none() {
                        authed = true;
                    }
                    write_line(&writer, &resp);
                } else {
                    write_line(
                        &writer,
                        &Response::err(req.id, codes::INVALID_REQUEST, "auth required"),
                    );
                }
                continue;
            }
            match req.method.as_str() {
                "auth" => {
                    // Already authenticated (or UDS): treat as a no-op success.
                    write_line(
                        &writer,
                        &Response::ok(req.id, serde_json::json!({"ok": true})),
                    );
                }
                "shutdown" => {
                    write_line(
                        &writer,
                        &Response::ok(req.id, serde_json::json!({"ok": true})),
                    );
                    // Flush, then tear down and exit the process so the UDS is
                    // released and clients fall back to spawning a fresh daemon.
                    self.daemon.shutdown();
                    std::thread::spawn(|| {
                        std::thread::sleep(Duration::from_millis(50));
                        std::process::exit(0);
                    });
                }
                "job.subscribe" => {
                    let resp = self.start_subscription(&req, &writer, &mut subs);
                    write_line(&writer, &resp);
                }
                "job.unsubscribe" => {
                    let resp = self.stop_subscription(&req, &mut subs);
                    write_line(&writer, &resp);
                }
                "task.subscribe" | "project.subscribe" => {
                    if note_sub.is_none() {
                        note_sub = Some(self.daemon.hub.register(Arc::clone(&writer)));
                    }
                    write_line(
                        &writer,
                        &Response::ok(req.id, serde_json::json!({"subscribed": true})),
                    );
                }
                "task.unsubscribe" | "project.unsubscribe" => {
                    if let Some(id) = note_sub.take() {
                        self.daemon.hub.unregister(id);
                    }
                    write_line(
                        &writer,
                        &Response::ok(req.id, serde_json::json!({"unsubscribed": true})),
                    );
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
                    let ok = resp.error.is_none();
                    write_line(&writer, &resp);
                    // Eager `task-changed` after a successful task-mutating write
                    // (race-free); the change poller additionally covers the
                    // daemon's own executor-driven advances.
                    if ok && is_task_write(&req.method) {
                        self.broadcast_task_changed(req.params.as_ref());
                    }
                }
            }
        }
        // Disconnect: tear down every live stream (releases attach guards) and
        // drop the hub registration so notifications stop targeting this conn.
        for (_, mut s) in subs.drain() {
            s.cancel();
        }
        if let Some(id) = note_sub {
            self.daemon.hub.unregister(id);
        }
    }

    /// Broadcasts `task-changed` for the project named by a write request's
    /// selector (best-effort; resolves the canonical root/db for the payload).
    fn broadcast_task_changed(&self, params: Option<&Value>) {
        let cwd = params
            .and_then(|p| p.get("cwd"))
            .and_then(Value::as_str)
            .unwrap_or("");
        let db = params
            .and_then(|p| p.get("db_path"))
            .and_then(Value::as_str)
            .unwrap_or("");
        if let Ok(proj) = self.daemon.resolve(cwd, db) {
            self.daemon.hub.task_changed(&proj.root, &proj.db_path);
        }
    }

    /// Validates an `auth{token}` request against the configured token.
    fn check_auth(&self, req: &Request) -> Response {
        let supplied = req
            .params
            .as_ref()
            .and_then(|p| p.get("token"))
            .and_then(Value::as_str)
            .unwrap_or("");
        match &self.token {
            Some(expected) if !expected.is_empty() && supplied == expected => {
                Response::ok(req.id, serde_json::json!({"ok": true}))
            }
            _ => Response::err(req.id, codes::INVALID_REQUEST, "invalid or missing token"),
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

            // ---- writes (Phase 3) -------------------------------------
            "task.create" => self.task_create(&params),
            "task.update" => self.task_update(&params),
            "task.done" => self.task_terminal(&params, "done"),
            "task.cancel" => self.task_terminal(&params, "cancel"),
            "task.reopen" => self.task_reopen(&params),
            "task.setStatus" => self.task_set_status(&params),
            "task.setTitleDescription" => self.task_set_title_desc(&params),
            "task.setPriority" => self.task_set_priority(&params),
            "task.enroll" => self.task_enroll(&params),
            "task.resume" => self.task_resume(&params),
            "task.block" => self.task_block(&params),
            "task.unblock" => self.task_unblock(&params),
            "task.unblockAll" => self.task_unblock_all(&params),
            "task.metadata.set" => self.task_metadata_set(&params),
            "task.metadata.unset" => self.task_metadata_unset(&params),
            "task.metadata.resetVisits" => self.task_metadata_reset_visits(&params),
            "comment.add" => self.comment_add(&params),
            "workflow.create" => self.workflow_create(&params),
            "workflow.delete" => self.workflow_delete(&params),
            "workflow.updateIsolation" => self.workflow_update_isolation(&params),
            "agent.install" => self.agent_install(&params),
            "agent.uninstall" => self.agent_uninstall(&params),
            "sql.query" => self.sql_query(&params),
            "sql.exec" => self.sql_exec(&params),
            "step.next" => self.step_next(&params),
            "maint.compact" => self.maint_compact(&params),
            "project.init" => self.project_init(&params),

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
        let info = self
            .daemon
            .registry
            .add(&proj.root, &proj.db_path)
            .map_err(core_err)?;
        self.daemon.hub.project_changed();
        json(&info)
    }

    fn project_remove(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: RemoveParams = parse(params)?;
        let root = if !p.root.is_empty() {
            p.root
        } else {
            self.resolve(&p.cwd, &p.db_path)?.root.clone()
        };
        let removed = self.daemon.registry.remove(&root).map_err(core_err)?;
        if removed {
            self.daemon.hub.project_changed();
        }
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
        // Fire the per-job cancel token (interrupts a running executor; no-op
        // when not active). For a queued run, mark it cancelled directly —
        // REGARDLESS of the active result — so a worker can't later pick up a
        // cancelled row. Doing this unconditionally for queued runs (not gated
        // on `!was_active`) mirrors Go and closes the tiny window where the run
        // is in the scheduler's `active` map while its DB row still reads
        // 'queued'.
        let _was_active = self.daemon.scheduler.cancel(&job);
        if run.status == runstore::ST_QUEUED {
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
            // Retryable-not-applicable, not a malformed request: mirror Go's 409.
            return Err(ErrorObject {
                code: codes::CONFLICT,
                message: "run is terminal".into(),
                details: Some(serde_json::json!({"status": run.status, "retry": false})),
            });
        }
        let handle = self
            .daemon
            .runners
            .get(&p.job_id)
            .ok_or_else(|| ErrorObject {
                code: codes::CONFLICT,
                message: "runner not registered for job (not ready yet)".into(),
                details: Some(serde_json::json!({"job_id": p.job_id, "retry": true})),
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
            // Retryable-not-applicable, not a malformed request: mirror Go's 409.
            return Err(ErrorObject {
                code: codes::CONFLICT,
                message: "run is terminal".into(),
                details: Some(serde_json::json!({"status": run.status, "retry": false})),
            });
        }
        let handle = self
            .daemon
            .runners
            .get(&p.job_id)
            .ok_or_else(|| ErrorObject {
                code: codes::CONFLICT,
                message: "runner not registered for job (not ready yet)".into(),
                details: Some(serde_json::json!({"job_id": p.job_id, "retry": true})),
            })?;
        let dispatched = dispatch_input(&*handle, &p.message, &p.streaming_behavior)?;
        Ok(serde_json::json!({"job_id": p.job_id, "dispatched": dispatched}))
    }

    // ---- streaming --------------------------------------------------------

    fn start_subscription(
        &self,
        req: &Request,
        writer: &SharedWriter,
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
        // Validate the job exists up-front (so a typo gets an error, not
        // silence). Discriminate a genuine not-found from a transient DB error
        // so a read failure isn't mis-reported to the client as a missing job
        // (mirror of Go's 404-vs-500 split).
        match proj.db.run_get(&p.job_id) {
            Ok(_) => {}
            Err(CoreError::NotFound) => {
                return Response::err(req.id, codes::NOT_FOUND, "job not found")
            }
            Err(e) => {
                return Response {
                    id: req.id,
                    result: None,
                    error: Some(core_err(e)),
                }
            }
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

// ---- write handlers (Phase 3) ---------------------------------------------

impl Server {
    fn worktrees(&self) -> &dyn WorktreeManager {
        self.daemon.worktree.as_ref()
    }

    fn task_create(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: TaskCreateParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let view = verbs::create(
            &proj,
            &self.daemon.packages,
            self.worktrees(),
            &Ctx::background(),
            Source::parse(&p.source),
            CreateParams {
                title: p.title,
                description: p.description,
                priority: p.priority.unwrap_or(2),
                blocks: p.blocks,
                blocked_by: p.blocked_by,
                workflow: p.workflow,
                agent: p.agent,
                step: p.step,
                base_ref: p.base_ref,
                caller: p.caller,
            },
        )
        .map_err(core_err)?;
        json(&view)
    }

    fn task_update(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: TaskUpdateParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let view = verbs::update(
            &proj,
            self.worktrees(),
            &Ctx::background(),
            &p.id,
            p.title,
            p.description,
            p.priority,
            p.status,
        )
        .map_err(core_err)?;
        json(&view)
    }

    fn task_terminal(&self, params: &Value, which: &str) -> Result<Value, ErrorObject> {
        let p: IdParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let view = if which == "done" {
            verbs::done(&proj, self.worktrees(), &Ctx::background(), &p.id)
        } else {
            verbs::cancel(&proj, self.worktrees(), &Ctx::background(), &p.id)
        }
        .map_err(core_err)?;
        json(&view)
    }

    fn task_reopen(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: IdParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        json(&verbs::reopen(&proj, &p.id).map_err(core_err)?)
    }

    fn task_set_status(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: SetStatusParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let view = verbs::lazy_update_status(
            &proj,
            self.worktrees(),
            &Ctx::background(),
            &p.id,
            &p.status,
        )
        .map_err(core_err)?;
        json(&view)
    }

    fn task_set_title_desc(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: TitleDescParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        json(
            &verbs::lazy_update_title_description(&proj, &p.id, &p.title, &p.description)
                .map_err(core_err)?,
        )
    }

    fn task_set_priority(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: PriorityParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        json(&verbs::lazy_update_priority(&proj, &p.id, p.priority).map_err(core_err)?)
    }

    fn task_enroll(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: EnrollParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let view = verbs::enroll(
            &proj,
            &self.daemon.packages,
            self.worktrees(),
            &Ctx::background(),
            Source::parse(&p.source),
            &p.id,
            &p.workflow,
            &p.agent,
            &p.step,
            &p.base_ref,
        )
        .map_err(core_err)?;
        json(&view)
    }

    fn task_resume(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: ResumeParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        json(&verbs::resume(&proj, Source::parse(&p.source), &p.id, &p.to_step).map_err(core_err)?)
    }

    fn task_block(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: BlockParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        verbs::block(&proj, Source::parse(&p.source), &p.id, &p.blockers).map_err(core_err)?;
        Ok(serde_json::json!({"ok": true}))
    }

    fn task_unblock(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: BlockParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        verbs::unblock(&proj, Source::parse(&p.source), &p.id, &p.blockers).map_err(core_err)?;
        Ok(serde_json::json!({"ok": true}))
    }

    fn task_unblock_all(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: IdParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let n = verbs::unblock_all(&proj, &p.id).map_err(core_err)?;
        Ok(serde_json::json!({"removed": n}))
    }

    fn task_metadata_set(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: MetadataSetParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let r = verbs::metadata_set(
            &proj,
            Source::parse(&p.source),
            &p.id,
            &p.key,
            p.value,
            p.replace_all,
        )
        .map_err(core_err)?;
        Ok(serde_json::json!({"task": r.task, "changed": r.changed}))
    }

    fn task_metadata_unset(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: MetadataKeyParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let r = verbs::metadata_unset(&proj, &p.id, &p.key).map_err(core_err)?;
        Ok(serde_json::json!({"task": r.task, "changed": r.changed}))
    }

    fn task_metadata_reset_visits(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: ResetVisitsParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let r =
            verbs::metadata_reset_visits(&proj, &p.id, &p.step, &p.step_id).map_err(core_err)?;
        Ok(serde_json::json!({"task": r.task, "changed": r.changed}))
    }

    fn comment_add(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: CommentAddParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let c = verbs::comment_add(
            &proj,
            Source::parse(&p.source),
            &p.task_id,
            &p.author,
            &p.text,
        )
        .map_err(core_err)?;
        json(&c)
    }

    fn workflow_create(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: WorkflowCreateParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let name = verbs::workflow_create(
            &proj,
            &self.daemon.packages,
            Source::parse(&p.source),
            &p.file,
            &p.json,
            p.no_install,
        )
        .map_err(core_err)?;
        Ok(serde_json::json!({"name": name}))
    }

    fn workflow_delete(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: WorkflowDeleteParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        verbs::workflow_delete(&proj, Source::parse(&p.source), &p.name).map_err(core_err)?;
        Ok(serde_json::json!({"ok": true}))
    }

    fn workflow_update_isolation(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: WorkflowIsolationParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let (report, res) = verbs::workflow_update_isolation(
            &proj,
            self.worktrees(),
            &Ctx::background(),
            Source::parse(&p.source),
            &p.name,
            &p.mode,
            p.force,
            p.dry_run,
        );
        match res {
            Ok(()) => json(&report),
            Err(e) => {
                // Surface the (partial) force-safety report on the error path so
                // `workflow update --json` and the lazy TUI can render the
                // rollback/leftover diagnostics, not just a bare message
                // (parity with Go's `(out, err)`).
                let mut err = core_err(e);
                let report_val = serde_json::to_value(&report).unwrap_or(Value::Null);
                err.details = Some(serde_json::json!({ "report": report_val }));
                Err(err)
            }
        }
    }

    fn agent_install(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: AgentInstallParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let a = verbs::agent_install(&proj, &self.daemon.packages, &p.name, &p.version)
            .map_err(core_err)?;
        json(&a)
    }

    fn agent_uninstall(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: AgentUninstallParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        verbs::agent_uninstall(&proj, &self.daemon.packages, &p.name, p.force).map_err(core_err)?;
        Ok(serde_json::json!({"ok": true}))
    }

    fn sql_query(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: SqlParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let r = verbs::sql_query(&proj, &p.query).map_err(core_err)?;
        Ok(serde_json::json!({"columns": r.columns, "rows": r.rows}))
    }

    fn sql_exec(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: SqlParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let n = verbs::sql_exec(&proj, &p.query).map_err(core_err)?;
        Ok(serde_json::json!({"rows_affected": n}))
    }

    fn step_next(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: StepNextParams = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let e = verbs::step_next(&proj, &p.id, &p.to).map_err(core_err)?;
        Ok(serde_json::json!({
            "run_id": e.run_id,
            "task_id": e.task_id,
            "transition_id": e.transition_id,
            "next_step_name": e.next_step_name,
            "task_status": e.task_status,
            "prompt_rule": e.prompt_rule,
            "created_at": autosk_core::timefmt::rfc3339_utc(e.created_at),
        }))
    }

    fn maint_compact(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: Selector = parse(params)?;
        let proj = self.resolve(&p.cwd, &p.db_path)?;
        let g = verbs::compact(&proj).map_err(core_err)?;
        Ok(serde_json::json!({
            "chunks_removed": g.chunks_removed,
            "chunks_kept": g.chunks_kept,
            "raw": g.raw,
        }))
    }

    fn project_init(&self, params: &Value) -> Result<Value, ErrorObject> {
        let p: ProjectInitParams = parse(params)?;
        // project.init may target a fresh dir without an existing .autosk/db, so
        // resolve via Manager::init when the selector does not yet resolve.
        let proj = match self.daemon.resolve(&p.cwd, &p.db_path) {
            Ok(pr) => pr,
            Err(CoreError::ProjectNotFound(_)) => {
                let dir = if !p.cwd.is_empty() {
                    p.cwd.clone()
                } else {
                    ".".to_string()
                };
                autosk_core::projectmgr::Manager::init(&dir).map_err(core_err)?;
                self.daemon.resolve(&p.cwd, &p.db_path).map_err(core_err)?
            }
            Err(e) => return Err(core_err(e)),
        };
        let _ = self.daemon.registry.add(&proj.root, &proj.db_path);
        let (schema_version, bootstrapped) =
            verbs::project_init(&proj, &self.daemon.packages, p.skip_bootstrap)
                .map_err(core_err)?;
        self.daemon.hub.project_changed();
        Ok(serde_json::json!({
            "root": proj.root,
            "db_path": proj.db_path,
            "schema_version": schema_version,
            "bootstrapped": bootstrapped,
        }))
    }
}

/// The per-subscription replay-then-tail loop. Emits `job-event` notifications:
/// message (with `event_id`), status, done, error — mirroring the Go SSE frames.
fn stream_job(
    daemon: &Arc<Daemon>,
    proj: &Arc<Project>,
    job_id: &str,
    p: &SubscribeParams,
    writer: &SharedWriter,
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
    writer: &SharedWriter,
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
    writer: &SharedWriter,
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

fn emit_message(job_id: &str, event_id: i64, ev: &wire::MessageEvent, writer: &SharedWriter) {
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

fn emit_status(daemon: &Arc<Daemon>, proj: &Arc<Project>, job_id: &str, writer: &SharedWriter) {
    emit_job_frame(daemon, proj, job_id, "status", writer);
}
fn emit_done(daemon: &Arc<Daemon>, proj: &Arc<Project>, job_id: &str, writer: &SharedWriter) {
    emit_job_frame(daemon, proj, job_id, "done", writer);
}

fn emit_job_frame(
    daemon: &Arc<Daemon>,
    proj: &Arc<Project>,
    job_id: &str,
    kind: &str,
    writer: &SharedWriter,
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

fn write_line(writer: &SharedWriter, resp: &Response) {
    let Ok(mut buf) = serde_json::to_vec(resp) else {
        return;
    };
    buf.push(b'\n');
    let mut w = writer.lock().unwrap();
    let _ = w.write_all(&buf).and_then(|()| w.flush());
}

fn write_note(writer: &SharedWriter, note: &Notification) {
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

/// True for the task-mutating write methods that should push a `task-changed`
/// notification on success.
fn is_task_write(method: &str) -> bool {
    matches!(
        method,
        "task.create"
            | "task.update"
            | "task.done"
            | "task.cancel"
            | "task.reopen"
            | "task.setStatus"
            | "task.setTitleDescription"
            | "task.setPriority"
            | "task.enroll"
            | "task.resume"
            | "task.block"
            | "task.unblock"
            | "task.unblockAll"
            | "task.metadata.set"
            | "task.metadata.unset"
            | "task.metadata.resetVisits"
            | "comment.add"
            | "workflow.create"
            | "workflow.delete"
            | "workflow.updateIsolation"
            | "agent.install"
            | "agent.uninstall"
            | "sql.exec"
            | "step.next"
    )
}

fn core_err(e: CoreError) -> ErrorObject {
    let (code, message) = match &e {
        CoreError::ProjectNotFound(s) => {
            (codes::PROJECT_NOT_FOUND, format!("project not found: {s}"))
        }
        CoreError::InvalidProject(s) => (codes::INVALID_PROJECT, format!("invalid project: {s}")),
        CoreError::NotFound => (codes::NOT_FOUND, "not found".to_string()),
        CoreError::Invalid(m) => (codes::INVALID_PARAMS, m.clone()),
        CoreError::Conflict(m) => (codes::CONFLICT, m.clone()),
        CoreError::MaxVisitsExceeded { .. } => (codes::CONFLICT, e.to_string()),
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
struct StepNextParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    to: String,
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

// ---- write param structs --------------------------------------------------

#[derive(Debug, Default, Deserialize)]
struct TaskCreateParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    title: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    priority: Option<i64>,
    #[serde(default)]
    blocks: Vec<String>,
    #[serde(default)]
    blocked_by: Vec<String>,
    #[serde(default)]
    workflow: String,
    #[serde(default)]
    agent: String,
    #[serde(default)]
    step: String,
    #[serde(default)]
    base_ref: String,
    #[serde(default)]
    caller: String,
}

#[derive(Debug, Default, Deserialize)]
struct TaskUpdateParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    title: Option<String>,
    #[serde(default)]
    description: Option<String>,
    #[serde(default)]
    priority: Option<i64>,
    #[serde(default)]
    status: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
struct SetStatusParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    status: String,
}

#[derive(Debug, Default, Deserialize)]
struct TitleDescParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    title: String,
    #[serde(default)]
    description: String,
}

#[derive(Debug, Default, Deserialize)]
struct PriorityParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    priority: i64,
}

#[derive(Debug, Default, Deserialize)]
struct EnrollParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    workflow: String,
    #[serde(default)]
    agent: String,
    #[serde(default)]
    step: String,
    #[serde(default)]
    base_ref: String,
}

#[derive(Debug, Default, Deserialize)]
struct ResumeParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    to_step: String,
}

#[derive(Debug, Default, Deserialize)]
struct BlockParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    blockers: Vec<String>,
}

#[derive(Debug, Default, Deserialize)]
struct MetadataSetParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    key: String,
    #[serde(default)]
    value: Value,
    #[serde(default)]
    replace_all: bool,
}

#[derive(Debug, Default, Deserialize)]
struct MetadataKeyParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    key: String,
}

#[derive(Debug, Default, Deserialize)]
struct ResetVisitsParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    step: String,
    #[serde(default)]
    step_id: String,
}

#[derive(Debug, Default, Deserialize)]
struct CommentAddParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    task_id: String,
    #[serde(default)]
    author: String,
    #[serde(default)]
    text: String,
}

#[derive(Debug, Default, Deserialize)]
struct WorkflowCreateParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    file: String,
    #[serde(default)]
    json: String,
    #[serde(default)]
    no_install: bool,
}

#[derive(Debug, Default, Deserialize)]
struct WorkflowDeleteParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    name: String,
}

#[derive(Debug, Default, Deserialize)]
struct WorkflowIsolationParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    source: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    mode: String,
    #[serde(default)]
    force: bool,
    #[serde(default)]
    dry_run: bool,
}

#[derive(Debug, Default, Deserialize)]
struct AgentInstallParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    version: String,
}

#[derive(Debug, Default, Deserialize)]
struct AgentUninstallParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    force: bool,
}

#[derive(Debug, Default, Deserialize)]
struct SqlParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    query: String,
}

#[derive(Debug, Default, Deserialize)]
struct ProjectInitParams {
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    db_path: String,
    #[serde(default)]
    skip_bootstrap: bool,
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
