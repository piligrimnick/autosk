//! Remote transport: dial a configured `host:port` running `autoskd --tcp` and
//! authenticate with the token before any other request (plan §4.1 TCP auth
//! handshake). Behaviour is otherwise identical to local — the frontend never
//! knows the difference.

use std::sync::atomic::AtomicU64;
use std::sync::Arc;

use tauri::AppHandle;
use tokio::net::TcpStream;

use super::rpc::{spawn_io, Connection};

/// Connects to a remote daemon and performs the `auth{token}` handshake. The
/// daemon rejects every other request on a TCP connection until `auth` succeeds.
pub async fn connect(
    app: AppHandle,
    host: &str,
    token: &str,
    epoch: u64,
    active_epoch: Arc<AtomicU64>,
) -> Result<Connection, String> {
    let host = host.trim();
    if host.is_empty() {
        return Err("remote host is not configured".to_string());
    }
    let stream = TcpStream::connect(host)
        .await
        .map_err(|e| format!("connect {host}: {e}"))?;
    // TCP_NODELAY: line-delimited RPC is latency-sensitive, not throughput-bound.
    let _ = stream.set_nodelay(true);
    let (reader, writer) = stream.into_split();
    let conn = spawn_io(app, "remote", epoch, active_epoch, reader, writer);

    // Authenticate first; the handshake must precede any project-scoped call.
    conn.call("auth", serde_json::json!({ "token": token }))
        .await
        .map_err(|e| format!("auth failed: {e}"))?;
    Ok(conn)
}
