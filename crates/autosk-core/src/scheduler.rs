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
        // Each job gets its own cancellable ctx; `cancel(job)` and `stop()`
        // (which fires every active token) both unwind a running executor.
        let (job_ctx, token) = Ctx::new_cancellable();
        self.active.lock().unwrap().insert(key.clone(), token);
        self.exec.run(&job_ctx, &job);
        self.active.lock().unwrap().remove(&key);
    }
}
