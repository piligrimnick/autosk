//! `autoskd` library surface — the JSON-RPC server and UDS single-instance
//! binding, exposed so integration tests can drive the server in-process. The
//! binary (`main.rs`) is a thin CLI over these.

pub mod daemon;
pub mod notify;
pub mod server;
pub mod token;
pub mod uds;
