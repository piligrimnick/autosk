//! Local transport: connect to the co-located `autoskd` over a Unix-domain
//! socket, auto-spawning it if absent (plan §4.2 auto-spawn lifecycle, mirror
//! of `internal/daemon/rpcclient/connector.go`).
//!
//! Resolution order for the socket: `$AUTOSK_SOCK` → `~/.autosk/daemon.sock`.
//! Resolution order for the `autoskd` binary: `$AUTOSKD_BIN` → alongside the
//! app executable (where the Tauri sidecar lands) → `autoskd` on `$PATH`.

use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::AtomicU64;
use std::sync::Arc;
use std::time::{Duration, Instant};

use tauri::AppHandle;
use tokio::net::UnixStream;

use super::rpc::{spawn_io, Connection};

const SPAWN_WAIT_TOTAL: Duration = Duration::from_secs(5);
const SPAWN_WAIT_STEP: Duration = Duration::from_millis(25);

/// Connects to the local daemon, spawning it transparently if the socket is
/// absent/stale. autoskd's single-instance binding makes a double-spawn safe.
pub async fn connect(
    app: AppHandle,
    epoch: u64,
    active_epoch: Arc<AtomicU64>,
) -> Result<Connection, String> {
    let sock = resolve_sock()?;
    if let Ok(stream) = UnixStream::connect(&sock).await {
        return Ok(build(app, epoch, active_epoch, stream));
    }
    spawn_daemon(&sock)?;
    let stream = wait_connect(&sock).await?;
    Ok(build(app, epoch, active_epoch, stream))
}

fn build(app: AppHandle, epoch: u64, active_epoch: Arc<AtomicU64>, stream: UnixStream) -> Connection {
    let (reader, writer) = stream.into_split();
    spawn_io(app, "local", epoch, active_epoch, reader, writer)
}

/// `$AUTOSK_SOCK` → `~/.autosk/daemon.sock`.
fn resolve_sock() -> Result<PathBuf, String> {
    if let Ok(s) = std::env::var("AUTOSK_SOCK") {
        if !s.is_empty() {
            return Ok(PathBuf::from(s));
        }
    }
    let home = std::env::var_os("HOME").ok_or_else(|| "HOME not set".to_string())?;
    Ok(PathBuf::from(home).join(".autosk").join("daemon.sock"))
}

/// Spawns `autoskd serve --sock <sock>` detached (new session, stdio to null),
/// so it outlives this GUI process.
fn spawn_daemon(sock: &Path) -> Result<(), String> {
    let bin = locate_daemon()?;
    let mut cmd = Command::new(&bin);
    cmd.arg("serve").arg("--sock").arg(sock);
    cmd.stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    // Detach into a new session so closing the GUI does not SIGHUP the daemon.
    // Safety: setsid() is async-signal-safe and the only call in the hook.
    unsafe {
        cmd.pre_exec(|| {
            // libc::setsid via raw syscall to avoid a libc dependency.
            if detach_session() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    cmd.spawn()
        .map(|_child| ())
        .map_err(|e| format!("spawn autoskd ({}): {e}", bin.display()))
}

/// `$AUTOSKD_BIN` → alongside the app exe (Tauri sidecar location) → `$PATH`.
fn locate_daemon() -> Result<PathBuf, String> {
    if let Ok(p) = std::env::var("AUTOSKD_BIN") {
        if !p.is_empty() {
            return Ok(PathBuf::from(p));
        }
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            for name in ["autoskd", "autoskd.exe"] {
                let cand = dir.join(name);
                if cand.is_file() {
                    return Ok(cand);
                }
            }
            // macOS .app bundles place sidecars in ../Resources alongside MacOS/.
            if let Some(contents) = dir.parent() {
                let cand = contents.join("Resources").join("autoskd");
                if cand.is_file() {
                    return Ok(cand);
                }
            }
        }
    }
    // Fall back to PATH resolution by name.
    Ok(PathBuf::from("autoskd"))
}

/// Retries connecting with bounded backoff until the daemon is up.
async fn wait_connect(sock: &Path) -> Result<UnixStream, String> {
    let deadline = Instant::now() + SPAWN_WAIT_TOTAL;
    let mut last = String::new();
    while Instant::now() < deadline {
        match UnixStream::connect(sock).await {
            Ok(s) => return Ok(s),
            Err(e) => last = e.to_string(),
        }
        tokio::time::sleep(SPAWN_WAIT_STEP).await;
    }
    Err(format!(
        "autoskd did not become ready at {}: {last}",
        sock.display()
    ))
}

/// Calls `setsid(2)` via the platform's raw syscall mechanism. Returns the new
/// session id or -1 on error. Implemented with `libc`-free FFI to keep the
/// dependency surface minimal.
fn detach_session() -> i64 {
    extern "C" {
        fn setsid() -> i32;
    }
    unsafe { setsid() as i64 }
}
