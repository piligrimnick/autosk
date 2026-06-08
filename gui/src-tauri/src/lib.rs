//! autosk GUI — Tauri backend entrypoint (plan §6). A thin JSON-RPC client of
//! autoskd: `generate_handler![...]` registers the generic `daemon_request`
//! forwarder + the settings/connection commands; the local/remote switch and
//! notification re-emission live in `commands` / `client`.

mod client;
mod commands;
mod settings;
mod state;

use crate::commands::{
    daemon_request, daemon_status, get_app_settings, reconnect_daemon, update_app_settings,
};
use crate::settings::AppSettings;
use crate::state::AppState;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let loaded: AppSettings = settings::load();

    tauri::Builder::default()
        .manage(AppState::new(loaded))
        .invoke_handler(tauri::generate_handler![
            daemon_request,
            get_app_settings,
            update_app_settings,
            reconnect_daemon,
            daemon_status,
        ])
        // No background pre-connect: the frontend's bootstrap() issues its first
        // daemon_request (project.list) immediately, which lazily establishes
        // the connection through the single-flight `ensure_connection`. Dropping
        // the setup() racer removes a guaranteed concurrent connect at startup.
        .run(tauri::generate_context!())
        .expect("error while running autosk GUI");
}
