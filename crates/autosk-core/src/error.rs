//! Crate-wide error type for the read core.

/// Result alias for `autosk-core`.
pub type Result<T> = std::result::Result<T, Error>;

/// Errors surfaced by the read core. `autoskd` maps these onto JSON-RPC error
/// codes (`autosk-proto::rpc::error_codes`).
#[derive(Debug)]
pub enum Error {
    /// A SQLite/doltlite-level error.
    Sqlite(rusqlite::Error),
    /// An internal lock was poisoned by a panicking thread.
    LockPoisoned(&'static str),
    /// A requested entity (task/job/workflow/agent) was not found.
    NotFound,
    /// A schema-migration step failed.
    Migration(String),
    /// A filesystem error (registry, project resolution, transcript read).
    Io(std::io::Error),
    /// No `.autosk/db` could be resolved from the given selector.
    ProjectNotFound(String),
    /// The project selector was malformed (empty / non-absolute cwd).
    InvalidProject(String),
    /// The persisted project registry (`~/.autosk/projects.json`) exists but
    /// could not be parsed. Surfaced (never silently coerced to empty) so a
    /// partial/hand-edited file is not clobbered on the next `add`.
    Registry(String),
    /// Entering a workflow step would cross its `max_visits` cap. `visits` is
    /// the count BEFORE the would-be increment (mirror of Go's
    /// `workflow.MaxVisitsExceededError`).
    MaxVisitsExceeded {
        step_id: String,
        step_name: String,
        visits: i64,
        max: i64,
    },
}

impl Error {
    /// True for the [`Error::MaxVisitsExceeded`] cap error (the Rust analogue
    /// of `errors.As(err, &MaxVisitsExceededError{})`, which the executor uses
    /// to surface the documented `step_max_visits_exceeded:` run error).
    pub fn is_max_visits_exceeded(&self) -> bool {
        matches!(self, Error::MaxVisitsExceeded { .. })
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Sqlite(e) => write!(f, "doltlite: {e}"),
            Error::LockPoisoned(what) => write!(f, "doltlite: {what} lock poisoned"),
            Error::NotFound => write!(f, "not found"),
            Error::Migration(m) => write!(f, "migration: {m}"),
            Error::Io(e) => write!(f, "io: {e}"),
            Error::ProjectNotFound(s) => write!(f, "project not found: {s}"),
            Error::InvalidProject(s) => write!(f, "invalid project selector: {s}"),
            Error::Registry(s) => write!(f, "registry: {s}"),
            Error::MaxVisitsExceeded {
                step_name,
                visits,
                max,
                ..
            } => write!(
                f,
                "step_max_visits_exceeded: step {step_name:?} reached visits={visits} max={max}"
            ),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Sqlite(e) => Some(e),
            Error::Io(e) => Some(e),
            _ => None,
        }
    }
}

impl From<rusqlite::Error> for Error {
    fn from(e: rusqlite::Error) -> Self {
        Error::Sqlite(e)
    }
}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::Io(e)
    }
}
