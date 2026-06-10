//! Tauri commands — the local/remote chokepoint (plan §6). `daemon_request` is
//! the single generic forwarder: `if remote { remote::connect } else {
//! local::connect }`, then `connection.call(method, params)`. Notifications flow
//! back through the persistent connection's reader (client::rpc) which re-emits
//! them as Tauri events, so the frontend is transport-agnostic. The remaining
//! commands manage app settings + the connection lifecycle.

use serde_json::Value;
use tauri::{AppHandle, Emitter, State};

use crate::client::{local, remote, Connection, DISCONNECTED};
use crate::settings::{self, AppSettings, BackendMode};
use crate::state::{AppState, DaemonStatus};

/// Returns the live connection, establishing it (per the configured mode) and
/// subscribing to change notifications if needed.
pub(crate) async fn ensure_connection(
    app: &AppHandle,
    state: &AppState,
) -> Result<Connection, String> {
    // Fast path: an already-live connection needs no lock dance.
    {
        let guard = state.connection.lock().await;
        if let Some(conn) = guard.as_ref() {
            if conn.is_connected() {
                return Ok(conn.clone());
            }
        }
    }

    // Single-flight: serialize the connect+subscribe+store across concurrent
    // callers (setup/bootstrap race, StrictMode double bootstrap, the
    // DISCONNECTED retry path). Without this each racer would dial / spawn a
    // daemon and register a duplicate subscription, multiplying every change
    // notification until the loser connections drop.
    let _connect_guard = state.connect_lock.lock().await;
    // Double-check: a peer may have established the connection while we waited.
    {
        let guard = state.connection.lock().await;
        if let Some(conn) = guard.as_ref() {
            if conn.is_connected() {
                return Ok(conn.clone());
            }
        }
    }

    let settings = state.settings.lock().await.clone();
    let epoch = state.alloc_epoch();
    let conn = match settings.backend_mode {
        BackendMode::Local => local::connect(app.clone(), epoch, state.active_epoch_handle()).await?,
        BackendMode::Remote => {
            remote::connect(
                app.clone(),
                &settings.remote_host,
                &settings.remote_token,
                epoch,
                state.active_epoch_handle(),
            )
            .await?
        }
    };

    // Opt into the server's change pushes for this connection so task-changed /
    // project-changed flow without the frontend knowing the transport (plan §5).
    let _ = conn.call("task.subscribe", serde_json::json!({})).await;
    let _ = conn.call("project.subscribe", serde_json::json!({})).await;

    // Mark this generation active BEFORE emitting `true`: from here on any
    // superseded reader that EOFs sees a non-matching epoch and stays silent.
    state.set_active_epoch(epoch);
    *state.connection.lock().await = Some(conn.clone());
    let _ = app.emit(
        "daemon-status",
        DaemonStatus::new(true, settings.backend_mode, None),
    );
    Ok(conn)
}

/// The generic JSON-RPC forwarder. One call site for every autoskd method.
#[tauri::command]
pub async fn daemon_request(
    app: AppHandle,
    state: State<'_, AppState>,
    method: String,
    params: Value,
) -> Result<Value, String> {
    let conn = ensure_connection(&app, &state).await?;
    match conn.call(&method, params.clone()).await {
        Ok(value) => Ok(value),
        Err(err) if err == DISCONNECTED => {
            // Transparent reconnect + single retry (mirrors the Go client's
            // per-call dial). A still-failing call surfaces the error. Only
            // tear down state if `conn` is still the installed connection — a
            // concurrent caller may already have replaced it with a fresh one,
            // in which case we must not clobber it (just retry below).
            let superseded = {
                let mut guard = state.connection.lock().await;
                if guard.as_ref().map(|c| c.epoch) == Some(conn.epoch) {
                    *guard = None;
                    true
                } else {
                    false
                }
            };
            if superseded {
                state.retire_epoch(conn.epoch);
                let mode = state.settings.lock().await.backend_mode;
                let _ = app.emit("daemon-status", DaemonStatus::new(false, mode, None));
            }
            let conn = ensure_connection(&app, &state).await?;
            conn.call(&method, params).await
        }
        Err(err) => Err(err),
    }
}

#[tauri::command]
pub async fn get_app_settings(state: State<'_, AppState>) -> Result<AppSettings, String> {
    Ok(state.settings.lock().await.clone())
}

#[tauri::command]
pub async fn update_app_settings(
    app: AppHandle,
    state: State<'_, AppState>,
    settings: AppSettings,
) -> Result<AppSettings, String> {
    // Apply in-memory FIRST: the new settings must take effect for this session
    // even when persisting them fails (e.g. a read-only sandbox path on mobile);
    // a persist failure must not strand the app on the old host/token.
    *state.settings.lock().await = settings.clone();
    // Drop the old connection so the next request reconnects with the new mode.
    // Retire its epoch BEFORE the drop (held in `old` until end of scope) so its
    // late EOF — a slow remote socket can EOF after the fast new local `true` —
    // stays silent and can't strand the badge / freeze the tail (review R3 #1).
    let old = state.connection.lock().await.take();
    if let Some(conn) = old.as_ref() {
        state.retire_epoch(conn.epoch);
        let _ = app.emit(
            "daemon-status",
            DaemonStatus::new(false, settings.backend_mode, None),
        );
    }
    drop(old);
    // Persist last; surface the failure without undoing the in-memory update.
    settings::save(&settings)
        .map_err(|e| format!("settings applied for this session, but persisting failed: {e}"))?;
    Ok(settings)
}

#[tauri::command]
pub async fn reconnect_daemon(
    app: AppHandle,
    state: State<'_, AppState>,
) -> Result<DaemonStatus, String> {
    // Retire the old epoch before the drop so a proactively-replaced (still
    // LIVE) connection's late EOF stays silent — otherwise its stray `false`
    // could land after the new connection's `true` (review R3 #1). This makes
    // "Reconnect while connected" safe.
    let old = state.connection.lock().await.take();
    if let Some(conn) = old.as_ref() {
        state.retire_epoch(conn.epoch);
    }
    drop(old);
    let mode = state.settings.lock().await.backend_mode;
    match ensure_connection(&app, &state).await {
        Ok(_) => Ok(DaemonStatus::new(true, mode, None)),
        Err(e) => {
            let status = DaemonStatus::new(false, mode, Some(e));
            let _ = app.emit("daemon-status", status.clone());
            Ok(status)
        }
    }
}

#[tauri::command]
pub async fn daemon_status(state: State<'_, AppState>) -> Result<DaemonStatus, String> {
    let mode = state.settings.lock().await.backend_mode;
    let connected = state
        .connection
        .lock()
        .await
        .as_ref()
        .map(|c| c.is_connected())
        .unwrap_or(false);
    Ok(DaemonStatus::new(connected, mode, None))
}
