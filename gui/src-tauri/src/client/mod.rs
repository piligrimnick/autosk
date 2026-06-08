//! JSON-RPC client for autoskd — the transport-agnostic core of the Tauri
//! backend (plan §6). `rpc` is the shared framing; `local`/`remote` are the two
//! transports the `daemon_request` command switches between.

pub mod local;
pub mod remote;
pub mod rpc;

pub use rpc::{Connection, DISCONNECTED};
