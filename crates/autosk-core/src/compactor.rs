//! Per-project doltlite GC scheduler — the Rust port of
//! `internal/daemon/compactor`.
//!
//! Runs `SELECT dolt_gc()` (via [`Db::gc`], under the in-process RwLock GC
//! discipline) every `interval` so the chunk-store WAL never dominates query
//! cost. The first tick is offset by `interval` (a freshly-opened project has
//! nothing to reclaim).

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use crate::store::Db;

/// Default gap between compactions (mirror of `compactor.DefaultInterval`).
pub const DEFAULT_INTERVAL: Duration = Duration::from_secs(30 * 60);

/// A running compaction loop for one project.
pub struct Compactor {
    db: Arc<Db>,
    project_key: String,
    interval: Duration,
    disabled: bool,
    stop: Arc<AtomicBool>,
    handle: Mutex<Option<JoinHandle<()>>>,
    ticks: Arc<AtomicU64>,
}

impl Compactor {
    /// Constructs a compactor. `interval == 0` → [`DEFAULT_INTERVAL`]; a
    /// negative interval is expressed as `disabled = true`.
    pub fn new(
        db: Arc<Db>,
        project_key: String,
        interval: Duration,
        disabled: bool,
    ) -> Arc<Compactor> {
        let interval = if interval.is_zero() {
            DEFAULT_INTERVAL
        } else {
            interval
        };
        Arc::new(Compactor {
            db,
            project_key,
            interval,
            disabled,
            stop: Arc::new(AtomicBool::new(false)),
            handle: Mutex::new(None),
            ticks: Arc::new(AtomicU64::new(0)),
        })
    }

    /// Successful scheduled compactions so far.
    pub fn ticks(&self) -> u64 {
        self.ticks.load(Ordering::SeqCst)
    }

    /// Launches the loop (no-op when disabled). The first tick fires after
    /// `interval`, not immediately.
    pub fn start(self: &Arc<Self>) {
        if self.disabled {
            return;
        }
        let me = Arc::clone(self);
        let handle = std::thread::spawn(move || {
            while !me.stop.load(Ordering::SeqCst) {
                let mut slept = Duration::ZERO;
                while slept < me.interval && !me.stop.load(Ordering::SeqCst) {
                    std::thread::sleep(Duration::from_millis(100));
                    slept += Duration::from_millis(100);
                }
                if me.stop.load(Ordering::SeqCst) {
                    break;
                }
                me.tick();
            }
        });
        *self.handle.lock().unwrap() = Some(handle);
    }

    /// Stops the loop and joins it.
    pub fn stop(&self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.handle.lock().unwrap().take() {
            let _ = h.join();
        }
    }

    /// Fires a single compaction outside the loop (the `autosk gc` path + tests).
    pub fn run_once(&self) -> crate::Result<crate::store::GcStats> {
        self.db.gc()
    }

    fn tick(&self) {
        match self.db.gc() {
            Ok(stats) => {
                self.ticks.fetch_add(1, Ordering::SeqCst);
                eprintln!(
                    "compactor: dolt_gc (project={} removed={} kept={})",
                    self.project_key, stats.chunks_removed, stats.chunks_kept
                );
            }
            Err(e) => eprintln!(
                "compactor: dolt_gc failed (project={}): {e}",
                self.project_key
            ),
        }
    }
}
