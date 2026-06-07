//! The daemon's runtime wiring (plan §3, §7): a global job [`Scheduler`]
//! driving per-project workflow runs, a per-project [`Poller`] + [`Compactor`]
//! started on first project open, and the daemon-wide live-runner
//! [`RunnerRegistry`] + [`Attachments`] hubs the attach surface needs.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use autosk_core::compactor::Compactor;
use autosk_core::ctx::Ctx;
use autosk_core::executor::{self, Config as ExecConfig, Deps as ExecDeps, Executor};
use autosk_core::pirunners::{Attachments, Registry as RunnerRegistry};
use autosk_core::pkg::Registry as PkgRegistry;
use autosk_core::poller::Poller;
use autosk_core::projectmgr::{Manager, Project};
use autosk_core::registry::Registry;
use autosk_core::scheduler::{Config as SchedConfig, Job, SchedExecutor, Scheduler};
use autosk_core::worktree::Manager as WorktreeManager;
use autosk_core::Result;

use crate::notify::{ChangePoller, Hub};

/// Daemon-wide tuning (the `serve` flags).
#[derive(Clone)]
pub struct DaemonConfig {
    pub workers: usize,
    pub poll_interval: Duration,
    /// `Some(d)` → compaction interval; `None` → disabled.
    pub gc_interval: Option<Duration>,
    pub pi_bin: String,
    pub idle_timeout: Duration,
    pub grace: Duration,
    pub session_poll_budget: Duration,
}

impl Default for DaemonConfig {
    fn default() -> Self {
        DaemonConfig {
            workers: 4,
            poll_interval: Duration::from_secs(2),
            gc_interval: Some(Duration::from_secs(30 * 60)),
            pi_bin: String::new(),
            idle_timeout: Duration::from_secs(30 * 60),
            grace: Duration::from_secs(10),
            session_poll_budget: Duration::ZERO,
        }
    }
}

/// The running daemon: the project cache + the scheduler/poller/compactor mesh.
pub struct Daemon {
    pub mgr: Arc<Manager>,
    pub registry: Arc<Registry>,
    pub scheduler: Arc<Scheduler>,
    pub runners: Arc<RunnerRegistry>,
    pub attachments: Arc<Attachments>,
    /// Agent-package registry (shared with the executor) — used by the write
    /// verbs (`agent.install`/`workflow.create`/`project.init`).
    pub packages: Arc<PkgRegistry>,
    /// Worktree manager (shared with the executor) — used by isolated-workflow
    /// write verbs (`create`/`enroll`/`done`/`cancel`/`workflow.updateIsolation`).
    pub worktree: Arc<WorktreeManager>,
    /// Broadcast hub for `task-changed`/`project-changed` notifications.
    pub hub: Arc<Hub>,
    cfg: DaemonConfig,
    started: Mutex<HashMap<String, ProjectRuntime>>,
}

struct ProjectRuntime {
    poller: Arc<Poller>,
    compactor: Arc<Compactor>,
    change: ChangePoller,
}

impl Daemon {
    /// Builds + starts the daemon's scheduler. Projects start their poller +
    /// compactor lazily on first [`Daemon::resolve`].
    pub fn new(mgr: Arc<Manager>, registry: Arc<Registry>, cfg: DaemonConfig) -> Arc<Daemon> {
        let runners = Arc::new(RunnerRegistry::new());
        let attachments = Arc::new(Attachments::new());
        let packages = Arc::new(PkgRegistry::open(
            PkgRegistry::default_prefix()
                .unwrap_or_else(|| std::path::PathBuf::from(".autosk/packages")),
        ));
        let worktree = Arc::new(WorktreeManager::new());

        let exec = Arc::new(DaemonExecutor {
            mgr: Arc::clone(&mgr),
            packages: Arc::clone(&packages),
            worktree: Arc::clone(&worktree),
            runners: Arc::clone(&runners),
            attachments: Arc::clone(&attachments),
            cfg: cfg.clone(),
        });
        let scheduler = Scheduler::new(
            exec as Arc<dyn SchedExecutor>,
            SchedConfig {
                workers: cfg.workers,
                queue_depth: 0,
            },
        );
        scheduler.start();

        Arc::new(Daemon {
            mgr,
            registry,
            scheduler,
            runners,
            attachments,
            packages,
            worktree,
            hub: Arc::new(Hub::new()),
            cfg,
            started: Mutex::new(HashMap::new()),
        })
    }

    /// Resolves a project selector and ensures its poller + compactor are
    /// running (idempotent). The Phase-1 read paths can keep calling
    /// `mgr.resolve` directly; the job/streaming paths go through here so a
    /// freshly-touched project starts being driven.
    pub fn resolve(&self, cwd: &str, db_path: &str) -> Result<Arc<Project>> {
        let proj = self.mgr.resolve(cwd, db_path)?;
        self.ensure_started(&proj);
        Ok(proj)
    }

    fn ensure_started(&self, proj: &Arc<Project>) {
        let mut started = self.started.lock().unwrap();
        if started.contains_key(&proj.root) {
            return;
        }
        let poller = Poller::new(
            Arc::clone(&proj.db),
            Arc::clone(&self.scheduler),
            proj.root.clone(),
            self.cfg.poll_interval,
        );
        poller.start();
        let (interval, disabled) = match self.cfg.gc_interval {
            Some(d) => (d, false),
            None => (Duration::ZERO, true),
        };
        let compactor = Compactor::new(Arc::clone(&proj.db), proj.root.clone(), interval, disabled);
        compactor.start();
        // task-changed change poller (poll-backed notifications, plan §5).
        let change = ChangePoller::start(
            Arc::clone(&self.hub),
            Arc::clone(&proj.db),
            proj.root.clone(),
            proj.db_path.clone(),
            self.cfg.poll_interval,
        );
        started.insert(
            proj.root.clone(),
            ProjectRuntime {
                poller,
                compactor,
                change,
            },
        );
    }

    /// Stops every project's poller/compactor and the scheduler. Best-effort.
    pub fn shutdown(&self) {
        let mut runtimes: Vec<ProjectRuntime> = self
            .started
            .lock()
            .unwrap()
            .drain()
            .map(|(_, v)| v)
            .collect();
        for rt in runtimes.iter_mut() {
            rt.poller.stop();
            rt.compactor.stop();
            rt.change.stop();
        }
        self.scheduler.stop();
    }

    /// Number of started (poller-running) projects (observability/tests).
    pub fn started_len(&self) -> usize {
        self.started.lock().unwrap().len()
    }

    /// True when the daemon is actively driving work: any queued/running job OR
    /// any `status='work'` task across loaded projects. Part of the idle-
    /// shutdown policy (plan §4.2: shut down only when no clients AND no running
    /// jobs AND no non-terminal work tasks).
    pub fn has_pending_work(&self) -> bool {
        for p in self.mgr.loaded() {
            let pending =
                p.db.with_read(|conn| {
                    let jobs: i64 = conn.query_row(
                        "SELECT COUNT(*) FROM daemon_runs WHERE status IN ('queued','running')",
                        [],
                        |r| r.get(0),
                    )?;
                    let work: i64 = conn.query_row(
                        "SELECT COUNT(*) FROM tasks WHERE status = 'work'",
                        [],
                        |r| r.get(0),
                    )?;
                    Ok(jobs + work)
                })
                .unwrap_or(1);
            if pending > 0 {
                return true;
            }
        }
        false
    }
}

/// The scheduler's per-job lifecycle: build an [`Executor`] for the job's
/// project and run it. The project is already open (the poller created the run).
struct DaemonExecutor {
    mgr: Arc<Manager>,
    packages: Arc<PkgRegistry>,
    worktree: Arc<WorktreeManager>,
    runners: Arc<RunnerRegistry>,
    attachments: Arc<Attachments>,
    cfg: DaemonConfig,
}

impl SchedExecutor for DaemonExecutor {
    fn run(&self, ctx: &Ctx, job: &Job) {
        let proj = match self.mgr.resolve(&job.project, "") {
            Ok(p) => p,
            Err(e) => {
                eprintln!(
                    "daemon: cannot resolve project {} for job {}: {e}",
                    job.project, job.id
                );
                return;
            }
        };
        let deps = ExecDeps {
            db: Arc::clone(&proj.db),
            tasks: Arc::clone(&proj.db) as Arc<dyn autosk_core::wfengine::TaskWriter>,
            packages: Arc::clone(&self.packages),
            worktree: Arc::clone(&self.worktree) as Arc<dyn autosk_core::worktree::WorktreeManager>,
            runners: Some(Arc::clone(&self.runners)),
            attachments: Some(Arc::clone(&self.attachments)),
        };
        let cfg = ExecConfig {
            pi_bin: self.cfg.pi_bin.clone(),
            session_dir_root: String::new(),
            project_root: proj.root.clone(),
            db_path: proj.db_path.clone(),
            grace: self.cfg.grace,
            idle_timeout: self.cfg.idle_timeout,
            session_poll_budget: self.cfg.session_poll_budget,
        };
        let exec = Executor::new(deps, executor::default_pi_factory(), cfg);
        let _ = exec.run(ctx, &job.id);
    }
}
