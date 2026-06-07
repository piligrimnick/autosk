//! Global job queue + worker pool — the Rust port of
//! `internal/daemon/scheduler`.
//!
//! Jobs are enqueued as qualified `(project, job_id)` pairs, picked up by a
//! fixed-size worker pool, and handed to a [`SchedExecutor`] that drives the
//! run through its terminal state. Cancellation fires the per-job
//! [`CancelToken`]; the executor observes it and unwinds.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender, TrySendError};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

use crate::ctx::{CancelToken, Ctx};

/// A qualified job reference (mirror of `scheduler.Job`).
#[derive(Debug, Clone)]
pub struct Job {
    /// Canonical project root (matches the project manager's key).
    pub project: String,
    /// Per-project `daemon_runs.job_id`.
    pub id: String,
}

impl Job {
    /// `"<project>::<id>"` — the internal active-map key.
    pub fn key(&self) -> String {
        format!("{}::{}", self.project, self.id)
    }
}

/// The per-job lifecycle implementation the scheduler drives. The daemon wires
/// this to look up the job's project and call its executor (mirror of the Go
/// `scheduler.Executor` interface).
pub trait SchedExecutor: Send + Sync {
    /// Runs the job to a terminal state. The scheduler only logs the return.
    fn run(&self, ctx: &Ctx, job: &Job);
}

/// Scheduler tuning (mirror of `scheduler.Config`).
#[derive(Debug, Clone)]
pub struct Config {
    pub workers: usize,
    pub queue_depth: usize,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            workers: 1,
            queue_depth: 16,
        }
    }
}

/// Why an enqueue failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EnqueueError {
    NotStarted,
    QueueFull,
    EmptyId,
}

/// The scheduler.
pub struct Scheduler {
    exec: Arc<dyn SchedExecutor>,
    tx: SyncSender<Job>,
    rx: Mutex<Option<Receiver<Job>>>,
    root: Ctx,
    cancel_root: CancelToken,
    started: AtomicBool,
    workers: Mutex<Vec<JoinHandle<()>>>,
    active: Arc<Mutex<HashMap<String, CancelToken>>>,
    num_workers: usize,
}

impl Scheduler {
    /// Constructs a scheduler. Call [`Scheduler::start`] to launch the pool.
    pub fn new(exec: Arc<dyn SchedExecutor>, cfg: Config) -> Arc<Scheduler> {
        let workers = cfg.workers.max(1);
        let depth = if cfg.queue_depth == 0 {
            workers.max(16)
        } else {
            cfg.queue_depth
        };
        let (tx, rx) = sync_channel(depth);
        let (root, cancel_root) = Ctx::new_cancellable();
        Arc::new(Scheduler {
            exec,
            tx,
            rx: Mutex::new(Some(rx)),
            root,
            cancel_root,
            started: AtomicBool::new(false),
            workers: Mutex::new(Vec::new()),
            active: Arc::new(Mutex::new(HashMap::new())),
            num_workers: workers,
        })
    }

    /// Launches the worker pool.
    pub fn start(self: &Arc<Self>) {
        if self.started.swap(true, Ordering::SeqCst) {
            return;
        }
        let rx = self.rx.lock().unwrap().take().expect("rx taken once");
        let rx = Arc::new(Mutex::new(rx));
        let mut handles = Vec::new();
        for i in 0..self.num_workers {
            let me = Arc::clone(self);
            let rx = Arc::clone(&rx);
            handles.push(std::thread::spawn(move || me.worker_loop(i, rx)));
        }
        *self.workers.lock().unwrap() = handles;
    }

    /// Enqueues a qualified job (non-blocking; [`EnqueueError::QueueFull`] when
    /// the buffer is full — the caller leaves the row queued for the next tick).
    pub fn enqueue(&self, job: Job) -> Result<(), EnqueueError> {
        if !self.started.load(Ordering::SeqCst) {
            return Err(EnqueueError::NotStarted);
        }
        if job.id.is_empty() {
            return Err(EnqueueError::EmptyId);
        }
        match self.tx.try_send(job) {
            Ok(()) => Ok(()),
            Err(TrySendError::Full(_)) => Err(EnqueueError::QueueFull),
            Err(TrySendError::Disconnected(_)) => Err(EnqueueError::NotStarted),
        }
    }

    /// Fires the cancel token for an active job. No-op when not active.
    pub fn cancel(&self, job: &Job) -> bool {
        if let Some(tok) = self.active.lock().unwrap().get(&job.key()) {
            tok.cancel();
            true
        } else {
            false
        }
    }

    /// Whether the named job is currently executing.
    pub fn is_active(&self, job: &Job) -> bool {
        self.active.lock().unwrap().contains_key(&job.key())
    }

    /// In-flight job count grouped by project (for `healthz`).
    pub fn active_count_by_project(&self) -> HashMap<String, i64> {
        let mut out = HashMap::new();
        for k in self.active.lock().unwrap().keys() {
            if let Some(idx) = k.find("::") {
                *out.entry(k[..idx].to_string()).or_insert(0) += 1;
            }
        }
        out
    }

    /// Cancels every in-flight job and stops accepting new ones. Workers drain
    /// the queue and exit; this joins them.
    pub fn stop(&self) {
        if !self.started.swap(false, Ordering::SeqCst) {
            return;
        }
        self.cancel_root.cancel();
        for tok in self.active.lock().unwrap().values() {
            tok.cancel();
        }
        // Dropping the only sender would close the queue; but we hold `tx`
        // for the scheduler's lifetime, so instead workers exit on the root
        // cancel observed between jobs.
        let handles: Vec<_> = std::mem::take(&mut *self.workers.lock().unwrap());
        for h in handles {
            let _ = h.join();
        }
    }

    fn worker_loop(self: Arc<Self>, _idx: usize, rx: Arc<Mutex<Receiver<Job>>>) {
        loop {
            if self.root.is_cancelled() {
                return;
            }
            let job = {
                let guard = rx.lock().unwrap();
                guard.recv_timeout(std::time::Duration::from_millis(50))
            };
            match job {
                Ok(job) => self.run_one(job),
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => continue,
                Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => return,
            }
        }
    }

    fn run_one(&self, job: Job) {
        let key = job.key();
        // Derive the per-job ctx from `self.root` so root cancellation (shutdown)
        // ALWAYS propagates to an in-flight job, while the per-job token still
        // cancels just this one job. Insert the token into `active` BEFORE the
        // run and re-check root cancellation afterwards: that closes the race
        // with `stop()` (which fires `cancel_root` then snapshots `active`) —
        // even if `stop` ran between dequeue and insert, the job ctx already
        // carries the root flag, and the post-insert check abandons the run.
        let (job_ctx, token) = self.root.child_cancellable();
        self.active.lock().unwrap().insert(key.clone(), token);
        // RAII so the `active` entry is cleared on EVERY exit path — including a
        // panic inside `exec.run` (which `catch_unwind` below contains so the
        // worker thread survives and keeps dispatching).
        let _active_guard = ActiveGuard {
            active: Arc::clone(&self.active),
            key: key.clone(),
        };
        if self.root.is_cancelled() {
            return;
        }
        let exec = Arc::clone(&self.exec);
        let result =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| exec.run(&job_ctx, &job)));
        if result.is_err() {
            eprintln!("scheduler: job {key} panicked; worker recovered");
        }
    }
}

/// Clears a job's `active` entry on drop so a panic in `exec.run` can't leak it
/// (which would pin `is_active` true and swallow later `cancel()` calls).
struct ActiveGuard {
    active: Arc<Mutex<HashMap<String, CancelToken>>>,
    key: String,
}

impl Drop for ActiveGuard {
    fn drop(&mut self) {
        if let Ok(mut g) = self.active.lock() {
            g.remove(&self.key);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use std::time::Duration;

    /// An executor that panics on the first job, succeeds afterwards, and
    /// records whether root cancellation reached it.
    struct PanicOnceExec {
        runs: AtomicUsize,
        observed_cancel: AtomicBool,
    }
    impl SchedExecutor for PanicOnceExec {
        fn run(&self, ctx: &Ctx, _job: &Job) {
            let n = self.runs.fetch_add(1, Ordering::SeqCst);
            if ctx.is_cancelled() {
                self.observed_cancel.store(true, Ordering::SeqCst);
                return;
            }
            if n == 0 {
                panic!("boom");
            }
        }
    }

    fn wait_until(mut f: impl FnMut() -> bool) -> bool {
        for _ in 0..200 {
            if f() {
                return true;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        f()
    }

    #[test]
    fn worker_survives_a_panicking_job() {
        let exec = Arc::new(PanicOnceExec {
            runs: AtomicUsize::new(0),
            observed_cancel: AtomicBool::new(false),
        });
        let sched = Scheduler::new(
            Arc::clone(&exec) as Arc<dyn SchedExecutor>,
            Config {
                workers: 1,
                queue_depth: 8,
            },
        );
        sched.start();
        // First job panics; the single worker must keep dispatching the rest.
        for i in 0..4 {
            sched
                .enqueue(Job {
                    project: "p".into(),
                    id: format!("j{i}"),
                })
                .unwrap();
        }
        assert!(
            wait_until(|| exec.runs.load(Ordering::SeqCst) >= 4),
            "worker died after the panicking job (runs={})",
            exec.runs.load(Ordering::SeqCst)
        );
        // The panicked job's active entry must not have leaked.
        assert!(!sched.is_active(&Job {
            project: "p".into(),
            id: "j0".into()
        }));
        sched.stop();
    }

    #[test]
    fn stop_propagates_root_cancel_to_in_flight_job() {
        // A long-running job that only returns once its ctx is cancelled.
        struct BlockUntilCancel {
            entered: Arc<AtomicBool>,
            saw_cancel: Arc<AtomicBool>,
        }
        impl SchedExecutor for BlockUntilCancel {
            fn run(&self, ctx: &Ctx, _job: &Job) {
                self.entered.store(true, Ordering::SeqCst);
                for _ in 0..2000 {
                    if ctx.is_cancelled() {
                        self.saw_cancel.store(true, Ordering::SeqCst);
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(2));
                }
            }
        }
        let entered = Arc::new(AtomicBool::new(false));
        let saw = Arc::new(AtomicBool::new(false));
        let exec = Arc::new(BlockUntilCancel {
            entered: Arc::clone(&entered),
            saw_cancel: Arc::clone(&saw),
        });
        let sched = Scheduler::new(
            exec as Arc<dyn SchedExecutor>,
            Config {
                workers: 1,
                queue_depth: 4,
            },
        );
        sched.start();
        sched
            .enqueue(Job {
                project: "p".into(),
                id: "j".into(),
            })
            .unwrap();
        assert!(
            wait_until(|| entered.load(Ordering::SeqCst)),
            "job never started"
        );
        // stop() must unwind the in-flight job via root-cancel propagation and
        // join the worker without hanging.
        sched.stop();
        assert!(
            saw.load(Ordering::SeqCst),
            "root cancellation did not reach the in-flight job"
        );
    }

    #[test]
    fn cancel_fires_token_of_a_single_active_job() {
        // The per-job cancel path the `job.cancel` RPC drives for a running run:
        // `scheduler.cancel(&job)` must unwind exactly that job's executor.
        struct BlockUntilCancel {
            entered: Arc<AtomicBool>,
            saw_cancel: Arc<AtomicBool>,
        }
        impl SchedExecutor for BlockUntilCancel {
            fn run(&self, ctx: &Ctx, _job: &Job) {
                self.entered.store(true, Ordering::SeqCst);
                for _ in 0..2000 {
                    if ctx.is_cancelled() {
                        self.saw_cancel.store(true, Ordering::SeqCst);
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(2));
                }
            }
        }
        let entered = Arc::new(AtomicBool::new(false));
        let saw = Arc::new(AtomicBool::new(false));
        let exec = Arc::new(BlockUntilCancel {
            entered: Arc::clone(&entered),
            saw_cancel: Arc::clone(&saw),
        });
        let sched = Scheduler::new(
            exec as Arc<dyn SchedExecutor>,
            Config {
                workers: 1,
                queue_depth: 4,
            },
        );
        sched.start();
        let job = Job {
            project: "p".into(),
            id: "j".into(),
        };
        sched.enqueue(job.clone()).unwrap();
        assert!(
            wait_until(|| entered.load(Ordering::SeqCst)),
            "job never ran"
        );
        assert!(sched.is_active(&job), "job should be active while running");
        assert!(sched.cancel(&job), "cancel should report the job active");
        assert!(
            wait_until(|| saw.load(Ordering::SeqCst)),
            "per-job cancel did not reach the executor"
        );
        assert!(
            wait_until(|| !sched.is_active(&job)),
            "active entry not cleared after the job returned"
        );
        sched.stop();
    }
}
