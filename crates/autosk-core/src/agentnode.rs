//! Node bootstrapper runner for custom-runner agent packages — the Rust port
//! of `internal/daemon/agentnode`.
//!
//! The executor talks to this through the [`PiRunner`] trait, but the child is
//! a one-shot Node process: [`PiRunner::send_prompt`] writes the JSON
//! `RunContextSeed` to stdin and closes it; [`PiRunner::wait_for_agent_end`]
//! blocks until the process exits. There are no per-turn events and no pi
//! session, so the executor forces `max_corrections = 0` for these runs.

use std::io::Write;
use std::process::{ChildStdin, Command as PCommand, Stdio};
use std::sync::mpsc::Receiver;
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use crate::ctx::{Ctx, Done};
use crate::pi::{Command, Response, SessionInfo};
use crate::runner::{PiRunner, RunnerError};

const POLL: Duration = Duration::from_millis(10);

/// Options for [`spawn`] (mirror of `agentnode.Opts`).
#[derive(Debug, Clone, Default)]
pub struct NodeOpts {
    pub node_bin: String,
    pub bootstrap_path: String,
    pub package_name: String,
    pub runner_path: String,
    pub cwd: String,
    pub env: Vec<(String, String)>,
    /// Adds `--import tsx` so the bootstrapper can require `.ts` files.
    pub use_tsx_loader: bool,
}

struct Shared {
    stdin: Mutex<Option<ChildStdin>>,
    stderr: Mutex<Vec<u8>>,
    exit: Mutex<Option<i32>>,
    exit_cv: Condvar,
    exit_err: Mutex<Option<String>>,
}

/// One Node bootstrapper subprocess.
pub struct Runner {
    pid: i32,
    shared: Arc<Shared>,
}

/// Spawns a Node child running the bootstrapper.
pub fn spawn(_ctx: &Ctx, opts: NodeOpts) -> Result<Runner, RunnerError> {
    if opts.bootstrap_path.is_empty() {
        return Err(RunnerError::Io(
            "agentnode.spawn: empty bootstrap_path".into(),
        ));
    }
    if opts.package_name.is_empty() {
        return Err(RunnerError::Io(
            "agentnode.spawn: empty package_name".into(),
        ));
    }
    if opts.runner_path.is_empty() {
        return Err(RunnerError::Io("agentnode.spawn: empty runner_path".into()));
    }
    if !std::path::Path::new(&opts.bootstrap_path).exists() {
        return Err(RunnerError::Io(format!(
            "agentnode.spawn: bootstrap missing at {}",
            opts.bootstrap_path
        )));
    }
    let bin = if opts.node_bin.is_empty() {
        "node".to_string()
    } else {
        opts.node_bin.clone()
    };
    let mut args: Vec<String> = Vec::new();
    if opts.use_tsx_loader {
        args.push("--import".into());
        args.push("tsx".into());
    }
    args.push(opts.bootstrap_path.clone());
    args.push("--pkg".into());
    args.push(opts.package_name.clone());
    args.push("--runner".into());
    args.push(opts.runner_path.clone());

    let mut cmd = PCommand::new(&bin);
    cmd.args(&args)
        .stdin(Stdio::piped())
        .stdout(Stdio::inherit())
        .stderr(Stdio::piped());
    if !opts.cwd.is_empty() {
        if !std::path::Path::new(&opts.cwd).is_absolute() {
            return Err(RunnerError::Io(format!(
                "agentnode.spawn: cwd must be absolute, got {:?}",
                opts.cwd
            )));
        }
        cmd.current_dir(&opts.cwd);
    }
    if !opts.env.is_empty() {
        cmd.env_clear();
        for (k, v) in &opts.env {
            cmd.env(k, v);
        }
    }
    let mut child = cmd
        .spawn()
        .map_err(|e| RunnerError::Io(format!("start {bin}: {e}")))?;
    let pid = child.id() as i32;
    let stdin = child.stdin.take();
    let stderr = child.stderr.take();

    let shared = Arc::new(Shared {
        stdin: Mutex::new(stdin),
        stderr: Mutex::new(Vec::new()),
        exit: Mutex::new(None),
        exit_cv: Condvar::new(),
        exit_err: Mutex::new(None),
    });

    if let Some(mut err_pipe) = stderr {
        let s = Arc::clone(&shared);
        std::thread::spawn(move || {
            use std::io::Read;
            let mut buf = [0u8; 4096];
            loop {
                match err_pipe.read(&mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => s.stderr.lock().unwrap().extend_from_slice(&buf[..n]),
                }
            }
        });
    }

    let reaper = Arc::clone(&shared);
    std::thread::spawn(move || {
        let res = child.wait();
        let code = res.as_ref().ok().and_then(|s| s.code()).unwrap_or(-1);
        if code != 0 {
            *reaper.exit_err.lock().unwrap() = Some(format!("runner exited with code {code}"));
        }
        let mut g = reaper.exit.lock().unwrap();
        *g = Some(code);
        reaper.exit_cv.notify_all();
    });

    Ok(Runner { pid, shared })
}

impl Runner {
    fn stderr_snapshot(&self) -> String {
        let b = self.shared.stderr.lock().unwrap();
        let start = b.len().saturating_sub(4096);
        String::from_utf8_lossy(&b[start..]).to_string()
    }
}

impl PiRunner for Runner {
    fn pid(&self) -> i32 {
        self.pid
    }

    fn get_state(&self, _ctx: &Ctx) -> Result<SessionInfo, RunnerError> {
        Ok(SessionInfo::default())
    }

    fn send_prompt(&self, _ctx: &Ctx, payload: &str) -> Result<(), RunnerError> {
        let mut g = self.shared.stdin.lock().unwrap();
        let Some(w) = g.as_mut() else {
            return Ok(());
        };
        if let Err(e) = w.write_all(payload.as_bytes()) {
            return Err(RunnerError::Io(format!("write seed: {e}")));
        }
        // Close so the bootstrapper sees EOF and finishes reading.
        *g = None;
        Ok(())
    }

    fn wait_for_agent_end(&self, ctx: &Ctx) -> Result<(), RunnerError> {
        let mut g = self.shared.exit.lock().unwrap();
        loop {
            if g.is_some() {
                if let Some(e) = self.shared.exit_err.lock().unwrap().clone() {
                    return Err(RunnerError::Io(format!(
                        "{e} (stderr: {})",
                        self.stderr_snapshot()
                    )));
                }
                return Ok(());
            }
            if let Some(d) = ctx.done() {
                return Err(match d {
                    Done::Cancelled => RunnerError::Cancelled,
                    Done::DeadlineExceeded => RunnerError::DeadlineExceeded,
                });
            }
            let (ng, _) = self.shared.exit_cv.wait_timeout(g, POLL).unwrap();
            g = ng;
        }
    }

    fn abort(&self, _ctx: &Ctx) -> Result<(), RunnerError> {
        let _ = self.close_stdin();
        self.terminate()
    }

    fn close_stdin(&self) -> Result<(), RunnerError> {
        *self.shared.stdin.lock().unwrap() = None;
        Ok(())
    }

    fn terminate(&self) -> Result<(), RunnerError> {
        if self.pid > 0 {
            unsafe {
                libc::kill(self.pid, libc::SIGTERM);
            }
        }
        Ok(())
    }

    fn kill(&self) -> Result<(), RunnerError> {
        if self.pid > 0 {
            unsafe {
                libc::kill(self.pid, libc::SIGKILL);
            }
        }
        Ok(())
    }

    fn wait(&self, ctx: &Ctx, grace: Duration) -> (i32, Result<(), RunnerError>) {
        let grace = if grace.is_zero() {
            Duration::from_secs(1)
        } else {
            grace
        };
        let deadline = Instant::now() + grace;
        let mut g = self.shared.exit.lock().unwrap();
        loop {
            if let Some(code) = *g {
                return (code, Ok(()));
            }
            if let Some(d) = ctx.done() {
                return (
                    -1,
                    Err(match d {
                        Done::Cancelled => RunnerError::Cancelled,
                        Done::DeadlineExceeded => RunnerError::DeadlineExceeded,
                    }),
                );
            }
            let now = Instant::now();
            if now >= deadline {
                return (-1, Err(RunnerError::WaitTimeout));
            }
            let wait = POLL.min(deadline - now);
            let (ng, _) = self.shared.exit_cv.wait_timeout(g, wait).unwrap();
            g = ng;
        }
    }

    fn send_command(&self, _c: Command) -> Result<Receiver<Response>, RunnerError> {
        Err(RunnerError::Io(
            "node runner does not support commands".into(),
        ))
    }

    fn is_streaming(&self) -> bool {
        false
    }

    fn supports_attach(&self) -> bool {
        false
    }
}
