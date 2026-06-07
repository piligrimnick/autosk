//! Live job-surface integration test (acceptance #3): `job.subscribe` streams a
//! running job's transcript with correct replay-then-tail + a terminal `done`
//! frame; `attach:true` bumps the attach counter; `job.input` dispatches
//! prompt/steer/follow_up per the runner's streaming state with the
//! state-mismatch retry; `job.abort` reaches the live runner.

use std::collections::VecDeque;
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::Receiver;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use autosk_core::ctx::Ctx;
use autosk_core::pi::{Command, Response as PiResponse, SessionInfo};
use autosk_core::projectmgr::Manager;
use autosk_core::registry::Registry;
use autosk_core::runner::{PiRunner, RunnerError};
use autosk_core::runstore::NewRun;
use autosk_core::{migrate, Db};
use autoskd::daemon::{Daemon, DaemonConfig};
use autoskd::server::Server;
use autoskd::uds;
use serde_json::{json, Value};

const FIXTURE_SQL: &str = "\
INSERT INTO agents(id,name,is_human,created_at) VALUES ('ag-h','human',1,1),('ag-g','@autogent/generic',0,1);\
INSERT INTO workflows(id,name,description,first_step_id,is_synthetic,isolation,created_at) VALUES ('wf','feature-dev','','st',0,'none',1);\
INSERT INTO steps(id,workflow_id,name,agent_id,seq,agent_params,max_visits) VALUES ('st','wf','dev','ag-g',0,NULL,0);\
INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) VALUES ('st',NULL,'done','.');\
INSERT INTO tasks(id,title,description,status,priority,author_id,workflow_id,current_step_id,metadata,created_at,updated_at) VALUES ('ask-000001','T','','human',2,'ag-h','wf','st',NULL,1,1);\
";

/// A controllable runner registered in the daemon's live registry.
struct FakeRunner {
    streaming: AtomicBool,
    aborted: AtomicBool,
    sent: Mutex<Vec<String>>,
    scripted: Mutex<VecDeque<(bool, String)>>,
}
impl FakeRunner {
    fn new() -> Arc<FakeRunner> {
        Arc::new(FakeRunner {
            streaming: AtomicBool::new(false),
            aborted: AtomicBool::new(false),
            sent: Mutex::new(Vec::new()),
            scripted: Mutex::new(VecDeque::new()),
        })
    }
}
impl PiRunner for FakeRunner {
    fn pid(&self) -> i32 {
        9
    }
    fn get_state(&self, _c: &Ctx) -> Result<SessionInfo, RunnerError> {
        Ok(SessionInfo::default())
    }
    fn send_prompt(&self, _c: &Ctx, _m: &str) -> Result<(), RunnerError> {
        Ok(())
    }
    fn wait_for_agent_end(&self, _c: &Ctx) -> Result<(), RunnerError> {
        Ok(())
    }
    fn abort(&self, _c: &Ctx) -> Result<(), RunnerError> {
        self.aborted.store(true, Ordering::SeqCst);
        Ok(())
    }
    fn close_stdin(&self) -> Result<(), RunnerError> {
        Ok(())
    }
    fn terminate(&self) -> Result<(), RunnerError> {
        Ok(())
    }
    fn kill(&self) -> Result<(), RunnerError> {
        Ok(())
    }
    fn wait(&self, _c: &Ctx, _g: Duration) -> (i32, Result<(), RunnerError>) {
        (0, Ok(()))
    }
    fn send_command(&self, c: Command) -> Result<Receiver<PiResponse>, RunnerError> {
        self.sent.lock().unwrap().push(c.typ.clone());
        let (ok, err) = self
            .scripted
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or((true, String::new()));
        let (tx, rx) = std::sync::mpsc::channel();
        let _ = tx.send(PiResponse {
            id: c.id,
            command: c.typ,
            success: ok,
            error: err,
            data: None,
        });
        Ok(rx)
    }
    fn is_streaming(&self) -> bool {
        self.streaming.load(Ordering::SeqCst)
    }
}

struct Env {
    daemon: Arc<Daemon>,
    cwd: String,
    db: Arc<Db>,
    sock: std::path::PathBuf,
    _proj_dir: tempfile::TempDir,
    _reg_dir: tempfile::TempDir,
    _sock_dir: tempfile::TempDir,
}
impl Drop for Env {
    fn drop(&mut self) {
        self.daemon.shutdown();
    }
}

fn setup() -> Env {
    let proj_dir = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(proj_dir.path().join(".autosk")).unwrap();
    {
        let db = Db::open_or_create(proj_dir.path().join(".autosk").join("db")).unwrap();
        db.with_write(|conn| {
            migrate::apply_schema_only(conn)?;
            conn.execute_batch(FIXTURE_SQL)?;
            Ok(())
        })
        .unwrap();
    }
    let reg_dir = tempfile::tempdir().unwrap();
    let registry = Arc::new(Registry::open_at(reg_dir.path().join("projects.json")));
    let mgr = Arc::new(Manager::new());
    let daemon = Daemon::new(
        mgr,
        registry,
        DaemonConfig {
            poll_interval: Duration::from_secs(3600),
            gc_interval: None,
            ..DaemonConfig::default()
        },
    );
    let cwd = proj_dir.path().to_string_lossy().to_string();
    let proj = daemon.resolve(&cwd, "").unwrap();
    let db = Arc::clone(&proj.db);

    let sock_dir = tempfile::tempdir().unwrap();
    let sock = sock_dir.path().join("d.sock");
    let listener = uds::listen(&sock).unwrap();
    let server = Arc::new(Server::new(Arc::clone(&daemon)));
    std::thread::spawn(move || server.serve(listener));

    Env {
        daemon,
        cwd,
        db,
        sock,
        _proj_dir: proj_dir,
        _reg_dir: reg_dir,
        _sock_dir: sock_dir,
    }
}

fn connect(sock: &std::path::Path) -> UnixStream {
    for _ in 0..100 {
        if let Ok(s) = UnixStream::connect(sock) {
            return s;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
    panic!("connect");
}

fn call(
    conn: &mut BufReader<UnixStream>,
    w: &mut UnixStream,
    id: u64,
    method: &str,
    params: Value,
) -> Value {
    let req = json!({"id": id, "method": method, "params": params});
    let mut line = serde_json::to_vec(&req).unwrap();
    line.push(b'\n');
    w.write_all(&line).unwrap();
    w.flush().unwrap();
    loop {
        let mut s = String::new();
        conn.read_line(&mut s).unwrap();
        let v: Value = serde_json::from_str(&s).unwrap();
        // Skip any notification frames that arrive on this connection.
        if v.get("method").is_some() {
            continue;
        }
        assert_eq!(v["id"], json!(id));
        return v;
    }
}

/// Creates a running run for the fixture task + registers a fake runner.
fn running_run(env: &Env, fake: Arc<FakeRunner>) -> String {
    let run = env
        .db
        .run_create(&NewRun {
            task_id: "ask-000001".into(),
            step_id: "st".into(),
            max_corrections: 3,
        })
        .unwrap();
    env.db.run_mark_running(&run.job_id, 9).unwrap();
    env.daemon.runners.register(&run.job_id, fake);
    run.job_id
}

#[test]
fn job_input_dispatches_per_streaming_state_with_retry() {
    let env = setup();
    let fake = FakeRunner::new();
    let job = running_run(&env, Arc::clone(&fake));
    let s = connect(&env.sock);
    let mut w = s.try_clone().unwrap();
    let mut conn = BufReader::new(s);
    let mut id = 0u64;
    let mut next = || {
        id += 1;
        id
    };

    // idle → prompt
    fake.streaming.store(false, Ordering::SeqCst);
    let r = call(
        &mut conn,
        &mut w,
        next(),
        "job.input",
        json!({"cwd": env.cwd, "job_id": job, "message": "hi"}),
    );
    assert_eq!(r["result"]["dispatched"], json!("prompt"), "{r}");

    // streaming + default → steer
    fake.streaming.store(true, Ordering::SeqCst);
    let r = call(
        &mut conn,
        &mut w,
        next(),
        "job.input",
        json!({"cwd": env.cwd, "job_id": job, "message": "x"}),
    );
    assert_eq!(r["result"]["dispatched"], json!("steer"), "{r}");

    // streaming + follow_up → follow_up
    let r = call(
        &mut conn,
        &mut w,
        next(),
        "job.input",
        json!({"cwd": env.cwd, "job_id": job, "message": "x", "streaming_behavior": "follow_up"}),
    );
    assert_eq!(r["result"]["dispatched"], json!("follow_up"), "{r}");

    // State-mismatch retry: idle shape (prompt) rejected with "not streaming",
    // retried with the opposite (steer) shape which succeeds.
    fake.streaming.store(false, Ordering::SeqCst);
    fake.scripted
        .lock()
        .unwrap()
        .push_back((false, "not streaming".into()));
    fake.scripted
        .lock()
        .unwrap()
        .push_back((true, String::new()));
    let r = call(
        &mut conn,
        &mut w,
        next(),
        "job.input",
        json!({"cwd": env.cwd, "job_id": job, "message": "x"}),
    );
    assert_eq!(
        r["result"]["dispatched"],
        json!("steer"),
        "retry flipped prompt→steer: {r}"
    );
}

#[test]
fn job_abort_reaches_live_runner() {
    let env = setup();
    let fake = FakeRunner::new();
    let job = running_run(&env, Arc::clone(&fake));
    let s = connect(&env.sock);
    let mut w = s.try_clone().unwrap();
    let mut conn = BufReader::new(s);
    let r = call(
        &mut conn,
        &mut w,
        1,
        "job.abort",
        json!({"cwd": env.cwd, "job_id": job}),
    );
    assert_eq!(r["result"]["ok"], json!(true), "{r}");
    assert!(
        fake.aborted.load(Ordering::SeqCst),
        "abort reached the runner"
    );
}

#[test]
fn job_subscribe_replays_tails_and_emits_done() {
    let env = setup();
    let fake = FakeRunner::new();
    let job = running_run(&env, Arc::clone(&fake));

    // Seed a session file with two events; record session_path on the run.
    let session = env._proj_dir.path().join("session.jsonl");
    let ev = |t: &str| {
        format!(
            "{}\n",
            json!({"type":"message","message":{"role":"assistant","content":[{"type":"text","text":t}]}})
        )
    };
    std::fs::write(&session, format!("{}{}", ev("e1"), ev("e2"))).unwrap();
    env.db
        .run_set_pi_session(&job, "sess", &session.to_string_lossy())
        .unwrap();

    let s = connect(&env.sock);
    let mut w = s.try_clone().unwrap();
    s.set_read_timeout(Some(Duration::from_millis(400)))
        .unwrap();
    let mut conn = BufReader::new(s);

    // Subscribe with attach.
    let req = json!({"id": 1, "method": "job.subscribe", "params": {"cwd": env.cwd, "job_id": job, "attach": true, "full": true}});
    let mut line = serde_json::to_vec(&req).unwrap();
    line.push(b'\n');
    w.write_all(&line).unwrap();
    w.flush().unwrap();

    let mut frames: Vec<Value> = Vec::new();
    let collect = |conn: &mut BufReader<UnixStream>, frames: &mut Vec<Value>, dur: Duration| {
        let deadline = Instant::now() + dur;
        while Instant::now() < deadline {
            let mut sline = String::new();
            match conn.read_line(&mut sline) {
                Ok(0) => break,
                Ok(_) => {
                    if let Ok(v) = serde_json::from_str::<Value>(sline.trim()) {
                        frames.push(v);
                    }
                }
                Err(_) => {} // read timeout; keep polling until deadline
            }
        }
    };
    // Replay + initial status.
    collect(&mut conn, &mut frames, Duration::from_millis(700));
    // Attach counter bumped while subscribed.
    assert_eq!(env.daemon.attachments.count(&job), 1, "attach counted");

    // Append a third event → expect a live message frame.
    {
        use std::io::Write as _;
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .open(&session)
            .unwrap();
        f.write_all(ev("e3").as_bytes()).unwrap();
    }
    collect(&mut conn, &mut frames, Duration::from_millis(700));

    // Terminal: mark the run done → expect a done frame; stream ends.
    env.db.run_mark_done(&job, 0, None).unwrap();
    collect(&mut conn, &mut frames, Duration::from_millis(700));

    // ---- assertions ----
    let job_events: Vec<&Value> = frames
        .iter()
        .filter(|f| f["method"] == json!("job-event"))
        .map(|f| &f["params"])
        .collect();
    let texts: Vec<&str> = job_events
        .iter()
        .filter(|p| p["kind"] == json!("message"))
        .filter_map(|p| p["event"]["text"].as_str())
        .collect();
    assert!(
        texts.contains(&"e1") && texts.contains(&"e2"),
        "replayed e1+e2: {texts:?}"
    );
    assert!(
        texts.contains(&"e3"),
        "tailed the live-appended e3: {texts:?}"
    );
    assert!(
        job_events.iter().any(|p| p["kind"] == json!("status")),
        "a status frame was emitted"
    );
    assert!(
        job_events.iter().any(|p| p["kind"] == json!("done")),
        "a done frame closed the stream"
    );
    // Replay cursor (event_id) is 1-based and monotonic on message frames.
    let ids: Vec<i64> = job_events
        .iter()
        .filter(|p| p["kind"] == json!("message"))
        .filter_map(|p| p["event_id"].as_i64())
        .collect();
    assert!(
        ids.windows(2).all(|w| w[0] < w[1]),
        "event_id monotonic: {ids:?}"
    );
}
