//! `autosk-proto` — the JSON-RPC wire contract for autoskd (plan §5).
//!
//! This crate is the **single source of truth** for the shapes exchanged over
//! the daemon's line-delimited JSON-RPC surface. `autoskd` (the Rust server),
//! the Go JSON-RPC client (`internal/daemon/rpcclient`) and the future Tauri
//! client all (de)serialise the same types so behaviour cannot drift.
//!
//! ## Conventions
//!
//! * **Timestamps** are RFC3339 UTC strings (the machine-wire-format rule from
//!   `AGENTS.md`). On-disk they are unix-second `INTEGER`s; `autosk-core`
//!   converts to RFC3339 whole-second form (`YYYY-MM-DDTHH:MM:SSZ`) on the way
//!   out, matching Go's `time.Unix(u,0).UTC()` JSON marshaling.
//! * **Field names** are `snake_case`. The job-shaped types mirror the existing
//!   `internal/daemon/api` JSON tags verbatim (`job_id`, `task_id`, …); the
//!   task/workflow/agent/comment/signal types mirror the Go
//!   `internal/lazy/datasource` projection structs (which had no on-disk JSON
//!   contract before — this crate defines it).
//! * **`omitempty` parity.** Optional-pointer / empty-string / `false`-bool
//!   fields that carried Go `omitempty` are reproduced with
//!   `skip_serializing_if` so the bytes match what a Go reader expects.

pub mod rpc;
pub mod wire;

pub use rpc::{error_codes, ErrorObject, Notification, Request, Response};
pub use wire::{
    Agent, Comment, Health, HealthProject, Job, MessageEvent, ProjectInfo, Signal, TaskRef,
    TaskView, VersionInfo, Workflow, WorkflowStep,
};
