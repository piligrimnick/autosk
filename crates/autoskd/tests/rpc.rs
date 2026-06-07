//! In-process integration test for the JSON-RPC server (acceptance #1, #2).
//!
//! Seeds a fresh v12 fixture DB (Rust migrator + INSERTs), binds the
//! single-instance UDS, runs the `Server` in a background thread, and drives
//! the read surface over a real `UnixStream` exactly as the Go client will.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::sync::Arc;

use autosk_core::projectmgr::Manager;
use autosk_core::registry::Registry;
use autosk_core::{migrate, Db};
use autoskd::server::Server;
use autoskd::uds;
use serde_json::{json, Value};

const FIXTURE_SQL: &str = "\
INSERT INTO agents(id,name,is_human,created_at) VALUES\
 ('ag-0001','human',1,1700000000),('ag-0002','@autogent/generic',0,1700000001);\
INSERT INTO workflows(id,name,description,first_step_id,is_synthetic,isolation,created_at) VALUES\
 ('wf-0001','feature-dev','','st-0001',0,'worktree',1700000010);\
INSERT INTO steps(id,workflow_id,name,agent_id,seq,agent_params,max_visits) VALUES\
 ('st-0001','wf-0001','dev','ag-0002',0,NULL,0),('st-0002','wf-0001','review','ag-0001',1,NULL,0);\
INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) VALUES\
 ('st-0001','st-0002',NULL,'full'),('st-0002',NULL,'done','full');\
INSERT INTO tasks(id,title,description,status,priority,author_id,workflow_id,current_step_id,metadata,created_at,updated_at) VALUES\
 ('ask-000001','Build read core','',\
   'work',1,'ag-0001','wf-0001','st-0001',NULL,1700000100,1700000200),\
 ('ask-000002','Write docs','','new',2,'ag-0001',NULL,NULL,NULL,1700000101,1700000101);\
INSERT INTO comments(task_id,author_id,text,created_at) VALUES\
 ('ask-000001','ag-0001','Kickoff',1700000150);\
INSERT INTO daemon_runs(job_id,task_id,step_id,status,transition_id,exit_code,pid,pi_session_id,session_path,error,max_corrections,corrections_used,created_at,started_at,finished_at) VALUES\
 ('job-000001','ask-000001','st-0001','done',1,0,NULL,'s1',NULL,NULL,3,0,1700000170,1700000171,1700000180);\
INSERT INTO step_signals(run_id,task_id,transition_id,created_at) VALUES\
 ('job-000001','ask-000001',1,1700000179);\
";

struct Harness {
    proj_dir: tempfile::TempDir,
    _reg_dir: tempfile::TempDir,
    _sock_dir: tempfile::TempDir,
    conn: BufReader<UnixStream>,
    writer: UnixStream,
    next_id: u64,
}

impl Harness {
    fn start() -> Harness {
        let proj_dir = tempfile::tempdir().unwrap();
        // Seed the fixture DB in-process, then drop the handle before the daemon
        // opens it (sequential single-writer access).
        {
            std::fs::create_dir_all(proj_dir.path().join(".autosk")).unwrap();
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

        let sock_dir = tempfile::tempdir().unwrap();
        let sock = sock_dir.path().join("d.sock");
        let listener = uds::listen(&sock).expect("bind");
        let server = Arc::new(Server::new(mgr, registry));
        std::thread::spawn(move || server.serve(listener));

        // Connect (the socket is listenable as soon as bind returned).
        let stream = connect_with_backoff(&sock);
        let writer = stream.try_clone().unwrap();
        Harness {
            proj_dir,
            _reg_dir: reg_dir,
            _sock_dir: sock_dir,
            conn: BufReader::new(stream),
            writer,
            next_id: 0,
        }
    }

    fn cwd(&self) -> String {
        self.proj_dir.path().to_string_lossy().to_string()
    }

    fn call(&mut self, method: &str, params: Value) -> Value {
        self.next_id += 1;
        let id = self.next_id;
        let req = json!({"id": id, "method": method, "params": params});
        let mut line = serde_json::to_vec(&req).unwrap();
        line.push(b'\n');
        self.writer.write_all(&line).unwrap();
        self.writer.flush().unwrap();
        let mut resp = String::new();
        self.conn.read_line(&mut resp).unwrap();
        let v: Value = serde_json::from_str(&resp).unwrap();
        assert_eq!(v["id"], json!(id), "response id mismatch");
        v
    }

    fn result(&mut self, method: &str, params: Value) -> Value {
        let v = self.call(method, params);
        assert!(
            v.get("error").is_none(),
            "{method} returned error: {}",
            v["error"]
        );
        v["result"].clone()
    }
}

fn connect_with_backoff(sock: &std::path::Path) -> UnixStream {
    for _ in 0..100 {
        if let Ok(s) = UnixStream::connect(sock) {
            return s;
        }
        std::thread::sleep(std::time::Duration::from_millis(5));
    }
    panic!("could not connect to {}", sock.display());
}

#[test]
fn serves_meta_and_read_surface() {
    let mut h = Harness::start();
    let cwd = h.cwd();

    // ---- meta ----
    let version = h.result("version", Value::Null);
    assert!(version["version"].as_str().unwrap().contains("phase1"));

    let health = h.result("healthz", Value::Null);
    assert_eq!(health["ok"], json!(true), "liveness probe");

    let scoped = h.result("healthz", json!({"cwd": cwd}));
    assert_eq!(scoped["ok"], json!(true));
    assert_eq!(scoped["running"], json!(0));
    assert!(scoped["project_root"]
        .as_str()
        .unwrap()
        .ends_with(h.proj_dir.path().file_name().unwrap().to_str().unwrap()));

    // ---- project registry ----
    let added = h.result("project.add", json!({"cwd": cwd}));
    assert!(!added["root"].as_str().unwrap().is_empty());
    let list = h.result("project.list", Value::Null);
    assert_eq!(list.as_array().unwrap().len(), 1);

    // ---- tasks ----
    let tasks = h.result("task.list", json!({"cwd": cwd}));
    let arr = tasks.as_array().unwrap();
    assert_eq!(arr.len(), 2, "two open tasks");
    let one = h.result("task.get", json!({"cwd": cwd, "id": "ask-000001"}));
    assert_eq!(one["workflow_name"], json!("feature-dev"));
    assert_eq!(one["step_name"], json!("dev"));
    assert_eq!(one["agent_name"], json!("@autogent/generic"));
    assert_eq!(one["comment_count"], json!(1));

    let ready = h.result("task.ready", json!({"cwd": cwd}));
    let ready_ids: Vec<&str> = ready
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["id"].as_str().unwrap())
        .collect();
    assert_eq!(ready_ids, vec!["ask-000002"]);

    // ---- comments / agents / workflows ----
    let comments = h.result("comment.list", json!({"cwd": cwd, "task_id": "ask-000001"}));
    assert_eq!(comments.as_array().unwrap().len(), 1);

    let agents = h.result("agent.list", json!({"cwd": cwd}));
    assert_eq!(agents.as_array().unwrap().len(), 2);

    let wfs = h.result("workflow.list", json!({"cwd": cwd}));
    assert_eq!(wfs.as_array().unwrap().len(), 1);
    let wf = h.result("workflow.get", json!({"cwd": cwd, "name": "feature-dev"}));
    assert_eq!(wf["first_step"], json!("dev"));

    // ---- jobs / signals ----
    let jobs = h.result("job.list", json!({"cwd": cwd}));
    assert_eq!(jobs.as_array().unwrap().len(), 1);
    let job = h.result("job.get", json!({"cwd": cwd, "id": "job-000001"}));
    assert_eq!(job["duration_ms"], json!(9000));

    let msgs = h.result("job.messages", json!({"cwd": cwd, "job_id": "job-000001"}));
    assert_eq!(
        msgs.as_array().unwrap().len(),
        0,
        "no session file → no events"
    );

    let sigs = h.result(
        "signal.forTask",
        json!({"cwd": cwd, "task_id": "ask-000001"}),
    );
    assert_eq!(sigs.as_array().unwrap()[0]["target"], json!("review"));

    // ---- errors ----
    let bogus = h.call("nope.nope", Value::Null);
    assert_eq!(bogus["error"]["code"], json!(-32601), "method not found");

    let missing = h.call("task.get", json!({"cwd": cwd, "id": "ask-zzzzzz"}));
    assert_eq!(missing["error"]["code"], json!(1003), "NOT_FOUND");

    let no_project = h.call("task.list", json!({"cwd": "/nonexistent/dir"}));
    assert_eq!(
        no_project["error"]["code"],
        json!(1001),
        "PROJECT_NOT_FOUND"
    );

    // ---- project.remove ----
    let removed = h.result("project.remove", json!({"cwd": cwd}));
    assert_eq!(removed["removed"], json!(true));
}

#[test]
fn single_instance_binding_rejects_second_listener() {
    let dir = tempfile::tempdir().unwrap();
    let sock = dir.path().join("d.sock");
    let _first = uds::listen(&sock).expect("first bind");
    match uds::listen(&sock) {
        Err(uds::ListenError::AlreadyRunning) => {}
        other => panic!("expected AlreadyRunning, got {other:?}"),
    }
}

#[test]
fn stale_socket_is_reaped() {
    let dir = tempfile::tempdir().unwrap();
    let sock = dir.path().join("d.sock");
    {
        let _l = uds::listen(&sock).expect("bind");
        // drop the listener → socket file remains but no peer accepts.
    }
    assert!(sock.exists(), "socket file lingers after listener drop");
    // A fresh listen must reap the stale socket and bind.
    let _l2 = uds::listen(&sock).expect("rebind over stale socket");
}
