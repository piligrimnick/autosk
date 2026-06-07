//! Per-project workflow poller — the Rust port of `internal/daemon/poller`.
//!
//! Every interval it selects `work` tasks whose current step's agent is
//! non-human and which have no queued/running `daemon_runs` row, creates a run
//! for each, and enqueues it on the global [`Scheduler`].

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use rusqlite::Connection;

use crate::runstore::NewRun;
use crate::scheduler::{EnqueueError, Job, Scheduler};
use crate::store::Db;

/// Default poll cadence (mirror of `poller.DefaultInterval`).
pub const DEFAULT_INTERVAL: Duration = Duration::from_secs(2);

/// One scan candidate.
#[derive(Debug, Clone)]
pub struct Candidate {
    pub task_id: String,
    pub step_id: String,
}

/// Scans `db` for ready work-task candidates (mirror of `Poller.Scan`):
/// `status='work' AND agent.is_human=0 AND no active run`, ordered by
/// `priority ASC, created_at ASC`.
pub fn scan(conn: &Connection) -> crate::Result<Vec<Candidate>> {
    let mut stmt = conn.prepare(
        "SELECT t.id, t.current_step_id \
           FROM tasks t \
           JOIN steps s  ON t.current_step_id = s.id \
           JOIN agents a ON s.agent_id = a.id \
          WHERE t.status = 'work' \
            AND a.is_human = 0 \
            AND NOT EXISTS ( \
                  SELECT 1 FROM daemon_runs r \
                   WHERE r.task_id = t.id AND r.status IN ('queued','running')) \
          ORDER BY t.priority ASC, t.created_at ASC",
    )?;
    let rows = stmt.query_map([], |row| {
        Ok(Candidate {
            task_id: row.get(0)?,
            step_id: row.get(1)?,
        })
    })?;
    let mut out = Vec::new();
    for r in rows {
        out.push(r?);
    }
    Ok(out)
}

/// A running poll loop for one project.
pub struct Poller {
    db: Arc<Db>,
    sched: Arc<Scheduler>,
    project_key: String,
    interval: Duration,
    stop: Arc<AtomicBool>,
    handle: Mutex<Option<JoinHandle<()>>>,
}

impl Poller {
    pub fn new(
        db: Arc<Db>,
        sched: Arc<Scheduler>,
        project_key: String,
        interval: Duration,
    ) -> Arc<Poller> {
        let interval = if interval.is_zero() {
            DEFAULT_INTERVAL
        } else {
            interval
        };
        Arc::new(Poller {
            db,
            sched,
            project_key,
            interval,
            stop: Arc::new(AtomicBool::new(false)),
            handle: Mutex::new(None),
        })
    }

    /// Launches the loop (immediate first scan, then every interval).
    pub fn start(self: &Arc<Self>) {
        let me = Arc::clone(self);
        let handle = std::thread::spawn(move || {
            me.scan_once();
            while !me.stop.load(Ordering::SeqCst) {
                // Sleep in small slices so Stop is responsive.
                let mut slept = Duration::ZERO;
                while slept < me.interval && !me.stop.load(Ordering::SeqCst) {
                    std::thread::sleep(Duration::from_millis(50));
                    slept += Duration::from_millis(50);
                }
                if me.stop.load(Ordering::SeqCst) {
                    break;
                }
                me.scan_once();
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

    /// Runs one scan + enqueue pass. Exposed for the end-to-end test.
    pub fn scan_once(&self) {
        let cands = match self.db.with_write(scan) {
            Ok(c) => c,
            Err(e) => {
                eprintln!("poller: scan failed: {e}");
                return;
            }
        };
        for c in cands {
            if let Err(e) = self.enqueue_candidate(&c) {
                eprintln!("poller: enqueue failed (task={}): {e}", c.task_id);
            }
        }
    }

    fn enqueue_candidate(&self, c: &Candidate) -> crate::Result<()> {
        let run = self.db.run_create(&NewRun {
            task_id: c.task_id.clone(),
            step_id: c.step_id.clone(),
            max_corrections: 0,
        })?;
        match self.sched.enqueue(Job {
            project: self.project_key.clone(),
            id: run.job_id.clone(),
        }) {
            Ok(()) => Ok(()),
            // Queue full: the row stays queued; the next tick skips it (it now
            // has a queued run) and a freed worker picks it up.
            Err(EnqueueError::QueueFull) => Ok(()),
            Err(e) => Err(crate::Error::Migration(format!("enqueue: {e:?}"))),
        }
    }
}
