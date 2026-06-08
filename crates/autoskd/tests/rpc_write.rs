//! End-to-end RPC tests for the Phase 3 write surface + TCP auth + shutdown.
//! Drives the real [`Server`] over a UDS / TCP socket with a line-delimited
//! JSON-RPC client.

use std::io::{BufRead, BufReader, Write};
use std::net::TcpStream;
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use autosk_core::projectmgr::Manager;
use autosk_core::registry::Registry;
use autosk_core::store::Db;
use autoskd::daemon::{Daemon, DaemonConfig};
use autoskd::server::Server;
use serde_json::{json, Value};
use tempfile::TempDir;

struct Harness {
    _dir: TempDir,
    cwd: String,
    sock: std::path::PathBuf,
    id: AtomicU64,
}

fn spawn_daemon() -> (Harness, String, Arc<Daemon>) {
    let dir = tempfile::tempdir().unwrap();
    let cwd = dir.path().join("proj");
    std::fs::create_dir_all(cwd.join(".autosk")).unwrap();
    let db_path = cwd.join(".autosk").join("db");
    let db = Db::open_or_create(&db_path).unwrap();
    db.migrate().unwrap();
    drop(db); // the Manager re-opens it.

    let reg = Arc::new(Registry::open_at(dir.path().join("projects.json")));
    let mgr = Arc::new(Manager::new());
    // A short poll interval makes the change-poller's `task-changed` push prompt.
    let cfg = DaemonConfig {
        poll_interval: std::time::Duration::from_millis(100),
        gc_interval: None,
        ..DaemonConfig::default()
    };
    let daemon = Daemon::new(mgr, reg, cfg);

    let sock = dir.path().join("d.sock");
    let listener = autoskd::uds::listen(&sock).unwrap();
    let token = "secret-token".to_string();
    let server = Arc::new(Server::new(Arc::clone(&daemon)).with_token(Some(token.clone())));

    // UDS serve loop.
    let srv = Arc::clone(&server);
    std::thread::spawn(move || srv.serve(listener));

    // TCP serve loop on an ephemeral port.
    let tcp = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = tcp.local_addr().unwrap().to_string();
    let srv2 = Arc::clone(&server);
    std::thread::spawn(move || srv2.serve_tcp(tcp));

    let h = Harness {
        cwd: cwd.to_string_lossy().to_string(),
        _dir: dir,
        sock,
        id: AtomicU64::new(1),
    };
    (h, addr, daemon)
}

impl Harness {
    /// One request/response over a fresh UDS connection (matches the Go client's
    /// connection-per-call model).
    fn call(&self, method: &str, mut params: Value) -> Value {
        if let Value::Object(m) = &mut params {
            m.insert("cwd".into(), json!(self.cwd));
        }
        let conn = UnixStream::connect(&self.sock).unwrap();
        rpc_roundtrip(conn, self.id.fetch_add(1, Ordering::SeqCst), method, params)
    }
}

fn rpc_roundtrip<S: std::io::Read + Write>(stream: S, id: u64, method: &str, params: Value) -> Value
where
    S: Sized,
{
    let mut w = stream;
    let req = json!({"id": id, "method": method, "params": params});
    let mut line = serde_json::to_vec(&req).unwrap();
    line.push(b'\n');
    w.write_all(&line).unwrap();
    w.flush().unwrap();
    let mut reader = BufReader::new(w);
    let mut resp = String::new();
    reader.read_line(&mut resp).unwrap();
    serde_json::from_str(&resp).unwrap()
}

#[test]
fn write_create_and_get_over_uds() {
    let (h, _addr, _daemon) = spawn_daemon();
    // create
    let resp = h.call(
        "task.create",
        json!({"source": "cli", "title": "via rpc", "caller": "human", "priority": 1}),
    );
    assert!(resp.get("error").is_none(), "create error: {resp}");
    let id = resp["result"]["id"].as_str().unwrap().to_string();
    assert_eq!(resp["result"]["status"], "new");

    // get it back
    let got = h.call("task.get", json!({"id": id}));
    assert_eq!(got["result"]["title"], "via rpc");

    // comment.add
    let c = h.call(
        "comment.add",
        json!({"task_id": id, "author": "rev", "text": "looks good"}),
    );
    assert_eq!(c["result"]["text"], "looks good");
    assert_eq!(c["result"]["author_name"], "rev");

    // metadata.set returns {task, changed}
    let m = h.call(
        "task.metadata.set",
        json!({"id": id, "key": "tags.k", "value": 5}),
    );
    assert_eq!(m["result"]["changed"], true);

    // block self → error surfaced
    let b = h.call("task.block", json!({"id": id, "blockers": [id]}));
    assert!(b["error"]["message"]
        .as_str()
        .unwrap()
        .contains("cannot block itself"));
}

#[test]
fn task_subscribe_receives_change_notification() {
    let (h, _addr, _daemon) = spawn_daemon();
    // Persistent connection that subscribes to task-changed.
    let mut sub = UnixStream::connect(&h.sock).unwrap();
    {
        let req = json!({"id": 1, "method": "task.subscribe", "params": {"cwd": h.cwd}});
        let mut line = serde_json::to_vec(&req).unwrap();
        line.push(b'\n');
        sub.write_all(&line).unwrap();
        sub.flush().unwrap();
    }
    let mut reader = BufReader::new(sub.try_clone().unwrap());
    let mut ack = String::new();
    reader.read_line(&mut ack).unwrap();
    assert!(ack.contains("subscribed"), "subscribe ack: {ack}");

    // A write on a separate connection (resolves the project → starts the
    // change poller → broadcasts task-changed within the poll interval).
    let r = h.call(
        "task.create",
        json!({"source": "cli", "title": "x", "caller": "human"}),
    );
    assert!(r.get("error").is_none());

    // The subscriber receives a task-changed notification.
    sub.set_read_timeout(Some(std::time::Duration::from_secs(3)))
        .unwrap();
    let mut note = String::new();
    reader.read_line(&mut note).unwrap();
    let v: Value = serde_json::from_str(&note).unwrap();
    assert_eq!(v["method"], "task-changed", "got: {note}");
    assert!(v["params"]["root"].as_str().unwrap().contains("proj"));
}

const WF_JSON: &str = r#"{
  "name": "wf1",
  "first_step": "do",
  "isolation": "none",
  "steps": {
    "do": {
      "agent": { "name": "human" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "done" } ]
    }
  }
}"#;

/// Drives `task.resume` + `workflow.updateIsolation` with `source=lazy` over RPC
/// and asserts the daemon writes the lazy commit dialect (review comment 654:
/// these are the two verbs whose lazy dialect regressed).
#[test]
fn lazy_resume_and_isolation_dialect_over_rpc() {
    let (h, _addr, daemon) = spawn_daemon();

    // Read the latest dolt_log message via the daemon's WRITER connection:
    // pooled reader connections (what sql.query uses) cache the commit graph
    // and won't observe a fresh dolt_commit, so SELECT message FROM dolt_log
    // over the read path is stale here.
    let proj = daemon.resolve(&h.cwd, "").unwrap();
    let last_commit = || -> String {
        proj.db
            .with_write(|conn| {
                Ok(
                    conn.query_row("SELECT message FROM dolt_log LIMIT 1", [], |r| {
                        r.get::<_, String>(0)
                    })?,
                )
            })
            .unwrap()
    };

    // workflow.create wf1 (isolation none).
    let w = h.call(
        "workflow.create",
        json!({"source": "cli", "json": WF_JSON, "no_install": true}),
    );
    assert!(w.get("error").is_none(), "wf create: {w}");

    // workflow.updateIsolation none→worktree, source=lazy (no tasks → clean flip).
    let iso = h.call(
        "workflow.updateIsolation",
        json!({"source": "lazy", "name": "wf1", "mode": "worktree"}),
    );
    assert!(iso.get("error").is_none(), "iso: {iso}");
    assert_eq!(
        last_commit(),
        "lazy: workflow update wf1 isolation=none\u{2192}worktree"
    );

    // A second workflow (isolation none) for the resume path — lazy enroll into
    // an isolation=none workflow allocates no worktree (no git needed).
    let wf2 = WF_JSON.replace("wf1", "wf2");
    let w2 = h.call(
        "workflow.create",
        json!({"source": "cli", "json": wf2, "no_install": true}),
    );
    assert!(w2.get("error").is_none(), "wf2 create: {w2}");

    let t = h.call(
        "task.create",
        json!({"source": "cli", "title": "t", "caller": "human"}),
    );
    let id = t["result"]["id"].as_str().unwrap().to_string();
    let en = h.call(
        "task.enroll",
        json!({"source": "lazy", "id": id, "workflow": "wf2"}),
    );
    assert!(en.get("error").is_none(), "enroll: {en}");

    // Park to 'human': sql.exec stages the status flip (no commit); the
    // following comment.add commits the staged change (dolt_commit '-A'), so
    // resume back to 'work' produces a real diff from HEAD.
    let _ = h.call(
        "sql.exec",
        json!({"query": format!("UPDATE tasks SET status='human' WHERE id='{id}'")}),
    );
    let c = h.call(
        "comment.add",
        json!({"source": "cli", "task_id": id, "author": "human", "text": "park"}),
    );
    assert!(c.get("error").is_none(), "park commit: {c}");

    // resume source=lazy → lazy dialect commit.
    let r = h.call("task.resume", json!({"source": "lazy", "id": id}));
    assert!(r.get("error").is_none(), "resume: {r}");
    assert_eq!(r["result"]["status"], "work");
    assert_eq!(last_commit(), format!("lazy: resume {id}"));
}

#[test]
fn tcp_requires_auth() {
    let (_h, addr, _daemon) = spawn_daemon();

    // 1. A request before auth is rejected.
    let s = TcpStream::connect(&addr).unwrap();
    let r = rpc_roundtrip(s, 1, "version", json!({}));
    assert!(r["error"]["message"]
        .as_str()
        .unwrap()
        .contains("auth required"));

    // 2. Wrong token rejected.
    let s = TcpStream::connect(&addr).unwrap();
    let r = rpc_roundtrip(s, 1, "auth", json!({"token": "nope"}));
    assert!(r["error"]["message"]
        .as_str()
        .unwrap()
        .contains("invalid or missing token"));

    // 3. Correct token → ok, then a real request works on the same connection.
    let mut s = TcpStream::connect(&addr).unwrap();
    {
        let req = json!({"id": 1, "method": "auth", "params": {"token": "secret-token"}});
        let mut line = serde_json::to_vec(&req).unwrap();
        line.push(b'\n');
        s.write_all(&line).unwrap();
        s.flush().unwrap();
    }
    let mut reader = BufReader::new(s.try_clone().unwrap());
    let mut resp = String::new();
    reader.read_line(&mut resp).unwrap();
    let auth: Value = serde_json::from_str(&resp).unwrap();
    assert_eq!(auth["result"]["ok"], true, "auth resp: {resp}");
    // version now works.
    {
        let req = json!({"id": 2, "method": "version", "params": {}});
        let mut line = serde_json::to_vec(&req).unwrap();
        line.push(b'\n');
        s.write_all(&line).unwrap();
        s.flush().unwrap();
    }
    let mut resp2 = String::new();
    reader.read_line(&mut resp2).unwrap();
    let v: Value = serde_json::from_str(&resp2).unwrap();
    assert!(v["result"]["version"].is_string(), "version resp: {resp2}");
}

/// `maint.compact` + `step.next` over the wire (the two write RPCs added so the
/// remaining Go CLI verbs — gc + step — can flip to pure RPC clients).
#[test]
fn compact_and_step_next_over_uds() {
    let (h, _addr, _daemon) = spawn_daemon();

    // maint.compact succeeds and returns parseable stats with a verbatim
    // dolt_gc() reply.
    let g = h.call("maint.compact", json!({}));
    assert!(g.get("error").is_none(), "compact error: {g}");
    assert!(g["result"]["chunks_removed"].is_i64(), "stats: {g}");
    assert!(
        !g["result"]["raw"].as_str().unwrap().is_empty(),
        "dolt_gc reply is non-empty: {g}"
    );

    // step.next with no active run surfaces the byte-identical CLI-final error
    // (task id + daemon hint) — the parity-sensitive mapping.
    let c = h.call(
        "task.create",
        json!({"source": "cli", "title": "t", "caller": "human"}),
    );
    let id = c["result"]["id"].as_str().unwrap().to_string();
    let s = h.call("step.next", json!({"id": id, "to": "done"}));
    assert_eq!(
        s["error"]["message"].as_str().unwrap(),
        format!("no active run for task {id} (is the daemon running it?)")
    );
}
