//! Unix-domain-socket single-instance binding — the Rust port of
//! `internal/daemon/uds` (plan §4.2 step 4).
//!
//! [`listen`] ensures the parent dir is `0700`, refuses to take over a socket
//! whose other end accepts connections (a live peer), reaps stale leftovers,
//! binds, and chmods the socket to `0600`. This is what makes the auto-spawn
//! race safe: the loser of a double-spawn gets [`ListenError::AlreadyRunning`]
//! and falls back to connecting to the winner.

use std::io::ErrorKind;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;

/// Why [`listen`] could not bind.
#[derive(Debug)]
pub enum ListenError {
    /// The socket path already has a live peer accepting connections.
    AlreadyRunning,
    /// Any other failure (mkdir / probe / bind / chmod).
    Io(std::io::Error),
}

impl std::fmt::Display for ListenError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ListenError::AlreadyRunning => write!(f, "uds: daemon already running"),
            ListenError::Io(e) => write!(f, "uds: {e}"),
        }
    }
}

impl std::error::Error for ListenError {}

impl From<std::io::Error> for ListenError {
    fn from(e: std::io::Error) -> Self {
        ListenError::Io(e)
    }
}

/// Binds a unix listener at `path` with single-instance semantics:
///
/// 1. create `dir(path)` at mode `0700` (best-effort);
/// 2. if a file exists at `path`, probe it — a successful connect means a live
///    peer ([`ListenError::AlreadyRunning`]); `ECONNREFUSED`/`ENOENT` means a
///    stale leftover, which is unlinked;
/// 3. `bind`, then `chmod 0600`.
pub fn listen(path: &Path) -> Result<UnixListener, ListenError> {
    if let Some(dir) = path.parent() {
        if !dir.as_os_str().is_empty() {
            std::fs::create_dir_all(dir)?;
            let _ = std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700));
        }
    }

    if path.symlink_metadata().is_ok() {
        match UnixStream::connect(path) {
            Ok(c) => {
                drop(c);
                return Err(ListenError::AlreadyRunning);
            }
            Err(e) if is_dead_socket(&e) => match std::fs::remove_file(path) {
                Ok(()) => {}
                Err(e) if e.kind() == ErrorKind::NotFound => {}
                Err(e) => return Err(ListenError::Io(e)),
            },
            Err(e) => return Err(ListenError::Io(e)),
        }
    }

    let listener = UnixListener::bind(path)?;
    if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
        let _ = std::fs::remove_file(path);
        return Err(ListenError::Io(e));
    }
    Ok(listener)
}

/// Removes the socket file (best-effort; `ENOENT` is a no-op).
pub fn cleanup(path: &Path) {
    if let Err(e) = std::fs::remove_file(path) {
        if e.kind() != ErrorKind::NotFound {
            // Nothing actionable on shutdown; the next start reaps it anyway.
        }
    }
}

/// Reports whether a connect error is consistent with a stale leftover (no peer
/// accepting, the file vanished between stat and connect, or the peer closed
/// mid-handshake). Mirrors the Go `isDeadSocketErr`, which reaps
/// `ECONNREFUSED` / `ENOENT` / `ECONNRESET`.
fn is_dead_socket(e: &std::io::Error) -> bool {
    matches!(
        e.kind(),
        ErrorKind::ConnectionRefused | ErrorKind::NotFound | ErrorKind::ConnectionReset
    )
}
