//! `autoskd` library surface — the JSON-RPC server and UDS single-instance
//! binding, exposed so integration tests can drive the server in-process. The
//! binary (`main.rs`) is a thin CLI over these.

pub mod server;
pub mod uds;
