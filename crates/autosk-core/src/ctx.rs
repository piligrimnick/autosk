//! A minimal cancellation + deadline context — the Rust stand-in for the Go
//! `context.Context` the daemon threads through the executor and runners.
//!
//! Go's executor relies on two `context` behaviours: explicit cancellation
//! (the scheduler cancels a job) and per-turn deadlines (the unattached
//! idle-timeout). [`Ctx`] models exactly those. A child [`Ctx`] derived via
//! [`Ctx::with_timeout`] shares the parent's cancellation flag (so cancelling
//! the parent cancels the child) and adds its own deadline.

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
/// cancel flag).
#[derive(Clone)]
pub struct Ctx {
    cancel: Arc<AtomicBool>,
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
            cancel: Arc::new(AtomicBool::new(false)),
            deadline: None,
        }
    }

    /// A fresh cancellable context + its token.
    pub fn new_cancellable() -> (Ctx, CancelToken) {
        let cancel = Arc::new(AtomicBool::new(false));
        (
            Ctx {
                cancel: cancel.clone(),
                deadline: None,
            },
            CancelToken { cancel },
        )
    }

    /// A child context sharing this context's cancel flag, with a deadline
    /// `d` from now (mirror of `context.WithTimeout`).
    pub fn with_timeout(&self, d: Duration) -> Ctx {
        Ctx {
            cancel: self.cancel.clone(),
            deadline: Some(Instant::now() + d),
        }
    }

    /// A child context sharing this context's cancel flag, with no deadline
    /// (mirror of `context.WithCancel` against an existing parent).
    pub fn child(&self) -> Ctx {
        Ctx {
            cancel: self.cancel.clone(),
            deadline: None,
        }
    }

    /// Returns the reason the context is done, or `None` if still live.
    pub fn done(&self) -> Option<Done> {
        if self.cancel.load(Ordering::SeqCst) {
            return Some(Done::Cancelled);
        }
        if let Some(dl) = self.deadline {
            if Instant::now() >= dl {
                return Some(Done::DeadlineExceeded);
            }
        }
        None
    }

    /// True if the cancel token has fired (regardless of deadline).
    pub fn is_cancelled(&self) -> bool {
        self.cancel.load(Ordering::SeqCst)
    }

    /// The remaining time until the deadline, or `None` when there is no
    /// deadline. A past deadline yields `Some(0)`.
    pub fn remaining(&self) -> Option<Duration> {
        self.deadline
            .map(|dl| dl.saturating_duration_since(Instant::now()))
    }
}
