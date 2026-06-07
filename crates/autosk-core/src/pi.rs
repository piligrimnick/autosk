//! `pi --mode rpc` wire — the Rust port of `internal/daemon/pi`.
//!
//! Protocol: JSON-Lines on the child's stdin/stdout. A background reader
//! thread frames stdout by `\n` (no per-line cap, line-oriented resync on a
//! bad line — mirroring the Go reader's rationale), delivers `response` lines
//! to the matching command's channel, auto-cancels blocking
//! `extension_ui_request`s so headless runs never hang, tracks the
//! agent_start/agent_end streaming flag, and signals every `agent_end` to
//! [`Runner::wait_for_agent_end`].

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::process::{ChildStdin, Command as PCommand, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, Sender, SyncSender, TrySendError};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::ctx::{Ctx, Done};
use crate::runner::{PiRunner, RunnerError};

/// Poll granularity for context-aware waits.
const POLL: Duration = Duration::from_millis(10);

/// One outgoing JSON-line command on pi's stdin (mirror of `pi.Command`).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Command {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub id: String,
    #[serde(rename = "type")]
    pub typ: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(
        rename = "streamingBehavior",
        default,
        skip_serializing_if = "String::is_empty"
    )]
    pub streaming_behavior: String,
}

/// Parsed `{type:"response", ...}` line (mirror of `pi.Response`).
#[derive(Debug, Clone, Default)]
pub struct Response {
    pub id: String,
    pub command: String,
    pub success: bool,
    pub error: String,
    pub data: Option<Value>,
}

/// The slice of `get_state` the daemon cares about (mirror of `pi.SessionInfo`).
#[derive(Debug, Clone, Default, Deserialize)]
pub struct SessionInfo {
    #[serde(rename = "sessionId", default)]
    pub session_id: String,
    #[serde(rename = "sessionFile", default)]
    pub session_file: String,
    #[serde(rename = "messageCount", default)]
    pub message_count: i64,
}

/// Stable projection of pi's event stream (mirror of `pi.EventKind`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EventKind {
    AgentStart,
    AgentEnd,
    Response,
    ExtensionRequest,
    Other,
}

/// A normalised event off the reader (mirror of `pi.Event`). `raw` is the
/// original wire object, emitted verbatim by transcript/streaming consumers.
#[derive(Debug, Clone)]
pub struct Event {
    pub kind: EventKind,
    pub raw: Value,
}

/// Options for [`spawn`] (mirror of `pi.Opts`).
#[derive(Debug, Clone, Default)]
pub struct PiOpts {
    pub pi_bin: String,
    pub cwd: String,
    pub model: String,
    pub thinking: String,
    pub session_dir: String,
    pub extra_args: Vec<String>,
    /// When non-empty, replaces the inherited environment.
    pub env: Vec<(String, String)>,
}

#[derive(Deserialize)]
struct Inbound {
    #[serde(rename = "type", default)]
    typ: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    command: String,
    #[serde(default)]
    method: String,
    #[serde(default)]
    success: Option<bool>,
    #[serde(default)]
    error: String,
    #[serde(default)]
    data: Option<Value>,
}

fn classify(typ: &str) -> EventKind {
    match typ {
        "agent_start" => EventKind::AgentStart,
        "agent_end" => EventKind::AgentEnd,
        "response" => EventKind::Response,
        "extension_ui_request" => EventKind::ExtensionRequest,
        _ => EventKind::Other,
    }
}

/// Shared state between the reader thread and the [`Runner`] handle.
struct Shared {
    stdin: Mutex<Option<ChildStdin>>,
    pending: Mutex<HashMap<String, Sender<Response>>>,
    next_id: AtomicU64,
    streaming: AtomicBool,
    closed: AtomicBool,
    read_err: Mutex<Option<String>>,
    turn_ends: SyncSender<()>,
    events: SyncSender<Event>,
    /// Reaper-populated exit state: `(exit_code, process_done)`.
    exit: Mutex<Option<i32>>,
    exit_cv: Condvar,
}

/// One pi child process.
pub struct Runner {
    pid: i32,
    shared: Arc<Shared>,
    turn_ends_rx: Mutex<Receiver<()>>,
    events_rx: Mutex<Option<Receiver<Event>>>,
}

/// Spawns a new pi child in `--mode rpc`.
pub fn spawn(_ctx: &Ctx, opts: PiOpts) -> Result<Runner, RunnerError> {
    let bin = if opts.pi_bin.is_empty() {
        "pi".to_string()
    } else {
        opts.pi_bin.clone()
    };
    let mut args: Vec<String> = vec!["--mode".into(), "rpc".into()];
    if !opts.model.is_empty() {
        args.push("--model".into());
        args.push(opts.model.clone());
    }
    if !opts.thinking.is_empty() {
        args.push("--thinking".into());
        args.push(opts.thinking.clone());
    }
    if !opts.session_dir.is_empty() {
        args.push("--session-dir".into());
        args.push(opts.session_dir.clone());
    }
    args.extend(opts.extra_args.iter().cloned());

    let mut cmd = PCommand::new(&bin);
    cmd.args(&args)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if !opts.cwd.is_empty() {
        cmd.current_dir(&opts.cwd);
    }
    if !opts.env.is_empty() {
        cmd.env_clear();
        for (k, v) in &opts.env {
            cmd.env(k, v);
        }
    }
    let mut child = cmd
        .spawn()
        .map_err(|e| RunnerError::Io(format!("start {bin}: {e}")))?;
    let pid = child.id() as i32;
    let stdin = child.stdin.take();
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");

    let (turn_tx, turn_rx) = sync_channel::<()>(32);
    let (ev_tx, ev_rx) = sync_channel::<Event>(256);
    let shared = Arc::new(Shared {
        stdin: Mutex::new(stdin),
        pending: Mutex::new(HashMap::new()),
        next_id: AtomicU64::new(0),
        streaming: AtomicBool::new(false),
        closed: AtomicBool::new(false),
        read_err: Mutex::new(None),
        turn_ends: turn_tx,
        events: ev_tx,
        exit: Mutex::new(None),
        exit_cv: Condvar::new(),
    });

    // Drain stderr so the pipe never fills + blocks the child.
    std::thread::spawn(move || {
        let mut r = BufReader::new(stderr);
        let mut buf = Vec::new();
        loop {
            buf.clear();
            if r.read_until(b'\n', &mut buf).unwrap_or(0) == 0 {
                break;
            }
        }
    });

    // Reader loop.
    let reader_shared = Arc::clone(&shared);
    std::thread::spawn(move || read_loop(reader_shared, stdout));

    // Reaper: wait the child, store the exit code, wake waiters.
    let reaper_shared = Arc::clone(&shared);
    std::thread::spawn(move || {
        let code = child.wait().ok().and_then(|s| s.code()).unwrap_or(-1);
        let mut g = reaper_shared.exit.lock().unwrap();
        *g = Some(code);
        reaper_shared.exit_cv.notify_all();
    });

    Ok(Runner {
        pid,
        shared,
        turn_ends_rx: Mutex::new(turn_rx),
        events_rx: Mutex::new(Some(ev_rx)),
    })
}

impl Runner {
    /// Takes the events receiver (once). Used by the pi-wire tests; the
    /// executor relies on `wait_for_agent_end` + the session file instead.
    pub fn take_events(&self) -> Option<Receiver<Event>> {
        self.events_rx.lock().unwrap().take()
    }

    fn send_command_inner(&self, mut c: Command) -> Result<Receiver<Response>, RunnerError> {
        if c.id.is_empty() {
            c.id = format!(
                "d{}",
                self.shared.next_id.fetch_add(1, Ordering::SeqCst) + 1
            );
        }
        let (tx, rx) = std::sync::mpsc::channel();
        {
            if self.shared.closed.load(Ordering::SeqCst) {
                return Err(RunnerError::Closed);
            }
            self.shared.pending.lock().unwrap().insert(c.id.clone(), tx);
        }
        let mut buf = serde_json::to_vec(&c).map_err(|e| RunnerError::Io(e.to_string()))?;
        buf.push(b'\n');
        {
            let mut g = self.shared.stdin.lock().unwrap();
            match g.as_mut() {
                Some(w) => {
                    if let Err(e) = w.write_all(&buf).and_then(|()| w.flush()) {
                        drop(g);
                        self.shared.pending.lock().unwrap().remove(&c.id);
                        return Err(RunnerError::Io(format!("write stdin: {e}")));
                    }
                }
                None => {
                    drop(g);
                    self.shared.pending.lock().unwrap().remove(&c.id);
                    return Err(RunnerError::Closed);
                }
            }
        }
        Ok(rx)
    }
}

fn await_response(ctx: &Ctx, rx: &Receiver<Response>) -> Result<Response, RunnerError> {
    loop {
        match rx.recv_timeout(POLL) {
            Ok(r) => return Ok(r),
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => match ctx.done() {
                Some(Done::Cancelled) => return Err(RunnerError::Cancelled),
                Some(Done::DeadlineExceeded) => return Err(RunnerError::DeadlineExceeded),
                None => continue,
            },
            Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                return Err(RunnerError::Closed)
            }
        }
    }
}

impl PiRunner for Runner {
    fn pid(&self) -> i32 {
        self.pid
    }

    fn get_state(&self, ctx: &Ctx) -> Result<SessionInfo, RunnerError> {
        let rx = self.send_command_inner(Command {
            typ: "get_state".into(),
            ..Default::default()
        })?;
        let resp = await_response(ctx, &rx)?;
        if !resp.success {
            return Err(RunnerError::Rejected(format!("get_state: {}", resp.error)));
        }
        let data = resp.data.unwrap_or(Value::Null);
        serde_json::from_value(data)
            .map_err(|e| RunnerError::Io(format!("decode session info: {e}")))
    }

    fn send_prompt(&self, ctx: &Ctx, message: &str) -> Result<(), RunnerError> {
        let rx = self.send_command_inner(Command {
            typ: "prompt".into(),
            message: message.to_string(),
            ..Default::default()
        })?;
        let resp = await_response(ctx, &rx)?;
        if !resp.success {
            return Err(RunnerError::Rejected(format!(
                "prompt rejected: {}",
                resp.error
            )));
        }
        Ok(())
    }

    fn wait_for_agent_end(&self, ctx: &Ctx) -> Result<(), RunnerError> {
        let rx = self.turn_ends_rx.lock().unwrap();
        loop {
            match rx.recv_timeout(POLL) {
                // A token always wins over a concurrent reader-exit: the reader
                // emits agent_end (→ token) before setting `closed`, so a
                // successful turn is honoured even if the pipe dies right after.
                Ok(()) => return Ok(()),
                Err(std::sync::mpsc::RecvTimeoutError::Timeout)
                | Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                    match ctx.done() {
                        Some(Done::Cancelled) => return Err(RunnerError::Cancelled),
                        Some(Done::DeadlineExceeded) => return Err(RunnerError::DeadlineExceeded),
                        None => {}
                    }
                    // The turn_ends sender lives in `shared` (held by this
                    // Runner), so it never disconnects on reader exit; the
                    // `closed` flag is the authoritative reader-done signal.
                    if self.shared.closed.load(Ordering::SeqCst) {
                        if let Some(e) = self.shared.read_err.lock().unwrap().clone() {
                            return Err(RunnerError::Io(e));
                        }
                        return Err(RunnerError::Eof);
                    }
                }
            }
        }
    }

    fn abort(&self, ctx: &Ctx) -> Result<(), RunnerError> {
        let rx = self.send_command_inner(Command {
            typ: "abort".into(),
            ..Default::default()
        })?;
        let resp = await_response(ctx, &rx)?;
        if !resp.success {
            return Err(RunnerError::Rejected(format!(
                "abort failed: {}",
                resp.error
            )));
        }
        Ok(())
    }

    fn close_stdin(&self) -> Result<(), RunnerError> {
        // Dropping the ChildStdin closes the pipe.
        *self.shared.stdin.lock().unwrap() = None;
        Ok(())
    }

    fn terminate(&self) -> Result<(), RunnerError> {
        if self.pid > 0 {
            unsafe {
                libc::kill(self.pid, libc::SIGTERM);
            }
        }
        Ok(())
    }

    fn kill(&self) -> Result<(), RunnerError> {
        if self.pid > 0 {
            unsafe {
                libc::kill(self.pid, libc::SIGKILL);
            }
        }
        Ok(())
    }

    fn wait(&self, ctx: &Ctx, grace: Duration) -> (i32, Result<(), RunnerError>) {
        let deadline = Instant::now() + grace;
        let mut g = self.shared.exit.lock().unwrap();
        loop {
            if let Some(code) = *g {
                return (code, Ok(()));
            }
            if let Some(d) = ctx.done() {
                let e = match d {
                    Done::Cancelled => RunnerError::Cancelled,
                    Done::DeadlineExceeded => RunnerError::DeadlineExceeded,
                };
                return (-1, Err(e));
            }
            let now = Instant::now();
            if now >= deadline {
                return (-1, Err(RunnerError::WaitTimeout));
            }
            let wait = POLL.min(deadline - now);
            let (ng, _) = self.shared.exit_cv.wait_timeout(g, wait).unwrap();
            g = ng;
        }
    }

    fn send_command(&self, c: Command) -> Result<Receiver<Response>, RunnerError> {
        self.send_command_inner(c)
    }

    fn is_streaming(&self) -> bool {
        self.shared.streaming.load(Ordering::SeqCst)
    }
}

fn read_loop(shared: Arc<Shared>, stdout: std::process::ChildStdout) {
    let mut br = BufReader::new(stdout);
    let mut read_err: Option<String> = None;
    let mut line: Vec<u8> = Vec::new();
    loop {
        line.clear();
        match br.read_until(b'\n', &mut line) {
            Ok(0) => break, // EOF
            Ok(_) => {
                let trimmed = trim_ascii(&line);
                if !trimmed.is_empty() {
                    handle_line(&shared, trimmed);
                }
            }
            Err(e) => {
                read_err = Some(e.to_string());
                break;
            }
        }
    }
    // Reader teardown: record error, mark closed, fail pending responses.
    *shared.read_err.lock().unwrap() = read_err;
    shared.closed.store(true, Ordering::SeqCst);
    shared.pending.lock().unwrap().clear(); // drops senders → recv disconnects
                                            // turn_ends / events senders drop when `shared` is dropped (or here, the
                                            // last clone) — Disconnected wakes wait_for_agent_end.
}

fn handle_line(shared: &Arc<Shared>, raw: &[u8]) {
    let parsed: Result<Inbound, _> = serde_json::from_slice(raw);
    let value: Value = serde_json::from_slice(raw).unwrap_or(Value::Null);
    let Ok(msg) = parsed else {
        // Malformed / unrecognised JSON: surface as KindOther, keep reading.
        emit(
            shared,
            Event {
                kind: EventKind::Other,
                raw: value,
            },
        );
        return;
    };
    let kind = classify(&msg.typ);
    match kind {
        EventKind::Response => {
            let resp = Response {
                id: msg.id.clone(),
                command: msg.command.clone(),
                success: msg.success.unwrap_or(false),
                error: msg.error.clone(),
                data: msg.data.clone(),
            };
            deliver_response(shared, resp);
            emit(shared, Event { kind, raw: value });
        }
        EventKind::ExtensionRequest => {
            reply_to_extension_ui(shared, &msg);
            emit(shared, Event { kind, raw: value });
        }
        EventKind::AgentStart => {
            shared.streaming.store(true, Ordering::SeqCst);
            emit(shared, Event { kind, raw: value });
        }
        EventKind::AgentEnd => {
            shared.streaming.store(false, Ordering::SeqCst);
            emit(shared, Event { kind, raw: value });
            let _ = shared.turn_ends.try_send(());
        }
        EventKind::Other => emit(shared, Event { kind, raw: value }),
    }
}

fn emit(shared: &Arc<Shared>, e: Event) {
    // Non-blocking: a slow/absent consumer must never stall the reader.
    match shared.events.try_send(e) {
        Ok(()) | Err(TrySendError::Full(_)) | Err(TrySendError::Disconnected(_)) => {}
    }
}

fn deliver_response(shared: &Arc<Shared>, resp: Response) {
    let tx = shared.pending.lock().unwrap().remove(&resp.id);
    if let Some(tx) = tx {
        let _ = tx.send(resp);
    }
}

fn reply_to_extension_ui(shared: &Arc<Shared>, msg: &Inbound) {
    if msg.id.is_empty() {
        return;
    }
    // Fire-and-forget methods need no reply.
    match msg.method.as_str() {
        "notify" | "setStatus" | "setWidget" | "setTitle" | "set_editor_text" => return,
        _ => {}
    }
    let reply = serde_json::json!({
        "type": "extension_ui_response",
        "id": msg.id,
        "cancelled": true,
    });
    let Ok(mut buf) = serde_json::to_vec(&reply) else {
        return;
    };
    buf.push(b'\n');
    if let Some(w) = shared.stdin.lock().unwrap().as_mut() {
        let _ = w.write_all(&buf).and_then(|()| w.flush());
    }
}

fn trim_ascii(b: &[u8]) -> &[u8] {
    let mut start = 0;
    let mut end = b.len();
    while start < end && b[start].is_ascii_whitespace() {
        start += 1;
    }
    while end > start && b[end - 1].is_ascii_whitespace() {
        end -= 1;
    }
    &b[start..end]
}
