//! Server→client notification fan-out (plan §5): `task-changed` /
//! `project-changed` (poll-backed) + the connection hub the streaming and
//! notification paths share.
//!
//! Every live connection registers its writer with the [`Hub`]; the daemon
//! broadcasts notifications to all of them. `task-changed` is emitted both
//! eagerly after a client write verb AND by a per-project change poller (so the
//! daemon's own executor-driven advances also notify), mirroring lazy's old
//! 2-second client poll but as a server push.

use std::collections::HashMap;
use std::io::Write;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use autosk_core::store::Db;
use autosk_proto::rpc::Notification;

/// A connection's shared, mutex-guarded line writer (UDS or TCP).
pub type SharedWriter = Arc<Mutex<Box<dyn Write + Send>>>;

/// Broadcast registry of all live connection writers.
pub struct Hub {
    subs: Mutex<HashMap<u64, SharedWriter>>,
    next: AtomicU64,
}

impl Default for Hub {
    fn default() -> Self {
        Hub {
            subs: Mutex::new(HashMap::new()),
            next: AtomicU64::new(1),
        }
    }
}

impl Hub {
    pub fn new() -> Hub {
        Hub::default()
    }

    /// Registers a connection writer; returns its id for [`Hub::unregister`].
    pub fn register(&self, w: SharedWriter) -> u64 {
        let id = self.next.fetch_add(1, Ordering::SeqCst);
        self.subs.lock().unwrap().insert(id, w);
        id
    }

    pub fn unregister(&self, id: u64) {
        self.subs.lock().unwrap().remove(&id);
    }

    /// Number of notification-subscribed connections (those that issued
    /// `task.subscribe`/`project.subscribe`). NB: this is a SUBSET of all live
    /// connections — the idle-shutdown "no connected clients" predicate uses
    /// [`crate::daemon::Daemon::live_connections`], which counts every
    /// connection, not just subscribers.
    pub fn client_count(&self) -> usize {
        self.subs.lock().unwrap().len()
    }

    /// Sends `note` to every connected client (best-effort; a write failure
    /// just skips that client — its read loop will reap it on disconnect).
    ///
    /// The `subs` lock is held only long enough to SNAPSHOT the writer handles;
    /// the (potentially blocking) socket writes happen AFTER the lock is
    /// released. A single slow/wedged consumer therefore can't stall
    /// `register`/`unregister`/`client_count` (the last of which feeds the
    /// idle-shutdown watchdog).
    pub fn broadcast(&self, note: &Notification) {
        let Ok(mut buf) = serde_json::to_vec(note) else {
            return;
        };
        buf.push(b'\n');
        let writers: Vec<SharedWriter> = {
            let subs = self.subs.lock().unwrap();
            subs.values().cloned().collect()
        };
        for w in writers {
            if let Ok(mut guard) = w.lock() {
                let _ = guard.write_all(&buf).and_then(|()| guard.flush());
            }
        }
    }

    /// Broadcasts a `task-changed` notification for a project.
    pub fn task_changed(&self, root: &str, db_path: &str) {
        self.broadcast(&Notification {
            method: "task-changed".into(),
            params: serde_json::json!({"root": root, "db_path": db_path}),
        });
    }

    /// Broadcasts a `project-changed` notification (registry add/remove).
    pub fn project_changed(&self) {
        self.broadcast(&Notification {
            method: "project-changed".into(),
            params: serde_json::json!({}),
        });
    }
}

/// A running per-project change poller; dropping/stopping it joins the thread.
pub struct ChangePoller {
    stop: Arc<AtomicBool>,
    handle: Option<std::thread::JoinHandle<()>>,
}

impl ChangePoller {
    /// Starts a thread that polls the project's task-state signature and
    /// broadcasts `task-changed` whenever it moves (covers executor-driven
    /// advances that don't flow through the RPC write verbs).
    pub fn start(
        hub: Arc<Hub>,
        db: Arc<Db>,
        root: String,
        db_path: String,
        interval: Duration,
    ) -> ChangePoller {
        let stop = Arc::new(AtomicBool::new(false));
        let stop_c = Arc::clone(&stop);
        let handle = std::thread::spawn(move || {
            let mut last = task_signature(&db);
            while !stop_c.load(Ordering::SeqCst) {
                // Sleep in small slices so stop()/join() returns promptly even
                // for a long poll interval (mirror of the core Poller).
                let mut slept = Duration::ZERO;
                while slept < interval && !stop_c.load(Ordering::SeqCst) {
                    std::thread::sleep(Duration::from_millis(50));
                    slept += Duration::from_millis(50);
                }
                if stop_c.load(Ordering::SeqCst) {
                    return;
                }
                let cur = task_signature(&db);
                if cur != last {
                    last = cur;
                    hub.task_changed(&root, &db_path);
                }
            }
        });
        ChangePoller {
            stop,
            handle: Some(handle),
        }
    }

    pub fn stop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

impl Drop for ChangePoller {
    fn drop(&mut self) {
        self.stop();
    }
}

/// A cheap signature of task + run state: `(count, max(updated_at), run-count,
/// max(run updated marker))`. Any insert/update/delete/status-change moves it.
fn task_signature(db: &Db) -> (i64, i64, i64, i64) {
    db.with_read(|conn| {
        let (n, mx): (i64, i64) = conn.query_row(
            "SELECT COUNT(*), COALESCE(MAX(updated_at),0) FROM tasks",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )?;
        let (rn, rmx): (i64, i64) = conn.query_row(
            "SELECT COUNT(*), COALESCE(MAX(COALESCE(finished_at, started_at, created_at)),0) FROM daemon_runs",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )?;
        Ok((n, mx, rn, rmx))
    })
    .unwrap_or((0, 0, 0, 0))
}
