//! `step_signals` — the Rust port of `internal/step`.
//!
//! One row per run (PK on `run_id` ⇒ "exactly one signal per run"). The
//! agent records its chosen transition via [`emit`]; the executor reads it
//! after end-of-turn via [`get_for_run`] and advances the task.

use rusqlite::{params, Connection, OptionalExtension, Row};

use crate::error::Error;

/// Why [`emit`] / [`get_for_run`] could not produce a signal.
#[derive(Debug)]
pub enum SignalError {
    /// No `daemon_runs` row in status='running' for this task.
    NoActiveRun,
    /// `target` did not match any outgoing transition (carries the valid set).
    UnknownTarget(String),
    /// `target` matched more than one transition.
    Ambiguous(String),
    /// `step next` was already called for this run.
    AlreadyEmitted,
    /// A doltlite-level error.
    Core(Error),
}

impl std::fmt::Display for SignalError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SignalError::NoActiveRun => write!(f, "no active run for this task"),
            SignalError::UnknownTarget(valid) => {
                write!(
                    f,
                    "target not in current step's transitions (valid: {valid})"
                )
            }
            SignalError::Ambiguous(t) => write!(f, "ambiguous target {t:?}"),
            SignalError::AlreadyEmitted => write!(f, "step_next_already_emitted"),
            SignalError::Core(e) => write!(f, "{e}"),
        }
    }
}

impl From<Error> for SignalError {
    fn from(e: Error) -> Self {
        SignalError::Core(e)
    }
}

impl From<rusqlite::Error> for SignalError {
    fn from(e: rusqlite::Error) -> Self {
        SignalError::Core(Error::Sqlite(e))
    }
}

/// The materialised signal row (mirror of `step.Emitted`).
#[derive(Debug, Clone)]
pub struct Emitted {
    pub run_id: String,
    pub task_id: String,
    pub transition_id: i64,
    /// Empty when [`Emitted::task_status`] is set.
    pub next_step_name: String,
    /// Empty when [`Emitted::next_step_name`] is set.
    pub task_status: String,
    pub prompt_rule: String,
    pub created_at: i64,
}

struct Cand {
    id: i64,
    task_status: Option<String>,
    prompt_rule: String,
    next_step_name: String,
}

/// `Emit` — resolves the task's active run, validates `target` against the
/// run's current step's outgoing transitions, and records the chosen
/// transition. `target` is a sibling step name or one of {done,cancel,human}.
pub fn emit(
    conn: &Connection,
    task_id: &str,
    target: &str,
) -> std::result::Result<Emitted, SignalError> {
    let target = target.trim();
    if task_id.is_empty() || target.is_empty() {
        return Err(SignalError::Core(Error::Migration(
            "emit: task_id and target are required".into(),
        )));
    }

    // 1. Active run (most recent running row for the task).
    let active: Option<(String, String)> = conn
        .query_row(
            "SELECT job_id, step_id FROM daemon_runs \
             WHERE task_id = ?1 AND status = 'running' \
             ORDER BY created_at DESC, job_id DESC LIMIT 1",
            params![task_id],
            |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)),
        )
        .optional()?;
    let (run_id, step_id) = active.ok_or(SignalError::NoActiveRun)?;

    // 2. Resolve target → transition.
    let mut stmt = conn.prepare(
        "SELECT t.id, t.task_status, t.prompt_rule, \
                COALESCE((SELECT name FROM steps WHERE id = t.next_step_id), '') \
           FROM step_transitions t WHERE t.step_id = ?1",
    )?;
    let rows = stmt.query_map(params![step_id], |row| {
        Ok(Cand {
            id: row.get(0)?,
            task_status: row.get::<_, Option<String>>(1)?,
            prompt_rule: row.get(2)?,
            next_step_name: row.get(3)?,
        })
    })?;
    let mut all: Vec<Cand> = Vec::new();
    let mut picks: Vec<usize> = Vec::new();
    for r in rows {
        let c = r?;
        let matches_status = c.task_status.as_deref() == Some(target);
        let matches_step = !c.next_step_name.is_empty() && c.next_step_name == target;
        if matches_status || matches_step {
            picks.push(all.len());
        }
        all.push(c);
    }
    if picks.is_empty() {
        return Err(SignalError::UnknownTarget(describe_targets(&all)));
    }
    if picks.len() > 1 {
        return Err(SignalError::Ambiguous(target.to_string()));
    }
    let pick = &all[picks[0]];

    // 3. Insert the signal. PK(run_id) enforces "exactly one per run".
    let now = crate::timefmt::now_unix();
    let res = conn.execute(
        "INSERT INTO step_signals(run_id, task_id, transition_id, created_at) VALUES (?1,?2,?3,?4)",
        params![run_id, task_id, pick.id, now],
    );
    if let Err(e) = res {
        let msg = e.to_string();
        if msg.contains("UNIQUE constraint failed: step_signals.run_id")
            || msg.contains("PRIMARY KEY")
        {
            return Err(SignalError::AlreadyEmitted);
        }
        return Err(SignalError::from(e));
    }
    Ok(Emitted {
        run_id,
        task_id: task_id.to_string(),
        transition_id: pick.id,
        next_step_name: pick.next_step_name.clone(),
        task_status: pick.task_status.clone().unwrap_or_default(),
        prompt_rule: pick.prompt_rule.clone(),
        created_at: now,
    })
}

/// `GetForRun` — the signal recorded for `run_id`, or [`SignalError::NoActiveRun`]
/// when none exists.
pub fn get_for_run(conn: &Connection, run_id: &str) -> std::result::Result<Emitted, SignalError> {
    let scan = |row: &Row| -> rusqlite::Result<Emitted> {
        Ok(Emitted {
            run_id: row.get(0)?,
            task_id: row.get(1)?,
            transition_id: row.get(2)?,
            created_at: row.get(3)?,
            task_status: row.get::<_, Option<String>>(4)?.unwrap_or_default(),
            next_step_name: row.get(5)?,
            prompt_rule: row.get(6)?,
        })
    };
    let found = conn
        .query_row(
            "SELECT ss.run_id, ss.task_id, ss.transition_id, ss.created_at, \
                    t.task_status, \
                    COALESCE((SELECT name FROM steps WHERE id = t.next_step_id), '') AS next_name, \
                    t.prompt_rule \
               FROM step_signals ss \
               JOIN step_transitions t ON t.id = ss.transition_id \
              WHERE ss.run_id = ?1",
            params![run_id],
            scan,
        )
        .optional()?;
    found.ok_or(SignalError::NoActiveRun)
}

fn describe_targets(all: &[Cand]) -> String {
    let mut parts = Vec::new();
    for c in all {
        if let Some(s) = &c.task_status {
            if !s.is_empty() {
                parts.push(s.clone());
                continue;
            }
        }
        if !c.next_step_name.is_empty() {
            parts.push(c.next_step_name.clone());
        }
    }
    parts.join(", ")
}
