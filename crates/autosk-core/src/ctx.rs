//! A minimal cancellation + deadline context — the Rust stand-in for the Go
//! `context.Context` the daemon threads through the executor and runners.
//!
//! Go's executor relies on two `context` behaviours: explicit cancellation
//! (the scheduler cancels a job) and per-turn deadlines (the unattached
//! idle-timeout). [`Ctx`] models exactly those. A child [`Ctx`] derived via
//! [`Ctx::with_timeout`] / [`Ctx::child`] shares the parent's cancellation
//! flag (so cancelling the parent cancels the child) and adds its own
//! deadline. [`Ctx::child_cancellable`] additionally ORs in a fresh per-child
//! cancel token — the analogue of `context.WithCancel(parent)`: cancelling the
//! parent OR the child token cancels the child.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

/// Why a [`Ctx`] is done.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Done {
    /// The cancel token was fired.
    Cancelled,
    /// The deadline elapsed.
    DeadlineExceeded,
}

/// A cancellation + optional deadline handle. Cheap to clone (shares the
/// cancel flags). A context is cancelled when ANY of its flags fire, so a
/// child built via [`Ctx::child_cancellable`] inherits every ancestor's
/// cancellation while also carrying its own token.
#[derive(Clone)]
pub struct Ctx {
    cancels: Vec<Arc<AtomicBool>>,
    deadline: Option<Instant>,
}

/// Fires the cancellation of a [`Ctx`] (and every child derived from it).
#[derive(Clone)]
pub struct CancelToken {
    cancel: Arc<AtomicBool>,
}

impl CancelToken {
    /// Cancels the associated context tree. Idempotent.
    pub fn cancel(&self) {
        self.cancel.store(true, Ordering::SeqCst);
    }
}

impl Ctx {
    /// A never-cancelled, no-deadline context (mirror of `context.Background`).
    pub fn background() -> Ctx {
        Ctx {
            cancels: Vec::new(),
            deadline: None,
        }
    }

    /// A fresh cancellable context + its token.
    pub fn new_cancellable() -> (Ctx, CancelToken) {
        let cancel = Arc::new(AtomicBool::new(false));
        (
            Ctx {
                cancels: vec![cancel.clone()],
                deadline: None,
            },
            CancelToken { cancel },
        )
    }

    /// A child context sharing this context's cancel flags, with a deadline
    /// `d` from now (mirror of `context.WithTimeout`).
    pub fn with_timeout(&self, d: Duration) -> Ctx {
        Ctx {
            cancels: self.cancels.clone(),
            deadline: Some(Instant::now() + d),
        }
    }

    /// A child context sharing this context's cancel flags, with no deadline
    /// (mirror of `context.WithCancel` against an existing parent).
    pub fn child(&self) -> Ctx {
        Ctx {
            cancels: self.cancels.clone(),
            deadline: None,
        }
    }

    /// A child context that is cancelled when EITHER this context's flags fire
    /// OR the returned per-child token fires — the analogue of
    /// `context.WithCancel(parent)`. The scheduler uses this so a job ctx
    /// inherits the root cancel flag (shutdown propagates) while still owning a
    /// token that cancels only that one job.
    pub fn child_cancellable(&self) -> (Ctx, CancelToken) {
        let cancel = Arc::new(AtomicBool::new(false));
        let mut cancels = self.cancels.clone();
        cancels.push(cancel.clone());
        (
            Ctx {
                cancels,
                deadline: None,
            },
            CancelToken { cancel },
        )
    }

    /// Returns the reason the context is done, or `None` if still live.
    pub fn done(&self) -> Option<Done> {
        if self.is_cancelled() {
            return Some(Done::Cancelled);
        }
        if let Some(dl) = self.deadline {
            if Instant::now() >= dl {
                return Some(Done::DeadlineExceeded);
            }
        }
        None
    }

    /// True if any cancel token has fired (regardless of deadline).
    pub fn is_cancelled(&self) -> bool {
        self.cancels.iter().any(|c| c.load(Ordering::SeqCst))
    }

    /// The remaining time until the deadline, or `None` when there is no
    /// deadline. A past deadline yields `Some(0)`.
    pub fn remaining(&self) -> Option<Duration> {
        self.deadline
            .map(|dl| dl.saturating_duration_since(Instant::now()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn child_cancellable_inherits_parent_cancel() {
        let (root, root_tok) = Ctx::new_cancellable();
        let (job, _job_tok) = root.child_cancellable();
        assert!(!job.is_cancelled());
        // Cancelling the parent (shutdown) propagates to the job ctx.
        root_tok.cancel();
        assert!(job.is_cancelled());
        assert_eq!(job.done(), Some(Done::Cancelled));
    }

    #[test]
    fn child_cancellable_token_is_independent() {
        let (root, _root_tok) = Ctx::new_cancellable();
        let (job_a, tok_a) = root.child_cancellable();
        let (job_b, _tok_b) = root.child_cancellable();
        // Cancelling one job's token must not cancel a sibling or the root.
        tok_a.cancel();
        assert!(job_a.is_cancelled());
        assert!(!job_b.is_cancelled());
        assert!(!root.is_cancelled());
    }

    #[test]
    fn child_cancellable_inherited_deadline_then_timeout() {
        let (root, _t) = Ctx::new_cancellable();
        let (job, _jt) = root.child_cancellable();
        let turn = job.with_timeout(Duration::from_millis(5));
        std::thread::sleep(Duration::from_millis(15));
        assert_eq!(turn.done(), Some(Done::DeadlineExceeded));
    }
}
