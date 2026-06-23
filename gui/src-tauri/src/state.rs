//! Shared backend state: the current settings + the live daemon connection.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use serde::Serialize;
use tokio::sync::Mutex;

use crate::client::Connection;
use crate::settings::{AppSettings, BackendMode};

pub struct AppState {
    pub settings: Mutex<AppSettings>,
    pub connection: Mutex<Option<Connection>>,
    /// Single-flight guard: serializes connect attempts so concurrent first
    /// requests (setup + bootstrap, or StrictMode's double bootstrap) build at
    /// most one connection / spawn at most one daemon per transition.
    pub connect_lock: Mutex<()>,
    /// Monotonic source of per-connection epochs (each connection gets a unique,
    /// never-zero id).
    next_epoch: AtomicU64,
    /// Epoch of the currently-installed connection (`0` = none / superseded).
    /// Shared with every connection's reader task so that, on EOF, a reader
    /// announces the disconnect ONLY while it is still the active connection.
    /// A proactively-replaced connection (Reconnect button / mode switch) has
    /// its epoch retired BEFORE it is dropped, so its late EOF stays silent —
    /// otherwise that stray `daemon-status:false` could land after the
    /// replacement's `true`, stranding the badge on "disconnected" and freezing
    /// the live tail (review R3 #1).
    active_epoch: Arc<AtomicU64>,
}

impl AppState {
    pub fn new(settings: AppSettings) -> AppState {
        AppState {
            settings: Mutex::new(settings),
            connection: Mutex::new(None),
            connect_lock: Mutex::new(()),
            next_epoch: AtomicU64::new(1),
            active_epoch: Arc::new(AtomicU64::new(0)),
        }
    }

    /// Allocates the next unique (non-zero) connection epoch.
    pub fn alloc_epoch(&self) -> u64 {
        self.next_epoch.fetch_add(1, Ordering::SeqCst)
    }

    /// A clone of the shared active-epoch cell to hand to a connection's reader.
    pub fn active_epoch_handle(&self) -> Arc<AtomicU64> {
        Arc::clone(&self.active_epoch)
    }

    /// Marks `epoch` as the live connection (called when installing it).
    pub fn set_active_epoch(&self, epoch: u64) {
        self.active_epoch.store(epoch, Ordering::SeqCst);
    }

    /// Retires `epoch` (-> `0`) IFF it is still the active one. Call before
    /// proactively dropping a connection so its reader suppresses the EOF
    /// `false`. The compare-and-swap makes this safe against a concurrent
    /// connect that already installed a newer connection: if `active_epoch` has
    /// moved on, this is a no-op and the fresh connection stays valid.
    pub fn retire_epoch(&self, epoch: u64) {
        let _ = self
            .active_epoch
            .compare_exchange(epoch, 0, Ordering::SeqCst, Ordering::SeqCst);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::settings::AppSettings;

    fn state() -> AppState {
        AppState::new(AppSettings::default())
    }

    #[test]
    fn alloc_epoch_is_monotonic_and_nonzero() {
        let s = state();
        let a = s.alloc_epoch();
        let b = s.alloc_epoch();
        assert!(a >= 1 && b > a, "epochs must be non-zero and increasing");
    }

    #[test]
    fn set_and_retire_active_epoch() {
        let s = state();
        let handle = s.active_epoch_handle();
        assert_eq!(handle.load(Ordering::SeqCst), 0, "starts with no active conn");

        let e = s.alloc_epoch();
        s.set_active_epoch(e);
        assert_eq!(handle.load(Ordering::SeqCst), e);

        // Retiring the live epoch clears it (reader will suppress its EOF false).
        s.retire_epoch(e);
        assert_eq!(handle.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn retire_is_a_noop_when_a_newer_connection_is_active() {
        // A stale drop (e.g. a late DISCONNECTED retry) must not clobber the
        // epoch of a fresh connection installed by a concurrent connect.
        let s = state();
        let old = s.alloc_epoch();
        let new = s.alloc_epoch();
        s.set_active_epoch(new);
        s.retire_epoch(old); // old != active -> CAS fails
        assert_eq!(
            s.active_epoch_handle().load(Ordering::SeqCst),
            new,
            "fresh connection's epoch must survive a stale retire"
        );
    }
}

/// The connection status surfaced to the UI (mirrors `DaemonStatus` in types.ts).
#[derive(Clone, Serialize)]
pub struct DaemonStatus {
    pub connected: bool,
    pub mode: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

impl DaemonStatus {
    pub fn new(connected: bool, mode: BackendMode, error: Option<String>) -> DaemonStatus {
        DaemonStatus {
            connected,
            mode: mode.as_str(),
            error,
        }
    }
}
