//! Read-RPC golden (acceptance criterion #2).
//!
//! Because autoskd is greenfield (v12-only), there is no shared on-disk DB to
//! diff against Go. Instead the fixture is seeded via the Rust migrator (schema)
//! plus deterministic INSERTs, read back through `autosk-core`'s read paths, and
//! the `autosk-proto`-serialised result is compared to checked-in golden JSON
//! (`tests/fixtures/read/*.json`). Targeted assertions additionally pin the
//! load-bearing derived values so the golden is not the sole check.
//!
//! Set `AUTOSK_UPDATE_GOLDEN=1` to regenerate the golden files after an
//! intentional contract change.

use std::path::Path;

use autosk_core::{migrate, read::JobFilter, read::TaskFilter, Db};
use serde::Serialize;

fn build_fixture() -> (tempfile::TempDir, Db) {
    let dir = tempfile::tempdir().expect("tempdir");
    let db = Db::open_or_create(dir.path().join("db")).expect("create db");
    db.with_write(|conn| {
        migrate::apply_schema_only(conn)?;
        conn.execute_batch(FIXTURE_SQL)?;
        Ok(())
    })
    .expect("seed fixture");
    (dir, db)
}

/// Deterministic fixture (fixed ids + unix timestamps) covering every table the
/// read surface touches, plus the Go behavioural quirks the port mirrors.
/// `ask-000003` is blocked by a `new` task (so `is_blocked` is true via the
/// legacy `IN ('new','claimed')` rule) AND a `work` task; `task.ready` must
/// surface only `ask-000002` (no open blocker); the `review` step records a
/// terminal (`done`) and a kickback (`->dev`) transition in id order.
const FIXTURE_SQL: &str = "\
INSERT INTO agents(id,name,is_human,created_at) VALUES\
 ('ag-0001','human',1,1700000000),\
 ('ag-0002','@autogent/generic',0,1700000001);\
INSERT INTO workflows(id,name,description,first_step_id,is_synthetic,isolation,created_at) VALUES\
 ('wf-0001','feature-dev','Feature development','st-0001',0,'worktree',1700000010),\
 ('wf-0002','single:@autogent/generic','','st-0003',1,'none',1700000011);\
INSERT INTO steps(id,workflow_id,name,agent_id,seq,agent_params,max_visits) VALUES\
 ('st-0001','wf-0001','dev','ag-0002',0,NULL,0),\
 ('st-0002','wf-0001','review','ag-0001',1,NULL,0),\
 ('st-0003','wf-0002','run','ag-0002',0,NULL,0);\
INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) VALUES\
 ('st-0001','st-0002',NULL,'full'),\
 ('st-0002',NULL,'done','full'),\
 ('st-0002','st-0001',NULL,'full');\
INSERT INTO tasks(id,title,description,status,priority,author_id,workflow_id,current_step_id,metadata,created_at,updated_at) VALUES\
 ('ask-000001','Build read core','Port the read core',  'work',1,'ag-0001','wf-0001','st-0001','{\"step_visits\":{\"st-0001\":1}}',1700000100,1700000200),\
 ('ask-000002','Write docs','',                          'new', 2,'ag-0001',NULL,NULL,NULL,1700000101,1700000101),\
 ('ask-000003','Blocked task','',                        'new', 0,NULL,NULL,NULL,NULL,1700000102,1700000102),\
 ('ask-000004','Done task','',                           'done',2,NULL,NULL,NULL,NULL,1700000103,1700000103);\
INSERT INTO task_deps(blocker_id,blocked_id,kind) VALUES\
 ('ask-000002','ask-000003','blocks'),\
 ('ask-000001','ask-000003','blocks');\
INSERT INTO comments(task_id,author_id,text,created_at) VALUES\
 ('ask-000001','ag-0001','Kickoff note',1700000150),\
 ('ask-000001','ag-0002','Working on it',1700000160);\
INSERT INTO daemon_runs(job_id,task_id,step_id,status,transition_id,exit_code,pid,pi_session_id,session_path,error,max_corrections,corrections_used,created_at,started_at,finished_at) VALUES\
 ('job-000001','ask-000001','st-0001','done',1,0,NULL,'pi-sess-1',NULL,NULL,3,0,1700000170,1700000171,1700000180),\
 ('job-000002','ask-000001','st-0001','running',NULL,NULL,4242,'pi-sess-2',NULL,NULL,3,1,1700000190,1700000191,NULL);\
INSERT INTO step_signals(run_id,task_id,transition_id,created_at) VALUES\
 ('job-000001','ask-000001',1,1700000179);\
";

fn assert_golden(name: &str, value: &impl Serialize) {
    let mut got = serde_json::to_string_pretty(value).expect("serialize");
    got.push('\n');
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/read")
        .join(name);
    if std::env::var("AUTOSK_UPDATE_GOLDEN").is_ok() || !path.exists() {
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, &got).unwrap();
        return;
    }
    let want = std::fs::read_to_string(&path).unwrap_or_default();
    assert_eq!(
        got, want,
        "golden mismatch for {name} (AUTOSK_UPDATE_GOLDEN=1 to refresh)"
    );
}

#[test]
fn task_list_golden() {
    let (_g, db) = build_fixture();
    let tasks = db.task_list(&TaskFilter::default()).expect("task_list");
    // Default = open statuses {new,work,human}; ask-000004 (done) excluded.
    assert_eq!(tasks.len(), 3, "default filter excludes the done task");
    // Order: priority ASC, created_at ASC → work(p1) before new(p0)? No: p0 < p1
    // < p2. ask-000003 p0, ask-000001 p1, ask-000002 p2.
    let ids: Vec<&str> = tasks.iter().map(|t| t.id.as_str()).collect();
    assert_eq!(ids, vec!["ask-000003", "ask-000001", "ask-000002"]);
    assert_golden("task_list_open.json", &tasks);
}

#[test]
fn task_get_enrichment() {
    let (_g, db) = build_fixture();
    let t = db.task_get("ask-000001").expect("task_get");
    assert_eq!(t.author_name, "human");
    assert_eq!(t.workflow_name, "feature-dev");
    assert_eq!(t.step_name, "dev");
    assert_eq!(t.agent_name, "@autogent/generic");
    assert!(!t.blocked, "ask-000001 has no incoming blockers");
    assert!(t.blocked_by.is_empty());
    assert_eq!(t.blocks.len(), 1);
    assert_eq!(t.blocks[0].id, "ask-000003");
    assert_eq!(t.comment_count, 2);
    assert!(t.metadata.is_some());
    assert_golden("task_get_ask000001.json", &t);

    // The legacy IN('new','claimed') quirk: ask-000003 is blocked because one of
    // its blockers (ask-000002) is 'new', even though the other ('work') is not.
    let b = db.task_get("ask-000003").expect("get blocked task");
    assert!(b.blocked, "ask-000003 blocked by a 'new' task");
    let by: Vec<&str> = b.blocked_by.iter().map(|r| r.id.as_str()).collect();
    assert_eq!(by, vec!["ask-000001", "ask-000002"], "blocker_id ASC");
    assert_golden("task_get_ask000003.json", &b);
}

#[test]
fn task_ready_golden() {
    let (_g, db) = build_fixture();
    let ready = db.task_ready(0).expect("task_ready");
    let ids: Vec<&str> = ready.iter().map(|t| t.id.as_str()).collect();
    assert_eq!(
        ids,
        vec!["ask-000002"],
        "only the unblocked 'new' task is ready"
    );
    assert_golden("task_ready.json", &ready);
}

#[test]
fn comments_golden() {
    let (_g, db) = build_fixture();
    let cs = db.comment_list("ask-000001").expect("comment_list");
    assert_eq!(cs.len(), 2);
    assert_eq!(cs[0].text, "Kickoff note"); // oldest first
    assert_golden("comments_ask000001.json", &cs);
}

#[test]
fn agents_golden() {
    let (_g, db) = build_fixture();
    let agents = db.agent_list().expect("agent_list");
    let names: Vec<&str> = agents.iter().map(|a| a.name.as_str()).collect();
    assert_eq!(names, vec!["@autogent/generic", "human"], "name ASC");
    let generic = &agents[0];
    assert_eq!(generic.source, "db_only");
    assert_eq!(generic.tasks_owned, 1, "owns ask-000001 via current step");
    let human = &agents[1];
    assert_eq!(human.source, "builtin");
    assert_eq!(human.tasks_owned, 2, "authored ask-000001 + ask-000002");
    assert_golden("agents.json", &agents);
}

#[test]
fn workflows_golden() {
    let (_g, db) = build_fixture();
    let wfs = db.workflow_list(false).expect("workflow_list");
    assert_eq!(wfs.len(), 1, "synthetic workflow hidden by default");
    let w = &wfs[0];
    assert_eq!(w.name, "feature-dev");
    assert_eq!(w.first_step, "dev");
    assert_eq!(w.isolation, "worktree");
    assert_eq!(w.steps.len(), 2);
    assert_eq!(w.steps[0].next_steps, vec!["review"]);
    assert_eq!(w.steps[1].next_status, vec!["done"]);
    assert_eq!(w.steps[1].next_steps, vec!["dev"]); // kickback
    assert_eq!(w.non_terminal_task_count, 1);
    assert_golden("workflows.json", &wfs);

    // include_synthetic surfaces the single:<agent> row too.
    let all = db.workflow_list(true).expect("workflow_list synthetic");
    assert_eq!(all.len(), 2);
}

#[test]
fn jobs_golden() {
    let (_g, db) = build_fixture();
    let jobs = db.job_list(&JobFilter::default()).expect("job_list");
    // created_at DESC, job_id DESC.
    let ids: Vec<&str> = jobs.iter().map(|j| j.job_id.as_str()).collect();
    assert_eq!(ids, vec!["job-000002", "job-000001"]);
    let done = jobs.iter().find(|j| j.job_id == "job-000001").unwrap();
    assert_eq!(done.duration_ms, 9000, "finished - started, in ms");
    assert_eq!(done.transition_id, Some(1));
    assert_eq!(done.step_name, "dev");
    assert_eq!(done.workflow_name, "feature-dev");
    assert!(!done.streaming);
    assert_eq!(done.attach_count, 0);
    assert_golden("jobs.json", &jobs);

    let j = db.job_get("job-000002").expect("job_get");
    assert_eq!(j.status, "running");
    assert_eq!(j.pid, Some(4242));
}

#[test]
fn signals_golden() {
    let (_g, db) = build_fixture();
    let by_task = db.signal_for_task("ask-000001").expect("signal_for_task");
    assert_eq!(by_task.len(), 1);
    assert_eq!(
        by_task[0].target, "review",
        "transition 1 advances dev→review"
    );
    assert_eq!(by_task[0].step_name, "dev");
    assert_eq!(by_task[0].workflow_name, "feature-dev");
    assert_golden("signals_ask000001.json", &by_task);

    let by_job = db.signal_for_job("job-000001").expect("signal_for_job");
    assert_eq!(by_job, by_task, "the single signal belongs to job-000001");
}
