//! Shared JSON-RPC framing over a persistent connection (plan §4.1, §6).
//!
//! One line-delimited JSON object per message. A single long-lived connection
//! carries BOTH request/response traffic AND server→client notifications: a
//! reader task demuxes by shape (`id` ⇒ response, `method` ⇒ notification) and
//! re-emits every notification verbatim as a Tauri event
//! (`app.emit("<same-name>", params)`), so the React layer is oblivious to
//! local-vs-remote (the CodexMonitor trick). Used identically by the local
//! (UDS) and remote (TCP) transports.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use tauri::{AppHandle, Emitter};
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};
use tokio::sync::{mpsc, oneshot, Mutex};
use tokio::time::timeout;

/// Sentinel returned when the connection has dropped; the command layer maps
/// this to a single transparent reconnect+retry.
pub const DISCONNECTED: &str = "daemon connection closed";

const OUTBOUND_CAPACITY: usize = 512;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(300);
const SEND_TIMEOUT: Duration = Duration::from_secs(15);

type PendingMap = HashMap<u64, oneshot::Sender<Result<Value, String>>>;

/// A live JSON-RPC connection. Cheap to clone (shared inner state).
#[derive(Clone)]
pub struct Connection {
    /// Unique generation id (see `AppState::active_epoch`): lets a superseded
    /// connection's reader suppress its EOF `daemon-status:false`.
    pub epoch: u64,
    out_tx: mpsc::Sender<String>,
    pending: Arc<Mutex<PendingMap>>,
    next_id: Arc<AtomicU64>,
    connected: Arc<AtomicBool>,
}

impl Connection {
    pub fn is_connected(&self) -> bool {
        self.connected.load(Ordering::SeqCst)
    }

    /// Sends one request and awaits its response. `Err(DISCONNECTED)` signals a
    /// dropped connection; daemon-level errors come back as a JSON-encoded
    /// `{code,message,details}` string the frontend parses.
    pub async fn call(&self, method: &str, params: Value) -> Result<Value, String> {
        if !self.is_connected() {
            return Err(DISCONNECTED.to_string());
        }
        let id = self.next_id.fetch_add(1, Ordering::SeqCst);
        let (tx, rx) = oneshot::channel();
        self.pending.lock().await.insert(id, tx);

        let line = serde_json::to_string(&json!({ "id": id, "method": method, "params": params }))
            .map_err(|e| e.to_string())?;
        match timeout(SEND_TIMEOUT, self.out_tx.send(line)).await {
            Ok(Ok(())) => {}
            _ => {
                self.pending.lock().await.remove(&id);
                return Err(DISCONNECTED.to_string());
            }
        }
        match timeout(REQUEST_TIMEOUT, rx).await {
            Ok(Ok(result)) => result,
            Ok(Err(_)) => Err(DISCONNECTED.to_string()),
            Err(_) => {
                self.pending.lock().await.remove(&id);
                Err(format!(
                    "daemon request timed out after {}s",
                    REQUEST_TIMEOUT.as_secs()
                ))
            }
        }
    }
}

/// Splits a duplex stream into reader+writer tasks and returns a [`Connection`].
/// `mode` ("local"|"remote") tags the `daemon-status` event emitted on
/// disconnect so the UI can show which transport dropped. `epoch` is this
/// connection's generation id and `active_epoch` the shared cell holding the id
/// of the currently-installed connection: the reader emits its EOF disconnect
/// ONLY while the two still match, so a superseded connection stays silent.
pub fn spawn_io<R, W>(
    app: AppHandle,
    mode: &str,
    epoch: u64,
    active_epoch: Arc<AtomicU64>,
    reader: R,
    mut writer: W,
) -> Connection
where
    R: AsyncRead + Unpin + Send + 'static,
    W: AsyncWrite + Unpin + Send + 'static,
{
    let (out_tx, mut out_rx) = mpsc::channel::<String>(OUTBOUND_CAPACITY);
    let pending = Arc::new(Mutex::new(PendingMap::new()));
    let connected = Arc::new(AtomicBool::new(true));

    // Writer task: drain the outbound queue to the socket.
    let pending_w = Arc::clone(&pending);
    let connected_w = Arc::clone(&connected);
    tauri::async_runtime::spawn(async move {
        while let Some(msg) = out_rx.recv().await {
            if writer.write_all(msg.as_bytes()).await.is_err()
                || writer.write_all(b"\n").await.is_err()
                || writer.flush().await.is_err()
            {
                mark_disconnected(&pending_w, &connected_w).await;
                break;
            }
        }
    });

    // Reader task: demux responses vs notifications; re-emit notifications.
    let pending_r = Arc::clone(&pending);
    let connected_r = Arc::clone(&connected);
    let mode_owned = mode.to_string();
    tauri::async_runtime::spawn(async move {
        let mut lines = BufReader::new(reader).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            let trimmed = line.trim();
            if trimmed.is_empty() {
                continue;
            }
            dispatch_line(&app, &pending_r, trimmed).await;
        }
        mark_disconnected(&pending_r, &connected_r).await;
        // Announce the disconnect ONLY if this is still the active connection.
        // A proactively-replaced connection had its epoch retired before the
        // drop, so this guard suppresses the stray `false` that could otherwise
        // land after the replacement's `true` (review R3 #1).
        if announce_disconnect(&active_epoch, epoch) {
            let _ = app.emit(
                "daemon-status",
                json!({ "connected": false, "mode": mode_owned }),
            );
        }
    });

    Connection {
        epoch,
        out_tx,
        pending,
        next_id: Arc::new(AtomicU64::new(1)),
        connected,
    }
}

/// Whether a reader at generation `epoch` may announce its EOF disconnect: only
/// while it is still the active connection (a retired/superseded epoch is
/// silent). Pure so the suppression contract is unit-testable.
fn announce_disconnect(active_epoch: &AtomicU64, epoch: u64) -> bool {
    active_epoch.load(Ordering::SeqCst) == epoch
}

/// One classified inbound message. Pure projection of a wire line so the demux
/// contract is unit-testable without an `AppHandle` (see tests).
#[derive(Debug)]
enum Inbound {
    /// Server→client notification — re-emit verbatim under `method`.
    Notification { method: String, params: Value },
    /// Response to a pending request `id`.
    Response {
        id: u64,
        outcome: Result<Value, String>,
    },
    /// Unparseable / shapeless line — drop it.
    Ignore,
}

/// Classifies one inbound line: a `method` field ⇒ notification; otherwise an
/// `id` field ⇒ response (an `error` object becomes a JSON-encoded string the
/// frontend parses back into `{code, message, details}`).
fn classify_line(line: &str) -> Inbound {
    let Ok(value) = serde_json::from_str::<Value>(line) else {
        return Inbound::Ignore;
    };
    if let Some(method) = value.get("method").and_then(Value::as_str) {
        let params = value.get("params").cloned().unwrap_or(Value::Null);
        return Inbound::Notification {
            method: method.to_string(),
            params,
        };
    }
    let Some(id) = value.get("id").and_then(Value::as_u64) else {
        return Inbound::Ignore;
    };
    let outcome = if let Some(err) = value.get("error") {
        Err(serde_json::to_string(err).unwrap_or_else(|_| "daemon error".to_string()))
    } else {
        Ok(value.get("result").cloned().unwrap_or(Value::Null))
    };
    Inbound::Response { id, outcome }
}

/// Routes one inbound line: a notification is re-emitted under the same name so
/// the events.ts hub fans it out (session-event / task-changed /
/// project-changed); a response resolves the pending oneshot.
async fn dispatch_line(app: &AppHandle, pending: &Arc<Mutex<PendingMap>>, line: &str) {
    match classify_line(line) {
        Inbound::Notification { method, params } => {
            let _ = app.emit(&method, params);
        }
        Inbound::Response { id, outcome } => {
            if let Some(tx) = pending.lock().await.remove(&id) {
                let _ = tx.send(outcome);
            }
        }
        Inbound::Ignore => {}
    }
}

async fn mark_disconnected(pending: &Arc<Mutex<PendingMap>>, connected: &Arc<AtomicBool>) {
    connected.store(false, Ordering::SeqCst);
    let mut map = pending.lock().await;
    for (_, tx) in map.drain() {
        let _ = tx.send(Err(DISCONNECTED.to_string()));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classify_response_result() {
        match classify_line(r#"{"id":7,"result":{"ok":true}}"#) {
            Inbound::Response { id, outcome } => {
                assert_eq!(id, 7);
                assert_eq!(outcome.unwrap(), json!({ "ok": true }));
            }
            other => panic!("expected response, got {other:?}"),
        }
    }

    #[test]
    fn classify_response_missing_result_is_null() {
        match classify_line(r#"{"id":3}"#) {
            Inbound::Response { id, outcome } => {
                assert_eq!(id, 3);
                assert_eq!(outcome.unwrap(), Value::Null);
            }
            other => panic!("expected response, got {other:?}"),
        }
    }

    #[test]
    fn classify_error_is_forwarded_as_json_string() {
        match classify_line(r#"{"id":9,"error":{"code":-32601,"message":"nope"}}"#) {
            Inbound::Response { id, outcome } => {
                assert_eq!(id, 9);
                let err = outcome.unwrap_err();
                // Round-trips into the structured object the frontend parses.
                let parsed: Value = serde_json::from_str(&err).unwrap();
                assert_eq!(parsed["code"], -32601);
                assert_eq!(parsed["message"], "nope");
            }
            other => panic!("expected response, got {other:?}"),
        }
    }

    #[test]
    fn classify_notification() {
        match classify_line(r#"{"method":"session-event","params":{"session_id":"s1"}}"#) {
            Inbound::Notification { method, params } => {
                assert_eq!(method, "session-event");
                assert_eq!(params["session_id"], "s1");
            }
            other => panic!("expected notification, got {other:?}"),
        }
    }

    #[test]
    fn classify_notification_without_params_is_null() {
        match classify_line(r#"{"method":"project-changed"}"#) {
            Inbound::Notification { method, params } => {
                assert_eq!(method, "project-changed");
                assert_eq!(params, Value::Null);
            }
            other => panic!("expected notification, got {other:?}"),
        }
    }

    #[test]
    fn classify_garbage_and_shapeless_are_ignored() {
        assert!(matches!(classify_line("not json"), Inbound::Ignore));
        assert!(matches!(classify_line(r#"{"foo":1}"#), Inbound::Ignore));
        // A non-numeric id has no pending slot to resolve.
        assert!(matches!(classify_line(r#"{"id":"x"}"#), Inbound::Ignore));
    }

    #[test]
    fn announce_disconnect_only_for_the_active_epoch() {
        let active = AtomicU64::new(5);
        // The currently-installed connection may announce its disconnect.
        assert!(announce_disconnect(&active, 5));
        // A superseded connection (different epoch) stays silent.
        assert!(!announce_disconnect(&active, 4));
        // A retired epoch (0 = none) also suppresses any reader.
        active.store(0, Ordering::SeqCst);
        assert!(!announce_disconnect(&active, 5));
    }

    #[tokio::test]
    async fn mark_disconnected_drains_pending_and_flips_flag() {
        let pending: Arc<Mutex<PendingMap>> = Arc::new(Mutex::new(PendingMap::new()));
        let connected = Arc::new(AtomicBool::new(true));
        let (tx, rx) = oneshot::channel::<Result<Value, String>>();
        pending.lock().await.insert(1, tx);

        mark_disconnected(&pending, &connected).await;

        assert!(!connected.load(Ordering::SeqCst));
        assert!(pending.lock().await.is_empty());
        assert_eq!(rx.await.unwrap(), Err(DISCONNECTED.to_string()));
    }
}
