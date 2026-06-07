//! The per-run executor — the Rust port of `internal/daemon/executor`.
//!
//! Drives one workflow-step run end to end: mark running → resolve agent
//! config → render prompt/seed → spawn the runner → turn loop
//! (`wait_for_agent_end` + consume `step_signals`, kickback ≤ `max_corrections`)
//! → advance the task atomically → mark done; with the full failure/cancel
//! ladder, worktree isolation, attach-aware idle-timeout disarm, and the
//! signal-honoured-after-reader-error defensive recovery.

use std::sync::Arc;
use std::time::Duration;

use serde::Serialize;

use crate::ctx::Ctx;
use crate::error::Error;
use crate::pi::{self, PiOpts};
use crate::pirunners::{Attachments, Registry as RunnerRegistry};
use crate::pkg::{PackageConfig, Registry as PkgRegistry};
use crate::runner::{PiRunner, RunnerError};
use crate::runstore::Run;
use crate::signals::{Emitted, SignalError};
use crate::store::Db;
use crate::tasks::{Task, TaskPatch, STATUS_CANCEL, STATUS_DONE, STATUS_HUMAN, STATUS_WORK};
use crate::wfengine::{self, AgentParams, Step, WorkflowMeta};
use crate::worktree::{self, WorktreeError, WorktreeManager};

/// A factory that spawns a standard pi runner.
pub type PiFactory =
    Arc<dyn Fn(&Ctx, PiOpts) -> Result<Arc<dyn PiRunner>, RunnerError> + Send + Sync>;
/// A factory that spawns a custom Node runner.
pub type NodeFactory = Arc<
    dyn Fn(&Ctx, crate::agentnode::NodeOpts) -> Result<Arc<dyn PiRunner>, RunnerError>
        + Send
        + Sync,
>;

/// Default pi factory wrapping [`crate::pi::spawn`].
pub fn default_pi_factory() -> PiFactory {
    Arc::new(|ctx, opts| pi::spawn(ctx, opts).map(|r| Arc::new(r) as Arc<dyn PiRunner>))
}

/// Default Node factory wrapping [`crate::agentnode::spawn`].
pub fn default_node_factory() -> NodeFactory {
    Arc::new(|ctx, opts| {
        crate::agentnode::spawn(ctx, opts).map(|r| Arc::new(r) as Arc<dyn PiRunner>)
    })
}

/// Everything the executor reads/writes (mirror of `executor.Deps`).
#[derive(Clone)]
pub struct Deps {
    pub db: Arc<Db>,
    /// Injectable task-write seam (defaults to `db`); tests wrap it to force
    /// failures. Mirror of the Go `TaskStore` interface.
    pub tasks: Arc<dyn wfengine::TaskWriter>,
    pub packages: Arc<PkgRegistry>,
    pub worktree: Arc<dyn WorktreeManager>,
    /// Live runner registry (attach surface); `None` disables registration.
    pub runners: Option<Arc<RunnerRegistry>>,
    /// Attach counter; `None` disables the idle-timeout disarm hook.
    pub attachments: Option<Arc<Attachments>>,
}

/// Executor tuning (mirror of `executor.Config`).
#[derive(Clone)]
pub struct Config {
    pub pi_bin: String,
    /// Parent dir for per-job pi session dirs. Empty → `<root>/.autosk/sessions`.
    pub session_dir_root: String,
    /// Where `.autosk/` lives (cwd for the agent + default session dir).
    pub project_root: String,
    /// Absolute path to the project's `.autosk/db` (threaded as `AUTOSK_DB`
    /// when the workflow opts into worktree isolation).
    pub db_path: String,
    pub grace: Duration,
    pub idle_timeout: Duration,
    /// Caps the background session-info poll. 0 → ~30s.
    pub session_poll_budget: Duration,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            pi_bin: String::new(),
            session_dir_root: String::new(),
            project_root: String::new(),
            db_path: String::new(),
            grace: Duration::from_secs(10),
            idle_timeout: Duration::from_secs(30 * 60),
            session_poll_budget: Duration::ZERO,
        }
    }
}

/// Terminal failure when the agent never emits a `step next` (mirror of
/// `ErrAgentDidNotEmit`).
pub const ERR_AGENT_DID_NOT_EMIT: &str = "agent_did_not_emit_transition";

/// The executor's run outcome error.
#[derive(Debug)]
pub enum ExecError {
    /// Out of correction attempts without a transition.
    AgentDidNotEmit,
    /// A `max_visits` cap fired in advanceTask (carries the cap error).
    MaxVisits(Error),
    /// A runner-level failure (pi crash, timeout, rejected prompt).
    Runner(RunnerError),
    /// A core/store failure.
    Core(Error),
    /// A wrapped failure with a fixed message (e.g. "advance task: …").
    Msg(String),
}

impl ExecError {
    fn is_cancelled(&self) -> bool {
        matches!(self, ExecError::Runner(e) if e.is_cancelled())
    }
    /// True for the [`ExecError::AgentDidNotEmit`] terminal failure.
    pub fn is_agent_did_not_emit(&self) -> bool {
        matches!(self, ExecError::AgentDidNotEmit)
    }
    /// True when this run failed on a `max_visits` cap.
    pub fn is_max_visits(&self) -> bool {
        matches!(self, ExecError::MaxVisits(_))
    }
}

impl std::fmt::Display for ExecError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ExecError::AgentDidNotEmit => write!(f, "{ERR_AGENT_DID_NOT_EMIT}"),
            ExecError::MaxVisits(e) => write!(f, "{e}"),
            ExecError::Runner(e) => write!(f, "{e}"),
            ExecError::Core(e) => write!(f, "{e}"),
            ExecError::Msg(s) => write!(f, "{s}"),
        }
    }
}

/// Wraps an advanceTask error with the id of the TARGET step the run was
/// entering, so [`Executor::park_task_on_failure`] parks on the target (the
/// run's intent) rather than the source. Mirror of `advanceTargetError`.
struct AdvanceError {
    target_step_id: Option<String>,
    err: Error,
}

const WORKTREE_CLEANUP_TIMEOUT: Duration = Duration::from_secs(30);

/// The executor (mirror of `executor.Executor`).
pub struct Executor {
    deps: Deps,
    factory: PiFactory,
    node_factory: NodeFactory,
    cfg: Config,
}

impl Executor {
    /// Builds an executor with the default Node factory.
    pub fn new(deps: Deps, factory: PiFactory, mut cfg: Config) -> Executor {
        if cfg.grace.is_zero() {
            cfg.grace = Duration::from_secs(10);
        }
        if cfg.idle_timeout.is_zero() {
            cfg.idle_timeout = Duration::from_secs(30 * 60);
        }
        Executor {
            deps,
            factory,
            node_factory: default_node_factory(),
            cfg,
        }
    }

    /// Overrides the Node bootstrapper factory (tests inject a stub).
    pub fn with_node_factory(mut self, nf: NodeFactory) -> Executor {
        self.node_factory = nf;
        self
    }

    /// Runs the job to a terminal state. The scheduler entry point.
    pub fn run(&self, ctx: &Ctx, job_id: &str) -> Result<(), ExecError> {
        let db = &self.deps.db;

        let _run0 = db.run_get(job_id).map_err(ExecError::Core)?;
        // 1. queued → running.
        let mut run = db.run_mark_running(job_id, 0).map_err(ExecError::Core)?;

        // 2. Resolve step → workflow → task → agent config.
        let step = match db.wf_find_step_by_id(&run.step_id) {
            Ok(s) => s,
            Err(e) => {
                return Err(self.fail_terminal(
                    job_id,
                    None,
                    &format!("find step {}: {e}", run.step_id),
                ))
            }
        };
        let wf = match db.wf_get_by_id(&step.workflow_id) {
            Ok(w) => w,
            Err(e) => {
                return Err(self.fail_terminal(
                    job_id,
                    None,
                    &format!("get workflow {}: {e}", step.workflow_id),
                ))
            }
        };
        let tk = match self.deps.tasks.get_task(&run.task_id) {
            Ok(t) => t,
            Err(e) => {
                return Err(self.fail_terminal(
                    job_id,
                    None,
                    &format!("get task {}: {e}", run.task_id),
                ))
            }
        };
        let mut agent_cfg = match self.deps.packages.resolve(&step.agent_name) {
            Ok(c) => c,
            Err(e) => {
                return Err(self.fail_terminal(job_id, None, &format!("agent_config_invalid: {e}")))
            }
        };
        if let Err(e) = apply_agent_param_overrides(&mut agent_cfg, step.agent_params.as_ref()) {
            return Err(self.fail_terminal(job_id, None, &format!("agent_config_invalid: {e}")));
        }

        // 3. Render prompt + (custom runners) a JSON seed.
        let comment_lines = db
            .comments_render_for_prompt(&run.task_id)
            .unwrap_or_default();
        let prompt = render_prompt(&tk, &wf, &step, &agent_cfg, &comment_lines);

        // 4. Spawn the right runner (+ worktree isolation).
        let session_dir = self.session_dir_for(&run);
        if let Err(e) = std::fs::create_dir_all(&session_dir) {
            return Err(self.fail_terminal(job_id, None, &format!("session dir: {e}")));
        }

        let mut cwd = self.cfg.project_root.clone();
        let mut run_env: Vec<(String, String)> = Vec::new();
        if wf.isolation == "worktree" {
            self.prepare_worktree(job_id, &tk, &mut cwd, &mut run_env)?;
        }

        let (runner, initial_msg): (Arc<dyn PiRunner>, String) = if !agent_cfg.runner.is_empty() {
            // Custom Node runner: stdin payload is a JSON RunContextSeed, not
            // the rendered prompt. Single-shot ⇒ force max_corrections=0.
            let bootstrap = self.deps.packages.runtime_bootstrap_path();
            let seed =
                match render_seed_json(&tk, &wf, &step, &agent_cfg, &comment_lines, &run.job_id) {
                    Ok(s) => s,
                    Err(e) => {
                        return Err(self.fail_terminal(job_id, None, &format!("render seed: {e}")))
                    }
                };
            let r = match (self.node_factory)(
                ctx,
                crate::agentnode::NodeOpts {
                    bootstrap_path: bootstrap,
                    package_name: agent_cfg.name.clone(),
                    runner_path: agent_cfg.runner.clone(),
                    cwd: cwd.clone(),
                    env: run_env.clone(),
                    use_tsx_loader: true,
                    ..Default::default()
                },
            ) {
                Ok(r) => r,
                Err(e) => {
                    return Err(self.fail_terminal(job_id, None, &format!("spawn runner: {e}")))
                }
            };
            run.max_corrections = 0;
            (r, seed)
        } else {
            let extra_args = build_pi_extra_args(&agent_cfg);
            let r = match (self.factory)(
                ctx,
                PiOpts {
                    pi_bin: self.cfg.pi_bin.clone(),
                    cwd: cwd.clone(),
                    env: run_env.clone(),
                    model: agent_cfg.model.clone(),
                    thinking: agent_cfg.thinking.clone(),
                    session_dir: session_dir.clone(),
                    extra_args,
                },
            ) {
                Ok(r) => r,
                Err(e) => return Err(self.fail_terminal(job_id, None, &format!("spawn pi: {e}"))),
            };
            (r, prompt)
        };

        // Register for the attach surface (pi runners only).
        let registered = if let Some(reg) = &self.deps.runners {
            if runner.supports_attach() {
                reg.register(job_id, Arc::clone(&runner));
                true
            } else {
                false
            }
        } else {
            false
        };
        // RAII guard so we unregister + reap on every exit path.
        let _cleanup = RunGuard {
            runner: Arc::clone(&runner),
            runners: if registered {
                self.deps.runners.clone()
            } else {
                None
            },
            job_id: job_id.to_string(),
            grace: self.cfg.grace,
        };

        if runner.pid() > 0 {
            let _ = db.run_set_pid(job_id, runner.pid() as i64);
        }

        // 5. Initial prompt / JSON seed.
        if let Err(e) = runner.send_prompt(ctx, &initial_msg) {
            return Err(self.handle_run_error(ctx, job_id, &*runner, ExecError::Runner(e)));
        }

        // 5a. Record pi's session info in the background (pi runners only).
        if agent_cfg.runner.is_empty() {
            self.spawn_session_poll(ctx, Arc::clone(&runner), job_id);
        }

        // 6. Turn loop.
        let signaled = self.turn_loop(ctx, &*runner, &mut run, &step, job_id)?;

        // 7. Clean shutdown of the runner.
        let (exit, werr) = self.shutdown(&*runner);
        if let Some(e) = werr {
            if !e.is_cancelled() {
                return Err(self.fail_terminal(job_id, Some(exit), &format!("pi exit: {e}")));
            }
        }

        // 8. Advance the task atomically + persist transition_id.
        if let Err(ae) = self.advance_task(&run.task_id, &signaled) {
            if ae.err.is_max_visits_exceeded() {
                let msg = ae.err.to_string();
                self.mark_failed_and_park(job_id, Some(exit), &msg, ae.target_step_id.clone());
                return Err(ExecError::MaxVisits(ae.err));
            }
            let msg = format!("advance task: {}", ae.err);
            self.mark_failed_and_park(job_id, Some(exit), &msg, ae.target_step_id);
            return Err(ExecError::Msg(msg));
        }
        let tid = signaled.transition_id;
        db.run_mark_done(job_id, exit, Some(tid))
            .map_err(ExecError::Core)?;

        // Reap the per-task worktree on terminal transitions for isolated wfs.
        if wf.isolation == "worktree" && is_terminal_status(&signaled.task_status) {
            self.cleanup_worktree_best_effort(&run.task_id, &signaled.task_status);
        }
        Ok(())
    }

    /// The turn loop: `wait_for_agent_end` → consume signal → kickback.
    fn turn_loop(
        &self,
        ctx: &Ctx,
        runner: &dyn PiRunner,
        run: &mut Run,
        step: &Step,
        job_id: &str,
    ) -> Result<Emitted, ExecError> {
        let db = &self.deps.db;
        loop {
            let attached = self
                .deps
                .attachments
                .as_ref()
                .map(|a| a.attached(job_id))
                .unwrap_or(false);
            let turn_ctx = if attached {
                ctx.child()
            } else {
                ctx.with_timeout(self.cfg.idle_timeout)
            };
            let werr = runner.wait_for_agent_end(&turn_ctx);
            if let Err(e) = werr {
                // Defensive: honour a recorded signal even if the wait errored
                // (reader crash / idle-timeout-after-signal), but never on
                // cancellation.
                if !ctx.is_cancelled() && !e.is_cancelled() {
                    if let Ok(sig) = db.signal_for_run(job_id) {
                        eprintln!(
                            "executor: wait_for_agent_end errored but step_signal already recorded; honoring signal (job={job_id} err={e})"
                        );
                        return Ok(sig);
                    }
                }
                return Err(self.handle_run_error(ctx, job_id, runner, ExecError::Runner(e)));
            }
            match db.signal_for_run(job_id) {
                Ok(sig) => return Ok(sig),
                Err(SignalError::NoActiveRun) => {
                    // No signal at agent_end.
                }
                Err(other) => {
                    return Err(self.handle_run_error(
                        ctx,
                        job_id,
                        runner,
                        ExecError::Core(core_of(other)),
                    ));
                }
            }
            // While attached, a missing signal is not a closure miss.
            if self
                .deps
                .attachments
                .as_ref()
                .map(|a| a.attached(job_id))
                .unwrap_or(false)
            {
                continue;
            }
            // Invalid closure: kick back if budget remains, else fail.
            if run.corrections_used >= run.max_corrections {
                let _ = runner.abort(ctx);
                return Err(self.fail_terminal(job_id, None, ERR_AGENT_DID_NOT_EMIT));
            }
            let used = match db.run_inc_corrections(job_id) {
                Ok(u) => u,
                Err(e) => {
                    return Err(self.handle_run_error(ctx, job_id, runner, ExecError::Core(e)));
                }
            };
            run.corrections_used = used;
            let msg = corrective_message(&run.task_id, step, used, run.max_corrections);
            if let Err(e) = runner.send_prompt(ctx, &msg) {
                return Err(self.handle_run_error(ctx, job_id, runner, ExecError::Runner(e)));
            }
        }
    }

    /// Worktree pre-flight for isolated workflows (mirror of the isolation
    /// block in `Run`). On success sets `cwd`/`run_env`; on failure returns the
    /// already-persisted terminal result.
    fn prepare_worktree(
        &self,
        job_id: &str,
        tk: &Task,
        cwd: &mut String,
        run_env: &mut Vec<(String, String)>,
    ) -> Result<(), ExecError> {
        let path = match worktree::path_for(&self.cfg.project_root, &tk.id) {
            Ok(p) => p,
            Err(e) => return Err(self.fail_terminal(job_id, None, &format!("worktree path: {e}"))),
        };
        match self.deps.worktree.verify(&self.cfg.project_root, &tk.id) {
            Ok(()) => {}
            Err(WorktreeError::WorktreeMissing(_)) => {
                // Auto-recovery: re-allocate the dir on the existing branch.
                match self
                    .deps
                    .worktree
                    .ensure(&self.cfg.project_root, &tk.id, "")
                {
                    Ok(res) => {
                        eprintln!(
                            "executor: re-allocated missing worktree (task={} job={job_id} path={} branch={})",
                            tk.id, res.path, res.branch
                        );
                    }
                    Err(e) => {
                        return Err(self.fail_terminal(
                            job_id,
                            None,
                            &format!("worktree_missing: re-allocate failed: {e}"),
                        ))
                    }
                }
            }
            Err(e) => {
                return Err(self.fail_terminal(job_id, None, &format!("worktree_stranded: {e}")))
            }
        }
        *cwd = path;
        let db_path = if self.cfg.db_path.is_empty() {
            format!("{}/.autosk/db", self.cfg.project_root)
        } else {
            self.cfg.db_path.clone()
        };
        let mut env: Vec<(String, String)> = std::env::vars().collect();
        env.push(("AUTOSK_DB".to_string(), db_path));
        *run_env = env;
        Ok(())
    }

    /// advanceTask: applies the transition's effect to the task.
    fn advance_task(&self, task_id: &str, sig: &Emitted) -> Result<(), AdvanceError> {
        let tasks = &*self.deps.tasks;
        if !sig.next_step_name.is_empty() {
            let tk = tasks.get_task(task_id).map_err(|e| AdvanceError {
                target_step_id: None,
                err: e,
            })?;
            if tk.workflow_id.is_empty() {
                return Err(AdvanceError {
                    target_step_id: None,
                    err: Error::Migration(format!("task {task_id} has no workflow_id")),
                });
            }
            let st = self
                .deps
                .db
                .wf_find_step_by_name(&tk.workflow_id, &sig.next_step_name)
                .map_err(|e| AdvanceError {
                    target_step_id: None,
                    err: Error::Migration(format!("find next step {:?}: {e}", sig.next_step_name)),
                })?;
            let target = st.id.clone();
            wfengine::enter_step(tasks, task_id, &st, None).map_err(|e| AdvanceError {
                target_step_id: Some(target),
                err: e,
            })?;
            Ok(())
        } else {
            let status = match sig.task_status.as_str() {
                "human" => STATUS_HUMAN,
                "done" => STATUS_DONE,
                "cancel" => STATUS_CANCEL,
                _ => {
                    return Err(AdvanceError {
                        target_step_id: None,
                        err: Error::Migration(format!(
                            "invalid transition signal: {:?}",
                            sig.task_status
                        )),
                    })
                }
            };
            let mut patch = TaskPatch {
                status: Some(status.to_string()),
                ..Default::default()
            };
            if status != STATUS_HUMAN {
                patch.current_step_id = Some(String::new());
            }
            tasks
                .update_task(task_id, &patch)
                .map_err(|e| AdvanceError {
                    target_step_id: None,
                    err: e,
                })?;
            Ok(())
        }
    }

    fn handle_run_error(
        &self,
        ctx: &Ctx,
        job_id: &str,
        runner: &dyn PiRunner,
        cause: ExecError,
    ) -> ExecError {
        let bg = Ctx::background();
        if cause.is_cancelled() || ctx.is_cancelled() {
            let _ = runner.abort(&bg);
            let _ = runner.close_stdin();
            let _ = runner.terminate();
            let (mut exit, _) = runner.wait(&bg, self.cfg.grace);
            if exit < 0 && runner.pid() > 0 {
                let _ = runner.kill();
                let (e2, _) = runner.wait(&bg, self.cfg.grace);
                exit = e2;
            }
            let _ = self.deps.db.run_mark_cancelled(job_id, Some(exit as i64));
            // Cancellation does NOT park the task.
            return cause;
        }
        let (exit, _) = self.shutdown(runner);
        let msg = cause.to_string();
        self.mark_failed_and_park(job_id, Some(exit), &msg, None);
        cause
    }

    /// shutdown: CloseStdin → Wait(grace) → SIGTERM → Wait → SIGKILL → Wait.
    fn shutdown(&self, runner: &dyn PiRunner) -> (i64, Option<RunnerError>) {
        let bg = Ctx::background();
        let _ = runner.close_stdin();
        let (mut exit, res) = runner.wait(&bg, self.cfg.grace);
        let mut err = res.err();
        if matches!(&err, Some(e) if e.is_wait_timeout()) {
            let _ = runner.terminate();
            let (e2, r2) = runner.wait(&bg, self.cfg.grace);
            exit = e2;
            err = r2.err();
            if matches!(&err, Some(e) if e.is_wait_timeout()) {
                let _ = runner.kill();
                let (e3, r3) = runner.wait(&bg, self.cfg.grace);
                exit = e3;
                err = r3.err();
            }
        }
        (exit as i64, err)
    }

    fn fail_terminal(&self, job_id: &str, exit: Option<i64>, cause: &str) -> ExecError {
        self.mark_failed_and_park(job_id, exit, cause, None);
        if cause == ERR_AGENT_DID_NOT_EMIT {
            ExecError::AgentDidNotEmit
        } else {
            ExecError::Msg(cause.to_string())
        }
    }

    fn mark_failed_and_park(
        &self,
        job_id: &str,
        exit: Option<i64>,
        cause: &str,
        target_step_id: Option<String>,
    ) {
        let _ = self.deps.db.run_mark_failed(job_id, exit, cause);
        self.park_task_on_failure(job_id, target_step_id);
    }

    /// Parks the run's task into `human` so the poller stops re-picking it.
    /// Only parks a task still in `work` (never clobbers an operator's
    /// terminal status). Mirror of `parkTaskOnFailure`.
    fn park_task_on_failure(&self, job_id: &str, target_step_id: Option<String>) {
        let Ok(run) = self.deps.db.run_get(job_id) else {
            return;
        };
        if run.task_id.is_empty() {
            return;
        }
        let Ok(tk) = self.deps.tasks.get_task(&run.task_id) else {
            return;
        };
        if tk.status != STATUS_WORK {
            return;
        }
        let mut patch = TaskPatch {
            status: Some(STATUS_HUMAN.to_string()),
            ..Default::default()
        };
        if let Some(t) = target_step_id {
            if !t.is_empty() {
                patch.current_step_id = Some(t);
            }
        }
        let _ = self.deps.tasks.update_task(&run.task_id, &patch);
    }

    fn cleanup_worktree_best_effort(&self, task_id: &str, _status: &str) {
        // Bounded by a hard timeout via a watchdog thread so a hung
        // `git worktree remove` can't pin the worker slot.
        let wt = Arc::clone(&self.deps.worktree);
        let root = self.cfg.project_root.clone();
        let tid = task_id.to_string();
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            let res = wt.on_terminal(&root, &tid);
            let _ = tx.send(res);
        });
        match rx.recv_timeout(WORKTREE_CLEANUP_TIMEOUT) {
            Ok(Ok(_)) => {}
            Ok(Err(e)) => eprintln!("executor: worktree cleanup failed (task={task_id}): {e}"),
            Err(_) => eprintln!("executor: worktree cleanup timed out (task={task_id})"),
        }
    }

    fn session_dir_for(&self, run: &Run) -> String {
        let root = if self.cfg.session_dir_root.is_empty() {
            format!("{}/.autosk/sessions", self.cfg.project_root)
        } else {
            self.cfg.session_dir_root.clone()
        };
        format!("{root}/{}", run.job_id)
    }

    fn spawn_session_poll(&self, ctx: &Ctx, runner: Arc<dyn PiRunner>, job_id: &str) {
        let db = Arc::clone(&self.deps.db);
        let job = job_id.to_string();
        let ctx = ctx.clone();
        let budget = if self.cfg.session_poll_budget.is_zero() {
            Duration::from_secs(30)
        } else {
            self.cfg.session_poll_budget
        };
        std::thread::spawn(move || poll_session_info(&ctx, &*runner, &db, &job, budget));
    }
}

/// RAII guard: unregister the runner + best-effort reap on every exit path.
struct RunGuard {
    runner: Arc<dyn PiRunner>,
    runners: Option<Arc<RunnerRegistry>>,
    job_id: String,
    grace: Duration,
}

impl Drop for RunGuard {
    fn drop(&mut self) {
        if let Some(reg) = &self.runners {
            reg.unregister(&self.job_id);
        }
        let runner = Arc::clone(&self.runner);
        let grace = self.grace;
        std::thread::spawn(move || {
            let _ = runner.close_stdin();
            let bg = Ctx::background();
            let _ = runner.wait(&bg, grace);
        });
    }
}

fn is_terminal_status(status: &str) -> bool {
    status == "done" || status == "cancel"
}

fn core_of(e: SignalError) -> Error {
    match e {
        SignalError::Core(c) => c,
        other => Error::Migration(other.to_string()),
    }
}

/// pollSessionInfo: retries get_state until SessionFile populates, then writes
/// it through once. Mirror of `pollSessionInfo`.
fn poll_session_info(ctx: &Ctx, runner: &dyn PiRunner, db: &Db, job_id: &str, budget: Duration) {
    let deadline = std::time::Instant::now() + budget;
    let mut delay = Duration::from_millis(100);
    let max_delay = Duration::from_secs(5);
    let per_attempt = Duration::from_secs(2);
    loop {
        if ctx.is_cancelled() || std::time::Instant::now() >= deadline {
            return;
        }
        let attempt_ctx = ctx.with_timeout(per_attempt);
        match runner.get_state(&attempt_ctx) {
            Ok(info) if !info.session_file.is_empty() => {
                let _ = db.run_set_pi_session(job_id, &info.session_id, &info.session_file);
                return;
            }
            Ok(_) => {}
            Err(e) => {
                if ctx.is_cancelled() {
                    return;
                }
                if !e.is_deadline_exceeded() {
                    eprintln!("executor: session info poll: get_state failed (job={job_id}): {e}");
                    return;
                }
            }
        }
        let remaining = deadline.saturating_duration_since(std::time::Instant::now());
        if remaining.is_zero() {
            return;
        }
        std::thread::sleep(delay.min(remaining));
        delay = (delay * 2).min(max_delay);
    }
}

// ---- agent-config merge + prompt rendering --------------------------------

/// Merges per-step [`AgentParams`] over a resolved [`PackageConfig`]. Custom
/// (runner) packages reject any override. Mirror of `applyAgentParamOverrides`.
fn apply_agent_param_overrides(
    cfg: &mut PackageConfig,
    p: Option<&AgentParams>,
) -> Result<(), String> {
    let Some(p) = p else {
        return Ok(());
    };
    if p.is_zero() {
        return Ok(());
    }
    if !cfg.runner.is_empty() {
        return Err(format!(
            "step's agent.params cannot override custom-runner package {:?}",
            cfg.name
        ));
    }
    if let Some(m) = &p.model {
        cfg.model = m.clone();
    }
    if let Some(t) = &p.thinking {
        cfg.thinking = t.clone();
    }
    if let Some(fm) = &p.first_message {
        cfg.first_message = fm.clone();
    }
    if let Some(a) = &p.extra_args {
        cfg.extra_args = a.clone();
    }
    if let Some(a) = &p.pi_extensions {
        cfg.pi_extensions = a.clone();
    }
    if let Some(a) = &p.pi_skills {
        cfg.pi_skills = a.clone();
    }
    Ok(())
}

/// Translates a resolved config into the pi CLI flags appended after the
/// daemon-managed ones. Mirror of `buildPiExtraArgs`.
fn build_pi_extra_args(cfg: &PackageConfig) -> Vec<String> {
    let mut out = Vec::new();
    out.extend(cfg.extra_args.iter().cloned());
    for ext in &cfg.pi_extensions {
        out.push("-e".to_string());
        out.push(ext.clone());
    }
    for sk in &cfg.pi_skills {
        out.push("--skill".to_string());
        out.push(sk.clone());
    }
    out
}

/// Builds the user-facing prompt for a step run. Mirror of `RenderPrompt`.
pub fn render_prompt(
    t: &Task,
    wf: &WorkflowMeta,
    step: &Step,
    cfg: &PackageConfig,
    comment_lines: &[String],
) -> String {
    let mut sb = String::new();
    if !cfg.first_message.is_empty() {
        sb.push_str(cfg.first_message.trim_end_matches('\n'));
        sb.push_str("\n\n");
    }
    sb.push_str(&format!(
        "You are agent {:?} on step {:?} of workflow {:?}.\n",
        step.agent_name, step.name, wf.name
    ));
    sb.push_str(&format!("Task: {}\n", t.id));
    if !t.title.is_empty() {
        sb.push_str(&format!("Title: {}\n", t.title));
    }
    if !t.description.is_empty() {
        sb.push_str("\nDescription:\n");
        sb.push_str(&t.description);
        sb.push('\n');
    }
    sb.push_str("\nAvailable transitions (pick exactly one before you stop):\n");
    for tr in &step.transitions {
        if tr.is_task_status() {
            sb.push_str(&format!(
                "  - task_status={} — {}\n",
                tr.task_status, tr.prompt_rule
            ));
        } else {
            sb.push_str(&format!(
                "  - step={} — {}\n",
                tr.next_step_name, tr.prompt_rule
            ));
        }
    }
    sb.push('\n');
    sb.push_str(&format!(
        "When you have decided, call: `autosk step next {} --to <name>` (sibling step name OR done|cancel|human).\n",
        t.id
    ));
    sb.push_str("Do not stop before issuing exactly one such call.\n");
    if !comment_lines.is_empty() {
        sb.push_str("\nComments (oldest first):\n");
        for line in comment_lines {
            sb.push_str("  ");
            sb.push_str(line);
            sb.push('\n');
        }
    } else {
        sb.push_str("\nNo comments on this task yet.\n");
    }
    sb
}

/// The kickback message sent when the agent ends a turn without a transition.
/// Mirror of `CorrectiveMessage`.
pub fn corrective_message(task_id: &str, step: &Step, attempt: i64, max: i64) -> String {
    let mut sb = String::new();
    sb.push_str(&format!(
        "You stopped without recording a transition on task {task_id}.\n"
    ));
    sb.push_str("Before you stop you MUST call `autosk step next` exactly once with one of:\n");
    for tr in &step.transitions {
        if tr.is_task_status() {
            sb.push_str(&format!(
                "  - autosk step next {task_id} --to {}\n",
                tr.task_status
            ));
        } else {
            sb.push_str(&format!(
                "  - autosk step next {task_id} --to {}\n",
                tr.next_step_name
            ));
        }
    }
    sb.push_str(&format!(
        "This is correction attempt {attempt} of {max}. If you ignore it the run will be marked failed.\n"
    ));
    sb
}

// ---- RunContextSeed (custom runners) --------------------------------------

#[derive(Serialize)]
struct RunContextSeed {
    schema_version: i64,
    task: TaskSeed,
    step: StepSeed,
    workflow: WorkflowSeed,
    comments: Vec<CommentSeed>,
    transitions: Vec<TransitionSeed>,
    project_root: String,
    job_id: String,
    agent_name: String,
    agent_version: String,
    agent_install: String,
}

#[derive(Serialize)]
struct TaskSeed {
    id: String,
    title: String,
    description: String,
    status: String,
    priority: i64,
    workflow_id: String,
    current_step_id: String,
    created_at: String,
    updated_at: String,
}

#[derive(Serialize)]
struct StepSeed {
    id: String,
    name: String,
    agent: String,
}

#[derive(Serialize)]
struct WorkflowSeed {
    id: String,
    name: String,
    description: String,
}

#[derive(Serialize)]
struct TransitionSeed {
    kind: String,
    target: String,
    prompt_rule: String,
}

#[derive(Serialize)]
struct CommentSeed {
    line: String,
}

/// Builds the JSON `RunContextSeed` for a custom runner's stdin. Mirror of
/// `RenderSeedJSON` (schema_version = 1, trailing newline).
pub fn render_seed_json(
    t: &Task,
    wf: &WorkflowMeta,
    step: &Step,
    cfg: &PackageConfig,
    comment_lines: &[String],
    job_id: &str,
) -> Result<String, String> {
    let seed = RunContextSeed {
        schema_version: 1,
        task: TaskSeed {
            id: t.id.clone(),
            title: t.title.clone(),
            description: t.description.clone(),
            status: t.status.clone(),
            priority: t.priority,
            workflow_id: t.workflow_id.clone(),
            current_step_id: t.current_step_id.clone(),
            created_at: crate::timefmt::rfc3339_utc(t.created_at),
            updated_at: crate::timefmt::rfc3339_utc(t.updated_at),
        },
        step: StepSeed {
            id: step.id.clone(),
            name: step.name.clone(),
            agent: step.agent_name.clone(),
        },
        workflow: WorkflowSeed {
            id: wf.id.clone(),
            name: wf.name.clone(),
            description: wf.description.clone(),
        },
        comments: comment_lines
            .iter()
            .map(|l| CommentSeed { line: l.clone() })
            .collect(),
        transitions: step
            .transitions
            .iter()
            .map(|tr| {
                if tr.is_task_status() {
                    TransitionSeed {
                        kind: "task_status".into(),
                        target: tr.task_status.clone(),
                        prompt_rule: tr.prompt_rule.clone(),
                    }
                } else {
                    TransitionSeed {
                        kind: "step".into(),
                        target: tr.next_step_name.clone(),
                        prompt_rule: tr.prompt_rule.clone(),
                    }
                }
            })
            .collect(),
        project_root: String::new(),
        job_id: job_id.to_string(),
        agent_name: cfg.name.clone(),
        agent_version: cfg.version.clone(),
        agent_install: cfg.install_dir.clone(),
    };
    let mut s = serde_json::to_string(&seed).map_err(|e| format!("marshal seed: {e}"))?;
    s.push('\n');
    Ok(s)
}
