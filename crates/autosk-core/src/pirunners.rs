//! Daemon-wide, in-memory registry of live runner handles + the attach
//! counter — the Rust port of `internal/daemon/pirunners`.
//!
//! Neither structure is persisted: a daemon restart wipes live runners (the
//! child processes are gone, and `daemon_restart` recovery sweeps the
//! abandoned `daemon_runs` rows) and resets attach counts to zero.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use crate::runner::PiRunner;

/// A live runner handle (the attach surface: `send_command`/`abort`/
/// `is_streaming`). `Arc<dyn PiRunner>` lets the executor and the daemon's
/// input/abort handlers share one runner.
pub type RunnerHandle = Arc<dyn PiRunner>;

/// Maps `job_id → live runner` for every job running inside this daemon. The
/// executor registers on spawn and unregisters in cleanup; handlers read.
#[derive(Default)]
pub struct Registry {
    entries: Mutex<HashMap<String, RunnerHandle>>,
}

impl Registry {
    pub fn new() -> Registry {
        Registry {
            entries: Mutex::new(HashMap::new()),
        }
    }

    /// Associates `h` with `job_id`.
    pub fn register(&self, job_id: &str, h: RunnerHandle) {
        if job_id.is_empty() {
            return;
        }
        self.entries.lock().unwrap().insert(job_id.to_string(), h);
    }

    /// Drops the entry for `job_id` if present. Idempotent.
    pub fn unregister(&self, job_id: &str) {
        self.entries.lock().unwrap().remove(job_id);
    }

    /// Returns the registered handle for `job_id`, if any.
    pub fn get(&self, job_id: &str) -> Option<RunnerHandle> {
        self.entries.lock().unwrap().get(job_id).cloned()
    }

    /// Number of currently-registered runners.
    pub fn len(&self) -> usize {
        self.entries.lock().unwrap().len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// In-memory attach counter. The streaming handler increments on
/// `job.subscribe {attach:true}` and decrements on disconnect; the executor
/// consults [`Attachments::attached`] on every turn boundary to disarm the
/// idle-timeout and skip kickback while a client drives the conversation.
#[derive(Default)]
pub struct Attachments {
    counts: Mutex<HashMap<String, i64>>,
}

impl Attachments {
    pub fn new() -> Attachments {
        Attachments {
            counts: Mutex::new(HashMap::new()),
        }
    }

    /// Bumps the counter for `job_id` and returns a one-shot release guard
    /// (decrements on drop). Mirror of `Attachments.Acquire`.
    pub fn acquire(self: &Arc<Self>, job_id: &str) -> AttachGuard {
        if !job_id.is_empty() {
            *self
                .counts
                .lock()
                .unwrap()
                .entry(job_id.to_string())
                .or_insert(0) += 1;
        }
        AttachGuard {
            attachments: Arc::clone(self),
            job_id: job_id.to_string(),
            released: false,
        }
    }

    /// Whether at least one client is attached to `job_id`.
    pub fn attached(&self, job_id: &str) -> bool {
        self.count(job_id) > 0
    }

    /// Current attach count for `job_id`.
    pub fn count(&self, job_id: &str) -> i64 {
        *self.counts.lock().unwrap().get(job_id).unwrap_or(&0)
    }

    fn release(&self, job_id: &str) {
        let mut g = self.counts.lock().unwrap();
        if let Some(n) = g.get_mut(job_id) {
            *n -= 1;
            if *n <= 0 {
                g.remove(job_id);
            }
        }
    }
}

/// One-shot attach release handle (decrements the counter on drop).
pub struct AttachGuard {
    attachments: Arc<Attachments>,
    job_id: String,
    released: bool,
}

impl AttachGuard {
    /// Releases early (idempotent; `drop` also releases).
    pub fn release(mut self) {
        if !self.released {
            self.attachments.release(&self.job_id);
            self.released = true;
        }
    }
}

impl Drop for AttachGuard {
    fn drop(&mut self) {
        if !self.released {
            self.attachments.release(&self.job_id);
            self.released = true;
        }
    }
}
