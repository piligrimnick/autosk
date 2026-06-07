//! JSON-RPC envelope (plan §4.1).
//!
//! Line-delimited JSON over UDS (local) and, later, TCP (remote). One JSON
//! object per line:
//!
//! * Request: `{"id":<u64>,"method":"<string>","params":<object|null>}`
//! * Response: `{"id":<u64>,"result":<any>}` | `{"id":<u64>,"error":{…}}`
//! * Notification (server→client): `{"method":"<string>","params":<object>}`

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// A client→server request.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Request {
    pub id: u64,
    pub method: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub params: Option<Value>,
}

/// A server→client response. Exactly one of `result` / `error` is set.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Response {
    pub id: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<ErrorObject>,
}

impl Response {
    /// Builds a success response carrying `result`.
    pub fn ok(id: u64, result: Value) -> Response {
        Response {
            id,
            result: Some(result),
            error: None,
        }
    }

    /// Builds an error response.
    pub fn err(id: u64, code: i64, message: impl Into<String>) -> Response {
        Response {
            id,
            result: None,
            error: Some(ErrorObject {
                code,
                message: message.into(),
                details: None,
            }),
        }
    }
}

/// The error payload of a failed response.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ErrorObject {
    pub code: i64,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub details: Option<Value>,
}

/// A server→client notification (no id, no response expected).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Notification {
    pub method: String,
    pub params: Value,
}

/// JSON-RPC error codes used by autoskd. The standard JSON-RPC reserved range
/// is kept for protocol-level failures; the application range (`1xxx`) carries
/// the domain errors the Go side maps to 4xx today.
pub mod error_codes {
    /// Malformed request envelope (not valid JSON / missing fields).
    pub const PARSE_ERROR: i64 = -32700;
    /// Request object is not a valid Request.
    pub const INVALID_REQUEST: i64 = -32600;
    /// Unknown method.
    pub const METHOD_NOT_FOUND: i64 = -32601;
    /// Invalid params for an otherwise-known method.
    pub const INVALID_PARAMS: i64 = -32602;
    /// Catch-all server-side failure.
    pub const INTERNAL_ERROR: i64 = -32603;

    /// The project selector did not resolve to an `.autosk/db` (mirror of the
    /// Go `projectmgr.ErrProjectNotFound` → HTTP 404).
    pub const PROJECT_NOT_FOUND: i64 = 1001;
    /// The selector was malformed (empty / non-absolute cwd).
    pub const INVALID_PROJECT: i64 = 1002;
    /// A requested entity (task/job/…) was not found.
    pub const NOT_FOUND: i64 = 1003;
    /// The entity exists but is not in a state that accepts the request right
    /// now — e.g. a job whose run is terminal, or a queued run whose live
    /// runner has not spawned yet. The Go API surfaced this as HTTP 409
    /// Conflict; it is RETRYABLE (the caller may try again shortly), unlike a
    /// malformed-params [`INVALID_PARAMS`]. Mirror of the daemon's 409.
    pub const CONFLICT: i64 = 1004;
}
