//! End-to-end acceptance (#1): a real task enrolled into `feature-dev-generic`
//! runs to human-park under the native poller + scheduler + executor, with the
//! same outcome the Go daemon produced — dev → review → docs → validator →
//! human, advancing through the workflow graph one step per run.

use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use autosk_core::ctx::Ctx;
use autosk_core::executor::{Config, Deps, Executor, PiFactory};
use autosk_core::pi::{Command, Response, SessionInfo};
use autosk_core::poller::Poller;
use autosk_core::runner::{PiRunner, RunnerError};
use autosk_core::scheduler::{Config as SchedConfig, Job, SchedExecutor, Scheduler};
use autosk_core::store::Db;
use autosk_core::tasks::{self, Task};
use autosk_core::worktree::Manager as WtManager;

// A stub runner that, on each prompt, looks up the single running run, maps its
// step to the canonical happy-path transition, and emits it — exactly what the
// real @autogent/generic agent does by calling `autosk step next`.
struct AutoStub {
    db: Arc<Db>,
    turn_tx: SyncSender<()>,
    turn_rx: Mutex<Receiver<()>>,
}

impl PiRunner for AutoStub {
    fn pid(&self) -> i32 {
        1
    }
    fn get_state(&self, _c: &Ctx) -> Result<SessionInfo, RunnerError> {
        Ok(SessionInfo::default())
    }
    fn send_prompt(&self, _c: &Ctx, _m: &str) -> Result<(), RunnerError> {
        // Find the one running run + its step, then emit the canonical target.
        let (task_id, step_name): (String, String) = self
            .db
            .with_write(|conn| {
                Ok(conn.query_row(
                    "SELECT dr.task_id, s.name FROM daemon_runs dr \
                       JOIN steps s ON s.id = dr.step_id \
                      WHERE dr.status='running' ORDER BY dr.created_at DESC LIMIT 1",
                    [],
                    |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)),
                )?)
            })
            .expect("running run");
        let target = match step_name.as_str() {
            "dev" => "review",
            "review" => "docs",
            "docs" => "validator",
            "validator" => "human",
            other => panic!("unexpected step {other}"),
        };
        self.db.signal_emit(&task_id, target).expect("emit");
        let _ = self.turn_tx.try_send(());
        Ok(())
    }
    fn wait_for_agent_end(&self, ctx: &Ctx) -> Result<(), RunnerError> {
        let rx = self.turn_rx.lock().unwrap();
        loop {
            match rx.recv_timeout(Duration::from_millis(5)) {
                Ok(()) => return Ok(()),
                Err(_) => {
                    if ctx.is_cancelled() {
                        return Err(RunnerError::Cancelled);
                    }
                }
            }
        }
    }
    fn abort(&self, _c: &Ctx) -> Result<(), RunnerError> {
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
    fn send_command(&self, _c: Command) -> Result<Receiver<Response>, RunnerError> {
        Err(RunnerError::Io("stub".into()))
    }
    fn is_streaming(&self) -> bool {
        false
    }
}

struct Adapter {
    exec: Arc<Executor>,
}
impl SchedExecutor for Adapter {
    fn run(&self, ctx: &Ctx, job: &Job) {
        let _ = self.exec.run(ctx, &job.id);
    }
}

fn exec_w(db: &Arc<Db>, sql: &str) {
    db.with_write(|c| {
        c.execute_batch(sql)?;
        Ok(())
    })
    .unwrap();
}

#[test]
fn feature_dev_generic_runs_to_human_park() {
    let tmp = tempfile::tempdir().unwrap();
    let dbp = tmp.path().join("test.db");
    let db = Arc::new(Db::open_or_create(&dbp).unwrap());
    db.migrate().unwrap();

    // Packages: one @autogent/generic agent.
    let prefix = tmp.path().join("packages");
    std::fs::create_dir_all(&prefix).unwrap();
    autosk_core::pkg::install_stub(
        &prefix,
        "@autogent/generic",
        "0.0.1",
        serde_json::json!({"model":"sonnet","thinking":"high"}),
    )
    .unwrap();

    // Seed feature-dev-generic (isolation=none for a deterministic e2e; the
    // worktree-isolation path is covered by the real-git worktree test).
    exec_w(
        &db,
        "INSERT INTO agents(id,name,is_human,created_at) VALUES ('ag-gen','@autogent/generic',0,1);\n\
         INSERT INTO workflows(id,name,description,first_step_id,is_synthetic,isolation,created_at) \
           VALUES ('wf-g','feature-dev-generic','',' st-dev',0,'none',1);",
    );
    // (workflows.first_step_id corrected below; the leading space above would
    // break find — set it explicitly.)
    exec_w(
        &db,
        "UPDATE workflows SET first_step_id='st-dev' WHERE id='wf-g';",
    );
    for (id, name, seq) in [
        ("st-dev", "dev", 0),
        ("st-rev", "review", 1),
        ("st-doc", "docs", 2),
        ("st-val", "validator", 3),
    ] {
        exec_w(
            &db,
            &format!(
                "INSERT INTO steps(id,workflow_id,name,agent_id,seq,agent_params,max_visits) \
                 VALUES ('{id}','wf-g','{name}','ag-gen',{seq},NULL,0);"
            ),
        );
    }
    // Transitions matching feature-dev-generic.
    let trs = [
        ("st-dev", Some("st-rev"), None),
        ("st-rev", Some("st-doc"), None),
        ("st-rev", Some("st-dev"), None),
        ("st-doc", Some("st-val"), None),
        ("st-val", Some("st-dev"), None),
        ("st-val", None, Some("human")),
    ];
    for (step, next, status) in trs {
        match (next, status) {
            (Some(n), None) => exec_w(
                &db,
                &format!(
                    "INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) VALUES ('{step}','{n}',NULL,'.');"
                ),
            ),
            (None, Some(s)) => exec_w(
                &db,
                &format!(
                    "INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) VALUES ('{step}',NULL,'{s}','.');"
                ),
            ),
            _ => unreachable!(),
        }
    }

    // Enroll a task at the first step.
    let task_id = "ask-abc123".to_string();
    db.task_create(Task {
        id: task_id.clone(),
        title: "Implement feature".into(),
        description: "do the thing".into(),
        status: tasks::STATUS_WORK.into(),
        priority: 2,
        author_id: String::new(),
        workflow_id: "wf-g".into(),
        current_step_id: "st-dev".into(),
        metadata: serde_json::Map::new(),
        created_at: 0,
        updated_at: 0,
    })
    .unwrap();

    // Wire the executor + scheduler + poller.
    let factory: PiFactory = {
        let db = Arc::clone(&db);
        Arc::new(move |_ctx, _opts| {
            let (tx, rx) = sync_channel(8);
            Ok(Arc::new(AutoStub {
                db: Arc::clone(&db),
                turn_tx: tx,
                turn_rx: Mutex::new(rx),
            }) as Arc<dyn PiRunner>)
        })
    };
    let deps = Deps {
        db: Arc::clone(&db),
        tasks: Arc::clone(&db) as Arc<dyn autosk_core::wfengine::TaskWriter>,
        packages: Arc::new(autosk_core::pkg::Registry::open(prefix.clone())),
        worktree: Arc::new(WtManager::new()),
        runners: None,
        attachments: None,
    };
    let cfg = Config {
        project_root: tmp.path().to_string_lossy().to_string(),
        grace: Duration::from_millis(50),
        idle_timeout: Duration::from_secs(5),
        session_poll_budget: Duration::from_millis(1),
        ..Default::default()
    };
    let exec = Arc::new(Executor::new(deps, factory, cfg));
    let sched = Scheduler::new(
        Arc::new(Adapter { exec }) as Arc<dyn SchedExecutor>,
        SchedConfig {
            workers: 1,
            queue_depth: 16,
        },
    );
    sched.start();
    let poller = Poller::new(
        Arc::clone(&db),
        Arc::clone(&sched),
        tmp.path().to_string_lossy().to_string(),
        Duration::from_millis(50),
    );
    poller.start();

    // Wait for the task to reach human-park.
    let deadline = Instant::now() + Duration::from_secs(15);
    let mut final_status = String::new();
    while Instant::now() < deadline {
        let tk = db.task_get_row(&task_id).unwrap();
        final_status = tk.status.clone();
        if final_status == "human" {
            break;
        }
        std::thread::sleep(Duration::from_millis(25));
    }
    poller.stop();
    sched.stop();

    assert_eq!(final_status, "human", "task should reach human-park");
    // The validator step is preserved on a human park (resume rewinds here).
    let tk = db.task_get_row(&task_id).unwrap();
    assert_eq!(tk.current_step_id, "st-val");

    // Every step ran to a done run with a recorded transition; at least 4 runs
    // (dev, review, docs, validator) all succeeded.
    let runs = db
        .run_list(&autosk_core::runstore::RunFilter::default())
        .unwrap();
    let done = runs.iter().filter(|r| r.status == "done").count();
    assert!(
        done >= 4,
        "expected ≥4 done runs, got {done} ({runs:?})",
        runs = runs.len()
    );
    assert!(
        runs.iter().all(|r| r.status == "done"),
        "no failed runs expected on the happy path"
    );
}
