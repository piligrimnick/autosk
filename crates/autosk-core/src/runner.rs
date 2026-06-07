//! The runner abstraction the executor drives — the Rust analogue of the Go
//! `executor.PiRunner` interface. Both the real pi runner ([`crate::pi`]) and
//! the custom Node runner ([`crate::agentnode`]) implement [`PiRunner`].

use std::sync::mpsc::Receiver;
use std::time::Duration;

use crate::ctx::Ctx;
use crate::pi::{Command, Response, SessionInfo};

/// Failure surfaced by a [`PiRunner`] operation. The executor distinguishes
/// [`RunnerError::Cancelled`] (routes through the cancel path, never advances
/// the task) from every other variant (a run error that triggers the
/// defensive signal lookup + kickback/fail handling).
#[derive(Debug, Clone)]
pub enum RunnerError {
    /// The context's cancel token fired.
    Cancelled,
    /// The context's deadline elapsed (the unattached idle-timeout).
    DeadlineExceeded,
    /// The reader exited (stdout EOF) before the awaited event.
    Eof,
    /// The response/turn channel closed before a reply.
    Closed,
    /// [`PiRunner::wait`] hit `grace` before the child exited (non-fatal; the
    /// caller escalates SIGTERM→SIGKILL). Mirror of `pi.IsWaitTimeout`.
    WaitTimeout,
    /// pi answered `success=false` (e.g. a rejected prompt).
    Rejected(String),
    /// Any other I/O / decode failure.
    Io(String),
}

impl RunnerError {
    /// True for [`RunnerError::Cancelled`] (the cancel-path discriminator).
    pub fn is_cancelled(&self) -> bool {
        matches!(self, RunnerError::Cancelled)
    }
    /// True for [`RunnerError::WaitTimeout`].
    pub fn is_wait_timeout(&self) -> bool {
        matches!(self, RunnerError::WaitTimeout)
    }
    /// True for [`RunnerError::DeadlineExceeded`].
    pub fn is_deadline_exceeded(&self) -> bool {
        matches!(self, RunnerError::DeadlineExceeded)
    }
}

impl std::fmt::Display for RunnerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RunnerError::Cancelled => write!(f, "context canceled"),
            RunnerError::DeadlineExceeded => write!(f, "context deadline exceeded"),
            RunnerError::Eof => write!(f, "EOF"),
            RunnerError::Closed => write!(f, "runner closed before reply"),
            RunnerError::WaitTimeout => write!(f, "wait timed out before child exited"),
            RunnerError::Rejected(s) => write!(f, "pi rejected: {s}"),
            RunnerError::Io(s) => write!(f, "{s}"),
        }
    }
}

impl std::error::Error for RunnerError {}

/// One running agent process the executor drives end to end. Must be `Send +
/// Sync` because the executor thread and the daemon's input/abort handlers
/// share the same `Arc<dyn PiRunner>` (the attach surface).
pub trait PiRunner: Send + Sync {
    /// The child's pid, or 0 if gone.
    fn pid(&self) -> i32;
    /// `get_state` → the current [`SessionInfo`].
    fn get_state(&self, ctx: &Ctx) -> Result<SessionInfo, RunnerError>;
    /// Sends a `prompt`; returns when pi acks preflight.
    fn send_prompt(&self, ctx: &Ctx, message: &str) -> Result<(), RunnerError>;
    /// Blocks until the next `agent_end`, ctx is done, or the reader exits.
    fn wait_for_agent_end(&self, ctx: &Ctx) -> Result<(), RunnerError>;
    /// Asks pi to stop the in-flight run.
    fn abort(&self, ctx: &Ctx) -> Result<(), RunnerError>;
    /// Closes stdin, asking the child to shut down cleanly.
    fn close_stdin(&self) -> Result<(), RunnerError>;
    /// Sends SIGTERM.
    fn terminate(&self) -> Result<(), RunnerError>;
    /// Sends SIGKILL.
    fn kill(&self) -> Result<(), RunnerError>;
    /// Waits for the child to exit (bounded by `grace`); returns the exit code.
    fn wait(&self, ctx: &Ctx, grace: Duration) -> (i32, Result<(), RunnerError>);

    // ---- attach surface (the Go `pirunners.RunnerHandle` subset) ----------

    /// Encodes a command onto stdin and returns its response channel. Used by
    /// `job.input` to dispatch prompt/steer/follow_up.
    fn send_command(&self, c: Command) -> Result<Receiver<Response>, RunnerError>;
    /// Whether pi is currently between `agent_start` and `agent_end`.
    fn is_streaming(&self) -> bool;
    /// Whether this runner participates in the attach surface (true for pi,
    /// false for the one-shot Node runner). The executor only registers
    /// attach-capable runners in the live registry.
    fn supports_attach(&self) -> bool {
        true
    }
}
