//! Human-driven status transitions — the Rust port of `internal/tasksvc`.
//!
//! `done`/`cancel` clear `current_step_id` (CHECK invariant) and best-effort
//! reap the per-task worktree of an isolated workflow, leaving a `human`-agent
//! breadcrumb comment on failure. `reopen` only accepts done|cancel sources.
//! `set_status` rejects `work` as source AND target (engine territory).

use rusqlite::Connection;

use crate::ctx::Ctx;
use crate::error::{Error, Result};
use crate::tasks::{
    self, Task, TaskPatch, STATUS_CANCEL, STATUS_DONE, STATUS_HUMAN, STATUS_NEW, STATUS_WORK,
};
use crate::worktree::WorktreeManager;
use crate::{agents_write, comments_write, wfengine};

/// `Done` — status='done', clear current_step_id, reap worktree.
pub fn done(
    conn: &Connection,
    ctx: &Ctx,
    id: &str,
    project_root: &str,
    worktrees: &dyn WorktreeManager,
) -> Result<Task> {
    set_terminal(conn, ctx, id, STATUS_DONE, project_root, worktrees)
}

/// `Cancel` — status='cancel', clear current_step_id, reap worktree.
pub fn cancel(
    conn: &Connection,
    ctx: &Ctx,
    id: &str,
    project_root: &str,
    worktrees: &dyn WorktreeManager,
) -> Result<Task> {
    set_terminal(conn, ctx, id, STATUS_CANCEL, project_root, worktrees)
}

/// `Reopen` — done|cancel → new (clears step; preserves workflow_id).
pub fn reopen(conn: &Connection, id: &str) -> Result<Task> {
    let cur = get_or_not_found(conn, id)?;
    if cur.status != STATUS_DONE && cur.status != STATUS_CANCEL {
        return Err(Error::Conflict(format!(
            "cannot reopen task in status {:?} (only done|cancel)",
            cur.status
        )));
    }
    tasks::update_task(
        conn,
        id,
        &TaskPatch {
            status: Some(STATUS_NEW.to_string()),
            current_step_id: Some(String::new()),
            ..Default::default()
        },
    )
}

/// `SetStatus` — the generic field-update path. Rejects `work` as source and
/// target; delegates done|cancel|reopen; else a plain patch (clearing the step
/// when moving off a stepful non-human status).
pub fn set_status(
    conn: &Connection,
    ctx: &Ctx,
    id: &str,
    target: &str,
    project_root: &str,
    worktrees: &dyn WorktreeManager,
) -> Result<Task> {
    if !status_valid(target) {
        return Err(Error::Invalid(format!("invalid status {target:?}")));
    }
    if target == STATUS_WORK {
        return Err(Error::Conflict(
            "refusing to set status='work' directly; use `autosk enroll` or let the workflow engine advance it".into(),
        ));
    }
    let cur = get_or_not_found(conn, id)?;
    if cur.status == STATUS_WORK {
        return Err(Error::Conflict(
            "refusing to change status on a work task; use `autosk done|cancel` or let the workflow engine advance it".into(),
        ));
    }
    match target {
        STATUS_DONE => return done(conn, ctx, id, project_root, worktrees),
        STATUS_CANCEL => return cancel(conn, ctx, id, project_root, worktrees),
        STATUS_NEW if cur.status == STATUS_DONE || cur.status == STATUS_CANCEL => {
            return reopen(conn, id)
        }
        _ => {}
    }
    let mut patch = TaskPatch {
        status: Some(target.to_string()),
        ..Default::default()
    };
    if !cur.current_step_id.is_empty() && target != STATUS_HUMAN {
        patch.current_step_id = Some(String::new());
    }
    tasks::update_task(conn, id, &patch)
}

fn set_terminal(
    conn: &Connection,
    ctx: &Ctx,
    id: &str,
    target: &str,
    project_root: &str,
    worktrees: &dyn WorktreeManager,
) -> Result<Task> {
    let t = tasks::update_task(
        conn,
        id,
        &TaskPatch {
            status: Some(target.to_string()),
            current_step_id: Some(String::new()),
            ..Default::default()
        },
    )
    .map_err(map_not_found(id))?;
    cleanup_worktree_on_terminal(conn, ctx, &t, project_root, worktrees);
    Ok(t)
}

fn cleanup_worktree_on_terminal(
    conn: &Connection,
    ctx: &Ctx,
    t: &Task,
    project_root: &str,
    worktrees: &dyn WorktreeManager,
) {
    if t.workflow_id.is_empty() {
        return;
    }
    let wf = match wfengine::get_workflow_by_id(conn, &t.workflow_id) {
        Ok(w) => w,
        Err(e) => {
            eprintln!(
                "warning: worktree cleanup for {} skipped: load workflow {}: {e}",
                t.id, t.workflow_id
            );
            return;
        }
    };
    if wf.isolation != "worktree" {
        return;
    }
    if project_root.is_empty() {
        eprintln!(
            "warning: worktree cleanup for {} skipped: no project root provided",
            t.id
        );
        return;
    }
    if let Err(werr) = worktrees.on_terminal(ctx, project_root, &t.id) {
        eprintln!("warning: worktree cleanup for {} failed: {werr}", t.id);
        // Breadcrumb comment under the seeded human agent so the failure
        // survives past the call (captured by the verb's commit).
        if let Ok(Some(human)) = agents_write::get_by_name(conn, agents_write::HUMAN_AGENT_NAME) {
            let _ = comments_write::add(
                conn,
                &t.id,
                &human.id,
                &format!("worktree cleanup failed on {}: {werr}", t.status),
            );
        }
    }
}

fn status_valid(s: &str) -> bool {
    matches!(
        s,
        STATUS_NEW | STATUS_WORK | STATUS_HUMAN | STATUS_DONE | STATUS_CANCEL
    )
}

fn get_or_not_found(conn: &Connection, id: &str) -> Result<Task> {
    tasks::get_task(conn, id).map_err(map_not_found(id))
}

fn map_not_found(id: &str) -> impl Fn(Error) -> Error + '_ {
    move |e| match e {
        Error::NotFound => Error::Conflict(format!("task not found: {id}")),
        other => other,
    }
}
