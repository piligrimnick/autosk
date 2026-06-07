//! Executor behavioural-parity harness (plan §8.1). Drives the Rust executor
//! through the same turn sequences as the Go `internal/daemon/executor` tests
//! and asserts identical `daemon_runs` / task outcomes:
//!
//!   * transition emitted → run done + task advanced;
//!   * task_status=human → parked, step preserved;
//!   * missed transition → kickback up to max_corrections → fail + park;
//!   * terminal done clears the step pointer;
//!   * agent_config_invalid → fail + park (step preserved);
//!   * max_visits cap-fire → fail + park on the TARGET step;
//!   * generic advance error → fail + park on the TARGET step;
//!   * runner crash before signal → fail + park on the SOURCE step;
//!   * signal-honoured-after-reader-error and -after-idle-timeout;
//!   * worktree alloc + terminal cleanup + missing auto-recovery + stranded park.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use std::time::Instant;

use autosk_core::ctx::Ctx;
use autosk_core::executor::{Config, Deps, Executor};
use autosk_core::pi::{Command, Response, SessionInfo};
use autosk_core::pirunners::Attachments;
use autosk_core::runner::{PiRunner, RunnerError};
use autosk_core::runstore::NewRun;
use autosk_core::store::Db;
use autosk_core::tasks::{self, Task};
use autosk_core::worktree::{WorktreeError, WorktreeManager, WtOutcome};

// ---- fixture --------------------------------------------------------------

struct Fixture {
    db: Arc<Db>,
    prefix: std::path::PathBuf,
    root: std::path::PathBuf,
    _tmp: tempfile::TempDir,
}

fn new_fixture() -> Fixture {
    let tmp = tempfile::tempdir().unwrap();
    let dbp = tmp.path().join("test.db");
    let db = Db::open_or_create(&dbp).unwrap();
    db.migrate().unwrap();
    let prefix = tmp.path().join("packages");
    std::fs::create_dir_all(&prefix).unwrap();
    Fixture {
        db: Arc::new(db),
        prefix,
        root: tmp.path().to_path_buf(),
        _tmp: tmp,
    }
}

impl Fixture {
    fn seed_agent(&self, id: &str, name: &str) {
        self.db
            .with_write(|c| {
                c.execute(
                    "INSERT INTO agents(id,name,is_human,created_at) VALUES (?1,?2,0,100)",
                    rusqlite::params![id, name],
                )?;
                Ok(())
            })
            .unwrap();
    }

    fn install_pkg(&self, name: &str) {
        autosk_core::pkg::install_stub(
            &self.prefix,
            name,
            "0.0.1",
            serde_json::json!({"model":"sonnet","thinking":"high","first_message":format!("You are the {name} agent.")}),
        )
        .unwrap();
    }

    fn uninstall_pkg(&self, name: &str) {
        // Drop the registry entry so resolve() returns NotInstalled.
        let reg_path = self.prefix.join("registry.json");
        let v: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&reg_path).unwrap()).unwrap();
        let mut agents = v["agents"].as_object().unwrap().clone();
        agents.remove(name);
        let out = serde_json::json!({"schema_version":1,"agents":agents});
        std::fs::write(&reg_path, serde_json::to_vec_pretty(&out).unwrap()).unwrap();
    }

    fn seed_workflow(&self, wf_id: &str, name: &str, isolation: &str, first_step_id: &str) {
        self.db
            .with_write(|c| {
                c.execute(
                    "INSERT INTO workflows(id,name,description,first_step_id,is_synthetic,isolation,created_at) \
                     VALUES (?1,?2,'',?3,0,?4,100)",
                    rusqlite::params![wf_id, name, first_step_id, isolation],
                )?;
                Ok(())
            })
            .unwrap();
    }

    #[allow(clippy::too_many_arguments)]
    fn seed_step(
        &self,
        st_id: &str,
        wf_id: &str,
        name: &str,
        agent_id: &str,
        seq: i64,
        max_visits: i64,
    ) {
        self.db
            .with_write(|c| {
                c.execute(
                    "INSERT INTO steps(id,workflow_id,name,agent_id,seq,agent_params,max_visits) \
                     VALUES (?1,?2,?3,?4,?5,NULL,?6)",
                    rusqlite::params![st_id, wf_id, name, agent_id, seq, max_visits],
                )?;
                Ok(())
            })
            .unwrap();
    }

    fn seed_transition_step(&self, step_id: &str, next_step_id: &str) {
        self.db
            .with_write(|c| {
                c.execute(
                    "INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) \
                     VALUES (?1,?2,NULL,'.')",
                    rusqlite::params![step_id, next_step_id],
                )?;
                Ok(())
            })
            .unwrap();
    }

    fn seed_transition_status(&self, step_id: &str, status: &str) {
        self.db
            .with_write(|c| {
                c.execute(
                    "INSERT INTO step_transitions(step_id,next_step_id,task_status,prompt_rule) \
                     VALUES (?1,NULL,?2,'.')",
                    rusqlite::params![step_id, status],
                )?;
                Ok(())
            })
            .unwrap();
    }

    fn create_task(&self, wf_id: &str, step_id: &str, metadata: serde_json::Value) -> String {
        let id = format!("ask-{:06x}", rand6());
        let md = metadata.as_object().cloned().unwrap_or_default();
        self.db
            .task_create(Task {
                id: id.clone(),
                title: "T".into(),
                description: String::new(),
                status: tasks::STATUS_WORK.into(),
                priority: 2,
                author_id: String::new(),
                workflow_id: wf_id.into(),
                current_step_id: step_id.into(),
                metadata: md,
                created_at: 0,
                updated_at: 0,
            })
            .unwrap();
        id
    }

    fn create_run(&self, task_id: &str, step_id: &str, max_corrections: i64) -> String {
        self.db
            .run_create(&NewRun {
                task_id: task_id.into(),
                step_id: step_id.into(),
                max_corrections,
            })
            .unwrap()
            .job_id
    }

    fn deps(&self, wt: Arc<dyn WorktreeManager>) -> Deps {
        Deps {
            db: Arc::clone(&self.db),
            tasks: Arc::clone(&self.db) as Arc<dyn autosk_core::wfengine::TaskWriter>,
            packages: Arc::new(autosk_core::pkg::Registry::open(self.prefix.clone())),
            worktree: wt,
            runners: None,
            attachments: None,
        }
    }

    fn deps_attach(&self, wt: Arc<dyn WorktreeManager>, att: Arc<Attachments>) -> Deps {
        let mut d = self.deps(wt);
        d.attachments = Some(att);
        d
    }

    fn cfg(&self) -> Config {
        Config {
            project_root: self.root.to_string_lossy().to_string(),
            grace: Duration::from_millis(100),
            idle_timeout: Duration::from_secs(5),
            session_poll_budget: Duration::from_millis(1),
            ..Default::default()
        }
    }
}

fn rand6() -> u32 {
    use std::time::{SystemTime, UNIX_EPOCH};
    let n = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .subsec_nanos();
    n & 0xff_ffff
}

/// A simple two-/three-step workflow shared by most tests:
/// `dev`(→review) `review`(→dev) `validator`(→human). Returns step ids.
fn seed_basic_wf(fx: &Fixture, isolation: &str) -> (String, String, String, String) {
    fx.seed_agent("ag-dev", "developer");
    fx.seed_agent("ag-rev", "code-reviewer");
    fx.seed_agent("ag-val", "task-validator");
    fx.install_pkg("developer");
    fx.install_pkg("code-reviewer");
    fx.install_pkg("task-validator");
    let (wf, dev, rev, val) = ("wf-1", "st-dev", "st-rev", "st-val");
    fx.seed_workflow(wf, "feature-dev", isolation, dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 0);
    fx.seed_step(rev, wf, "review", "ag-rev", 1, 0);
    fx.seed_step(val, wf, "validator", "ag-val", 2, 0);
    fx.seed_transition_step(dev, rev);
    fx.seed_transition_step(rev, dev);
    fx.seed_transition_status(val, "human");
    (
        wf.to_string(),
        dev.to_string(),
        rev.to_string(),
        val.to_string(),
    )
}

// ---- stub runner ----------------------------------------------------------

#[derive(Clone, Copy)]
enum WaitMode {
    Normal,
    HookThenErr,
    HookThenIdle,
}

/// The per-prompt callback the stub invokes (records/reacts to each prompt).
type OnPrompt = Box<dyn FnMut(&str, i64) + Send>;

struct StubRunner {
    prompts: Mutex<Vec<String>>,
    turn_tx: SyncSender<()>,
    turn_rx: Mutex<Receiver<()>>,
    on_prompt: Mutex<Option<OnPrompt>>,
    hook: Mutex<Option<Box<dyn Fn() + Send + Sync>>>,
    wait_mode: WaitMode,
    send_err: Mutex<Option<RunnerError>>,
    terminated: AtomicBool,
    closed: AtomicBool,
}

impl StubRunner {
    fn new() -> Arc<StubRunner> {
        let (tx, rx) = sync_channel(8);
        Arc::new(StubRunner {
            prompts: Mutex::new(Vec::new()),
            turn_tx: tx,
            turn_rx: Mutex::new(rx),
            on_prompt: Mutex::new(None),
            hook: Mutex::new(None),
            wait_mode: WaitMode::Normal,
            send_err: Mutex::new(None),
            terminated: AtomicBool::new(false),
            closed: AtomicBool::new(false),
        })
    }
    fn prompt_count(&self) -> usize {
        self.prompts.lock().unwrap().len()
    }
}

impl PiRunner for StubRunner {
    fn pid(&self) -> i32 {
        4242
    }
    fn get_state(&self, _ctx: &Ctx) -> Result<SessionInfo, RunnerError> {
        Ok(SessionInfo::default())
    }
    fn send_prompt(&self, _ctx: &Ctx, m: &str) -> Result<(), RunnerError> {
        if let Some(e) = self.send_err.lock().unwrap().take() {
            return Err(e);
        }
        let attempt = {
            let mut p = self.prompts.lock().unwrap();
            p.push(m.to_string());
            p.len() as i64
        };
        if let Some(cb) = self.on_prompt.lock().unwrap().as_mut() {
            cb(m, attempt);
        }
        let _ = self.turn_tx.try_send(());
        Ok(())
    }
    fn wait_for_agent_end(&self, ctx: &Ctx) -> Result<(), RunnerError> {
        match self.wait_mode {
            WaitMode::Normal => {
                let rx = self.turn_rx.lock().unwrap();
                loop {
                    match rx.recv_timeout(Duration::from_millis(5)) {
                        Ok(()) => return Ok(()),
                        Err(_) => match ctx.done() {
                            Some(autosk_core::ctx::Done::Cancelled) => {
                                return Err(RunnerError::Cancelled)
                            }
                            Some(autosk_core::ctx::Done::DeadlineExceeded) => {
                                return Err(RunnerError::DeadlineExceeded)
                            }
                            None => continue,
                        },
                    }
                }
            }
            WaitMode::HookThenErr => {
                if let Some(h) = self.hook.lock().unwrap().as_ref() {
                    h();
                }
                Err(RunnerError::Io("bufio.Scanner: token too long".into()))
            }
            WaitMode::HookThenIdle => {
                if let Some(h) = self.hook.lock().unwrap().as_ref() {
                    h();
                }
                loop {
                    match ctx.done() {
                        Some(autosk_core::ctx::Done::Cancelled) => {
                            return Err(RunnerError::Cancelled)
                        }
                        Some(autosk_core::ctx::Done::DeadlineExceeded) => {
                            return Err(RunnerError::DeadlineExceeded)
                        }
                        None => std::thread::sleep(Duration::from_millis(2)),
                    }
                }
            }
        }
    }
    fn abort(&self, _ctx: &Ctx) -> Result<(), RunnerError> {
        Ok(())
    }
    fn close_stdin(&self) -> Result<(), RunnerError> {
        self.closed.store(true, Ordering::SeqCst);
        Ok(())
    }
    fn terminate(&self) -> Result<(), RunnerError> {
        self.terminated.store(true, Ordering::SeqCst);
        Ok(())
    }
    fn kill(&self) -> Result<(), RunnerError> {
        Ok(())
    }
    fn wait(&self, _ctx: &Ctx, _grace: Duration) -> (i32, Result<(), RunnerError>) {
        (0, Ok(()))
    }
    fn send_command(&self, _c: Command) -> Result<Receiver<Response>, RunnerError> {
        Err(RunnerError::Io("stub".into()))
    }
    fn is_streaming(&self) -> bool {
        false
    }
}

fn stub_factory(stub: Arc<StubRunner>) -> autosk_core::executor::PiFactory {
    Arc::new(move |_ctx, _opts| Ok(Arc::clone(&stub) as Arc<dyn PiRunner>))
}

// A worktree manager that records calls and returns scripted results.
#[derive(Default)]
struct FakeWt {
    verify_result: Mutex<Option<WorktreeError>>, // None => Ok
    ensure_called: AtomicBool,
    on_terminal_called: AtomicBool,
}
impl WorktreeManager for FakeWt {
    fn ensure(&self, _r: &str, _t: &str, _b: &str) -> Result<WtOutcome, WorktreeError> {
        self.ensure_called.store(true, Ordering::SeqCst);
        Ok(WtOutcome::default())
    }
    fn on_terminal(&self, _r: &str, _t: &str) -> Result<WtOutcome, WorktreeError> {
        self.on_terminal_called.store(true, Ordering::SeqCst);
        Ok(WtOutcome::default())
    }
    fn verify(&self, _r: &str, _t: &str) -> Result<(), WorktreeError> {
        match self.verify_result.lock().unwrap().clone() {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }
}

fn noop_wt() -> Arc<dyn WorktreeManager> {
    Arc::new(autosk_core::worktree::Manager::new())
}

/// A stub whose `wait_for_agent_end` emits the step signal at +50ms and returns
/// `Ok` at +90ms, but honours `ctx.done()` each poll. With a 30ms idle-timeout:
/// attached (no deadline) → reaches the signal + Ok; unattached → the deadline
/// fires at 30ms, BEFORE the signal, so the run fails. This isolates the
/// attach-disarms-idle-timeout behaviour.
struct TimedStub {
    db: Arc<Db>,
    task_id: String,
    target: String,
}
impl PiRunner for TimedStub {
    fn pid(&self) -> i32 {
        7
    }
    fn get_state(&self, _c: &Ctx) -> Result<SessionInfo, RunnerError> {
        Ok(SessionInfo::default())
    }
    fn send_prompt(&self, _c: &Ctx, _m: &str) -> Result<(), RunnerError> {
        Ok(())
    }
    fn wait_for_agent_end(&self, ctx: &Ctx) -> Result<(), RunnerError> {
        let start = Instant::now();
        let mut emitted = false;
        loop {
            if let Some(d) = ctx.done() {
                return Err(match d {
                    autosk_core::ctx::Done::Cancelled => RunnerError::Cancelled,
                    autosk_core::ctx::Done::DeadlineExceeded => RunnerError::DeadlineExceeded,
                });
            }
            let el = start.elapsed();
            if !emitted && el >= Duration::from_millis(50) {
                self.db.signal_emit(&self.task_id, &self.target).unwrap();
                emitted = true;
            }
            if el >= Duration::from_millis(90) {
                return Ok(());
            }
            std::thread::sleep(Duration::from_millis(3));
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

fn timed_factory(stub: Arc<TimedStub>) -> autosk_core::executor::PiFactory {
    Arc::new(move |_c, _o| Ok(Arc::clone(&stub) as Arc<dyn PiRunner>))
}

#[test]
fn attach_disarms_idle_timeout() {
    let fx = new_fixture();
    let (wf, dev, rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let att = Arc::new(Attachments::new());
    let _guard = att.acquire(&job); // attach BEFORE the run starts

    let stub = Arc::new(TimedStub {
        db: Arc::clone(&fx.db),
        task_id: task.clone(),
        target: "review".into(),
    });
    let mut cfg = fx.cfg();
    cfg.idle_timeout = Duration::from_millis(30); // would fire at 30ms < signal@50ms
    let exec = Executor::new(
        fx.deps_attach(noop_wt(), Arc::clone(&att)),
        timed_factory(stub),
        cfg,
    );
    exec.run(&Ctx::background(), &job)
        .expect("attached run must not time out");

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "done", "attach disarmed the idle-timeout");
    assert_eq!(fx.db.task_get_row(&task).unwrap().current_step_id, rev);
}

#[test]
fn unattached_idle_timeout_fails_before_signal() {
    let fx = new_fixture();
    let (wf, dev, _rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 0);

    let att = Arc::new(Attachments::new()); // NOT attached
    let stub = Arc::new(TimedStub {
        db: Arc::clone(&fx.db),
        task_id: task.clone(),
        target: "review".into(),
    });
    let mut cfg = fx.cfg();
    cfg.idle_timeout = Duration::from_millis(30);
    let exec = Executor::new(fx.deps_attach(noop_wt(), att), timed_factory(stub), cfg);
    let _ = exec.run(&Ctx::background(), &job).unwrap_err();
    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(
        run.status, "failed",
        "unattached run times out before the signal lands"
    );
}

// ---- tests ----------------------------------------------------------------

#[test]
fn advances_on_valid_signal() {
    let fx = new_fixture();
    let (wf, dev, rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, attempt| {
            if attempt == 1 {
                db.signal_emit(&t, "review").unwrap();
            }
        }));
    }
    let exec = Executor::new(
        fx.deps(noop_wt()),
        stub_factory(Arc::clone(&stub)),
        fx.cfg(),
    );
    exec.run(&Ctx::background(), &job).unwrap();

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "done");
    assert!(run.transition_id.is_some());
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "work");
    assert_eq!(tk.current_step_id, rev);
}

#[test]
fn task_status_human_preserves_step() {
    let fx = new_fixture();
    let (wf, _dev, _rev, val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &val, serde_json::json!({}));
    let job = fx.create_run(&task, &val, 2);

    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, a| {
            if a == 1 {
                db.signal_emit(&t, "human").unwrap();
            }
        }));
    }
    let exec = Executor::new(
        fx.deps(noop_wt()),
        stub_factory(Arc::clone(&stub)),
        fx.cfg(),
    );
    exec.run(&Ctx::background(), &job).unwrap();

    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "human");
    assert_eq!(tk.current_step_id, val, "step preserved on human park");
}

#[test]
fn kickback_then_fail_parks_task() {
    let fx = new_fixture();
    let (wf, dev, _rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let stub = StubRunner::new(); // never emits a signal
    let exec = Executor::new(
        fx.deps(noop_wt()),
        stub_factory(Arc::clone(&stub)),
        fx.cfg(),
    );
    let err = exec.run(&Ctx::background(), &job).unwrap_err();
    assert!(
        err.is_agent_did_not_emit(),
        "want AgentDidNotEmit, got {err}"
    );

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "failed");
    assert_eq!(run.error, "agent_did_not_emit_transition");
    // max_corrections=2 → initial + 2 kickbacks = 3 prompts.
    assert_eq!(stub.prompt_count(), 3);
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "human", "task parked");
}

#[test]
fn terminal_done_clears_step() {
    let fx = new_fixture();
    fx.seed_agent("ag-dev", "developer");
    fx.install_pkg("developer");
    let (wf, dev) = ("wf-d", "st-only");
    fx.seed_workflow(wf, "single-dev", "none", dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 0);
    fx.seed_transition_status(dev, "done");
    let task = fx.create_task(wf, dev, serde_json::json!({}));
    let job = fx.create_run(&task, dev, 2);

    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, a| {
            if a == 1 {
                db.signal_emit(&t, "done").unwrap();
            }
        }));
    }
    let exec = Executor::new(
        fx.deps(noop_wt()),
        stub_factory(Arc::clone(&stub)),
        fx.cfg(),
    );
    exec.run(&Ctx::background(), &job).unwrap();

    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "done");
    assert_eq!(tk.current_step_id, "");
    assert_eq!(tk.workflow_id, wf, "workflow_id preserved for audit");
}

#[test]
fn missing_agent_config_fails_and_parks() {
    let fx = new_fixture();
    let (wf, dev, _rev, _val) = seed_basic_wf(&fx, "none");
    fx.uninstall_pkg("developer");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let stub = StubRunner::new();
    let exec = Executor::new(fx.deps(noop_wt()), stub_factory(stub), fx.cfg());
    let err = exec.run(&Ctx::background(), &job).unwrap_err();
    assert!(err.to_string().contains("agent_config_invalid"), "{err}");

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "failed");
    assert!(run.error.contains("agent_config_invalid"));
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "human");
    assert_eq!(tk.current_step_id, dev, "step preserved so resume works");
}

#[test]
fn cap_exceeded_parks_on_target_step() {
    let fx = new_fixture();
    fx.seed_agent("ag-dev", "developer");
    fx.seed_agent("ag-rev", "code-reviewer");
    fx.install_pkg("developer");
    fx.install_pkg("code-reviewer");
    let (wf, dev, rev) = ("wf-c", "st-dev", "st-rev");
    fx.seed_workflow(wf, "capped", "none", dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 5);
    fx.seed_step(rev, wf, "review", "ag-rev", 1, 1); // cap review at 1
    fx.seed_transition_step(dev, rev);
    fx.seed_transition_step(rev, dev);
    // step_visits[review] already at the cap.
    let task = fx.create_task(wf, dev, serde_json::json!({"step_visits": {rev: 1}}));
    let job = fx.create_run(&task, dev, 0);

    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, a| {
            if a == 1 {
                db.signal_emit(&t, "review").unwrap();
            }
        }));
    }
    let exec = Executor::new(fx.deps(noop_wt()), stub_factory(stub), fx.cfg());
    let err = exec.run(&Ctx::background(), &job).unwrap_err();
    assert!(err.is_max_visits(), "want MaxVisits, got {err}");

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "failed");
    assert!(
        run.error.starts_with("step_max_visits_exceeded:"),
        "run.error={}",
        run.error
    );
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "human");
    assert_eq!(tk.current_step_id, rev, "parked on TARGET step (review)");
    // The cap-side increment must NOT have landed.
    let sv = tk.metadata["step_visits"].as_object().unwrap();
    assert_eq!(sv[rev].as_i64(), Some(1), "review counter unchanged");
}

#[test]
fn advance_bumps_visit_counter() {
    let fx = new_fixture();
    fx.seed_agent("ag-dev", "developer");
    fx.seed_agent("ag-rev", "code-reviewer");
    fx.install_pkg("developer");
    fx.install_pkg("code-reviewer");
    let (wf, dev, rev) = ("wf-cm", "st-dev", "st-rev");
    fx.seed_workflow(wf, "countme", "none", dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 5);
    fx.seed_step(rev, wf, "review", "ag-rev", 1, 5);
    fx.seed_transition_step(dev, rev);
    fx.seed_transition_status(rev, "done");
    let task = fx.create_task(wf, dev, serde_json::json!({}));
    let job = fx.create_run(&task, dev, 2);

    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, a| {
            if a == 1 {
                db.signal_emit(&t, "review").unwrap();
            }
        }));
    }
    let exec = Executor::new(fx.deps(noop_wt()), stub_factory(stub), fx.cfg());
    exec.run(&Ctx::background(), &job).unwrap();

    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.current_step_id, rev);
    let sv = tk.metadata["step_visits"].as_object().unwrap();
    assert_eq!(sv[rev].as_i64(), Some(1));
}

#[test]
fn runner_crash_before_signal_preserves_source_step() {
    let fx = new_fixture();
    let (wf, dev, _rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let stub = StubRunner::new();
    *stub.send_err.lock().unwrap() = Some(RunnerError::Io("boom: pi died".into()));
    let exec = Executor::new(fx.deps(noop_wt()), stub_factory(stub), fx.cfg());
    let _ = exec.run(&Ctx::background(), &job).unwrap_err();

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "failed");
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "human");
    assert_eq!(tk.current_step_id, dev, "parked on SOURCE step");
}

#[test]
fn wait_error_after_signal_honours_signal() {
    let fx = new_fixture();
    let (wf, dev, rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let mut stub = StubRunner::new();
    Arc::get_mut(&mut stub).unwrap().wait_mode = WaitMode::HookThenErr;
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.hook.lock().unwrap() = Some(Box::new(move || {
            db.signal_emit(&t, "review").unwrap();
        }));
    }
    let exec = Executor::new(
        fx.deps(noop_wt()),
        stub_factory(Arc::clone(&stub)),
        fx.cfg(),
    );
    exec.run(&Ctx::background(), &job)
        .expect("signal honoured despite wait error");

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "done");
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.current_step_id, rev);
}

#[test]
fn idle_timeout_with_signal_honours_signal() {
    let fx = new_fixture();
    let (wf, dev, rev, _val) = seed_basic_wf(&fx, "none");
    let task = fx.create_task(&wf, &dev, serde_json::json!({}));
    let job = fx.create_run(&task, &dev, 2);

    let mut stub = StubRunner::new();
    Arc::get_mut(&mut stub).unwrap().wait_mode = WaitMode::HookThenIdle;
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.hook.lock().unwrap() = Some(Box::new(move || {
            db.signal_emit(&t, "review").unwrap();
        }));
    }
    let mut cfg = fx.cfg();
    cfg.idle_timeout = Duration::from_millis(20);
    let exec = Executor::new(fx.deps(noop_wt()), stub_factory(Arc::clone(&stub)), cfg);
    exec.run(&Ctx::background(), &job)
        .expect("signal honoured despite idle timeout");

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "done");
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.current_step_id, rev);
}

#[test]
fn worktree_alloc_terminal_cleanup() {
    let fx = new_fixture();
    fx.seed_agent("ag-dev", "developer");
    fx.install_pkg("developer");
    let (wf, dev) = ("wf-w", "st-only");
    fx.seed_workflow(wf, "iso-dev", "worktree", dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 0);
    fx.seed_transition_status(dev, "done");
    let task = fx.create_task(wf, dev, serde_json::json!({}));
    let job = fx.create_run(&task, dev, 2);

    let wt = Arc::new(FakeWt::default()); // verify Ok
    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, a| {
            if a == 1 {
                db.signal_emit(&t, "done").unwrap();
            }
        }));
    }
    let exec = Executor::new(fx.deps(wt.clone()), stub_factory(stub), fx.cfg());
    exec.run(&Ctx::background(), &job).unwrap();

    assert!(
        wt.on_terminal_called.load(Ordering::SeqCst),
        "worktree reaped on terminal done"
    );
    assert_eq!(fx.db.task_get_row(&task).unwrap().status, "done");
}

#[test]
fn worktree_missing_auto_recovers() {
    let fx = new_fixture();
    fx.seed_agent("ag-dev", "developer");
    fx.install_pkg("developer");
    let (wf, dev, rev) = ("wf-wm", "st-dev", "st-rev");
    fx.seed_agent("ag-rev", "code-reviewer");
    fx.install_pkg("code-reviewer");
    fx.seed_workflow(wf, "iso2", "worktree", dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 0);
    fx.seed_step(rev, wf, "review", "ag-rev", 1, 0);
    fx.seed_transition_step(dev, rev);
    let task = fx.create_task(wf, dev, serde_json::json!({}));
    let job = fx.create_run(&task, dev, 2);

    let wt = Arc::new(FakeWt::default());
    *wt.verify_result.lock().unwrap() = Some(WorktreeError::WorktreeMissing("gone".into()));
    let stub = StubRunner::new();
    {
        let db = Arc::clone(&fx.db);
        let t = task.clone();
        *stub.on_prompt.lock().unwrap() = Some(Box::new(move |_m, a| {
            if a == 1 {
                db.signal_emit(&t, "review").unwrap();
            }
        }));
    }
    let exec = Executor::new(fx.deps(wt.clone()), stub_factory(stub), fx.cfg());
    exec.run(&Ctx::background(), &job).unwrap();

    assert!(
        wt.ensure_called.load(Ordering::SeqCst),
        "missing worktree re-allocated via ensure"
    );
    assert_eq!(fx.db.task_get_row(&task).unwrap().current_step_id, rev);
}

#[test]
fn worktree_stranded_parks() {
    let fx = new_fixture();
    fx.seed_agent("ag-dev", "developer");
    fx.install_pkg("developer");
    let (wf, dev) = ("wf-ws", "st-only");
    fx.seed_workflow(wf, "iso3", "worktree", dev);
    fx.seed_step(dev, wf, "dev", "ag-dev", 0, 0);
    fx.seed_transition_status(dev, "done");
    let task = fx.create_task(wf, dev, serde_json::json!({}));
    let job = fx.create_run(&task, dev, 2);

    let wt = Arc::new(FakeWt::default());
    *wt.verify_result.lock().unwrap() = Some(WorktreeError::WorktreeStranded("moved".into()));
    let stub = StubRunner::new();
    let exec = Executor::new(fx.deps(wt.clone()), stub_factory(stub), fx.cfg());
    let err = exec.run(&Ctx::background(), &job).unwrap_err();
    assert!(err.to_string().contains("worktree_stranded"), "{err}");

    let run = fx.db.run_get(&job).unwrap();
    assert_eq!(run.status, "failed");
    assert!(run.error.contains("worktree_stranded"));
    let tk = fx.db.task_get_row(&task).unwrap();
    assert_eq!(tk.status, "human");
    assert_eq!(tk.current_step_id, dev, "stranded park keeps source step");
    assert!(
        !wt.ensure_called.load(Ordering::SeqCst),
        "stranded is NOT auto-recovered"
    );
}
