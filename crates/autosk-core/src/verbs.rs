//! High-level write verbs — the orchestration layer the RPC server dispatches
//! to. Each verb performs the mutation(s) on the project's writer connection,
//! formats the **byte-identical** `dolt_commit` message (branching on
//! [`Source`] so the CLI and lazy front-ends keep their distinct audit
//! strings), commits via [`crate::store::Db::commit`], and returns the enriched
//! wire projection.
//!
//! This is the single place commit-message + source-specific behaviour parity
//! is encoded (mirror of the `cmd/autosk/*.go` verbs and the
//! `internal/lazy/datasource/offline.go` mirrors).

use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{Map, Value};

use crate::ctx::Ctx;
use crate::error::{Error, Result};
use crate::pkg::Registry;
use crate::projectmgr::Project;
use crate::tasks::{self, Task, TaskPatch};
use crate::worktree::WorktreeManager;
use crate::{
    agents_write, bootstrap, comments_write, deps, ids, meta, metaverbs, signals, tasksvc, wfcrud,
    wfengine,
};
use autosk_proto::wire;

/// Which front end issued the write (selects the commit-message dialect + a few
/// behavioural differences). Defaults to [`Source::Cli`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Source {
    Cli,
    Lazy,
}

impl Source {
    pub fn parse(s: &str) -> Source {
        if s == "lazy" {
            Source::Lazy
        } else {
            Source::Cli
        }
    }
}

const MIN_PRIORITY: i64 = 0;
const MAX_PRIORITY: i64 = 3;
const DEFAULT_PRIORITY: i64 = 2;

fn enrich(proj: &Project, id: &str) -> Result<wire::TaskView> {
    proj.db.task_get(id)
}

// ---- create ---------------------------------------------------------------

/// Parameters for [`create`].
#[derive(Debug, Default, Clone)]
pub struct CreateParams {
    pub title: String,
    pub description: String,
    pub priority: i64,
    pub blocks: Vec<String>,
    pub blocked_by: Vec<String>,
    pub workflow: String,
    pub agent: String,
    pub step: String,
    pub base_ref: String,
    /// Caller agent name (the client's `$AUTOSK_AGENT`, or `human`). Resolved
    /// client-side and threaded through (the daemon never reads the client env).
    pub caller: String,
}

/// `create` — new task, optionally entering a workflow (CLI) or plain
/// title/desc/priority (lazy).
pub fn create(
    proj: &Project,
    packages: &Registry,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    source: Source,
    p: CreateParams,
) -> Result<wire::TaskView> {
    let title = p.title.trim().to_string();
    if title.is_empty() {
        return Err(Error::Invalid("title is required".into()));
    }

    if source == Source::Lazy {
        // Lazy: title/desc/priority only; clamp out-of-range priority.
        let mut priority = p.priority;
        if !(MIN_PRIORITY..=MAX_PRIORITY).contains(&priority) {
            priority = DEFAULT_PRIORITY;
        }
        let id = proj.db.with_write(|conn| {
            let id = ids::mint_unique(conn, ids::TASK_PREFIX, "tasks", "id")?;
            tasks::create_task(
                conn,
                Task {
                    id: id.clone(),
                    title: title.clone(),
                    description: p.description.clone(),
                    status: tasks::STATUS_NEW.into(),
                    priority,
                    ..Default::default()
                },
            )?;
            Ok(id)
        })?;
        proj.db.commit(&format!("lazy: create task {id}"))?;
        return enrich(proj, &id);
    }

    // CLI path.
    if !p.workflow.is_empty() && !p.agent.is_empty() {
        return Err(Error::Invalid(
            "--workflow and --agent are mutually exclusive".into(),
        ));
    }
    if !p.step.is_empty() && !p.agent.is_empty() {
        return Err(Error::Invalid(
            "--step only applies with --workflow (single:<agent> workflows have a single step)"
                .into(),
        ));
    }
    if !p.step.is_empty() && p.workflow.is_empty() && p.agent.is_empty() {
        return Err(Error::Invalid("--step requires --workflow".into()));
    }
    let priority = p.priority;

    // Resolve entry + author + create + (worktree) + enter-step + edges, then
    // one commit.
    let has = |n: &str| packages.has(n);
    let entry = if !p.workflow.is_empty() || !p.agent.is_empty() {
        Some(proj.db.with_write(|conn| {
            resolve_workflow_entry(conn, packages, &p.workflow, &p.agent, &p.step)
        })?)
    } else {
        None
    };

    let caller = if p.caller.trim().is_empty() {
        agents_write::HUMAN_AGENT_NAME.to_string()
    } else {
        p.caller.trim().to_string()
    };
    let id = proj.db.with_write(|conn| {
        let author = agents_write::ensure_by_name(conn, &caller, Some(&has))
            .map_err(|e| Error::Invalid(format!("resolve caller agent: {e}")))?;
        let id = ids::mint_unique(conn, ids::TASK_PREFIX, "tasks", "id")?;
        tasks::create_task(
            conn,
            Task {
                id: id.clone(),
                title: title.clone(),
                description: p.description.clone(),
                status: tasks::STATUS_NEW.into(),
                priority,
                author_id: author.id,
                ..Default::default()
            },
        )?;
        Ok(id)
    })?;

    if let Some(entry) = &entry {
        if entry.isolation == "worktree" {
            if let Err(e) = worktrees.ensure(ctx, &proj.root, &id, "") {
                let _ = proj.db.with_write(|conn| tasks::delete_task(conn, &id));
                return Err(Error::Invalid(format!(
                    "create {id}: allocate worktree: {e}"
                )));
            }
        }
        // EnterStep (atomic visit bump + pointer move). NB: enter_step takes
        // the writer lock itself, so it must NOT run inside a with_write
        // closure (std Mutex is not reentrant).
        let entered = proj
            .db
            .wf_find_step_by_id(&entry.step_id)
            .and_then(|step| wfengine::enter_step(&*proj.db, &id, &step, Some(&entry.workflow_id)));
        if let Err(e) = entered {
            if entry.isolation == "worktree" {
                let _ = worktrees.on_terminal(ctx, &proj.root, &id);
            }
            return Err(Error::Invalid(format!(
                "created task {id} but failed to enter the workflow: {}\nthe task is in status='new'; you can drop it with `autosk cancel {id}` or retry with `autosk enroll {id} ...`",
                map_enter_step_error(&e, &id)
            )));
        }
    }

    // Edges.
    if !p.blocks.is_empty() {
        for other in &p.blocks {
            proj.db
                .with_write(|conn| block_translate(conn, other, std::slice::from_ref(&id)))?;
        }
    }
    if !p.blocked_by.is_empty() {
        proj.db
            .with_write(|conn| block_translate(conn, &id, &p.blocked_by))?;
    }

    proj.db.commit(&format!("create {id}"))?;
    enrich(proj, &id)
}

// ---- enroll ---------------------------------------------------------------

/// `enroll` — (re-)attach a task to a workflow's entry step. Returns the
/// enriched task view plus a `base_ref_ignored` flag the front end renders as a
/// `--base-ref ignored` warning (the worktree branch already existed).
#[allow(clippy::too_many_arguments)]
pub fn enroll(
    proj: &Project,
    packages: &Registry,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    source: Source,
    task_id: &str,
    workflow: &str,
    agent: &str,
    step: &str,
    base_ref: &str,
) -> Result<(wire::TaskView, bool)> {
    if !workflow.is_empty() && !agent.is_empty() {
        return Err(Error::Invalid(
            "--workflow and --agent are mutually exclusive".into(),
        ));
    }
    if workflow.is_empty() && agent.is_empty() {
        return Err(Error::Invalid(
            "--workflow NAME or --agent NAME is required".into(),
        ));
    }
    if !step.is_empty() && !agent.is_empty() {
        return Err(Error::Invalid(
            "--step only applies with --workflow (single:<agent> workflows have a single step)"
                .into(),
        ));
    }
    if !base_ref.is_empty() && !agent.is_empty() {
        return Err(Error::Invalid(
            "--base-ref requires --workflow targeting an isolation=worktree workflow".into(),
        ));
    }

    let cur = proj.db.task_get_row(task_id).map_err(|e| match e {
        Error::NotFound => Error::Conflict(format!("task not found: {task_id}")),
        o => o,
    })?;

    // Enrollability guard (source-specific message).
    if cur.status == tasks::STATUS_WORK {
        return Err(Error::Conflict(match source {
            Source::Lazy => "enroll: task is 'work' (owned by engine); cancel then enroll to switch workflows (or reopen first to inspect in 'new')".into(),
            Source::Cli => format!(
                "task already enrolled in workflow {} at step {}; the daemon will advance it — to switch workflows, cancel then enroll (or reopen first if you want to inspect the task in 'new' state)",
                cur.workflow_id, cur.current_step_id
            ),
        }));
    }

    let entry = proj
        .db
        .with_write(|conn| resolve_workflow_entry(conn, packages, workflow, agent, step))?;

    // Worktree (CLI only; lazy never allocates one — documented limitation).
    let mut wt_allocated = false;
    let mut base_ref_ignored = false;
    if source == Source::Cli {
        if entry.isolation == "worktree" {
            match worktrees.ensure(ctx, &proj.root, task_id, base_ref) {
                Ok(res) => {
                    // The `--base-ref ignored` warning is returned to the front
                    // end (the daemon's stderr is not the client's), which
                    // renders it for the operator.
                    base_ref_ignored = res.base_ref_ignored;
                    wt_allocated = !res.existing;
                }
                Err(e) => {
                    return Err(Error::Invalid(format!(
                        "enroll {task_id}: allocate worktree: {e}"
                    )))
                }
            }
        } else if !base_ref.is_empty() {
            return Err(Error::Invalid(
                "--base-ref requires the target workflow to use isolation=worktree".into(),
            ));
        }
    }

    let entered = proj
        .db
        .wf_find_step_by_id(&entry.step_id)
        .and_then(|st| wfengine::enter_step(&*proj.db, task_id, &st, Some(&entry.workflow_id)));
    if let Err(e) = entered {
        if wt_allocated {
            let _ = worktrees.on_terminal(ctx, &proj.root, task_id);
        }
        return Err(map_enter_step_error(&e, task_id));
    }

    let msg = match source {
        Source::Cli => {
            let label = if !workflow.is_empty() {
                format!("--workflow {workflow}")
            } else {
                format!("--agent {agent}")
            };
            format!("enroll {task_id} {label}")
        }
        Source::Lazy => {
            if step.is_empty() {
                format!("lazy: enroll {task_id} -> {workflow}")
            } else {
                format!("lazy: enroll {task_id} -> {workflow} --step {step}")
            }
        }
    };
    proj.db.commit(&msg)?;
    let view = enrich(proj, task_id)?;
    Ok((view, base_ref_ignored))
}

// ---- resume ---------------------------------------------------------------

/// `resume` — human → work, optionally relocating the current step.
pub fn resume(
    proj: &Project,
    source: Source,
    task_id: &str,
    to_step: &str,
) -> Result<wire::TaskView> {
    let cur = proj.db.task_get_row(task_id).map_err(|e| match e {
        Error::NotFound => Error::Conflict(format!("task not found: {task_id}")),
        o => o,
    })?;
    if cur.status != tasks::STATUS_HUMAN {
        return Err(Error::Conflict(match source {
            Source::Lazy => format!("resume: task is not 'human' (status={})", cur.status),
            Source::Cli => format!(
                "cannot resume task in status {:?} (only `human`)",
                cur.status
            ),
        }));
    }
    if cur.workflow_id.is_empty() && source == Source::Cli {
        return Err(Error::Conflict(format!(
            "task {task_id} has no workflow_id; cannot resume"
        )));
    }
    if to_step.is_empty() {
        if cur.current_step_id.is_empty() {
            return Err(Error::Conflict(
                "task has no current_step_id; pass --to STEP".into(),
            ));
        }
        proj.db.with_write(|conn| {
            tasks::update_task(
                conn,
                task_id,
                &TaskPatch {
                    status: Some(tasks::STATUS_WORK.into()),
                    ..Default::default()
                },
            )
            .map(|_| ())
        })?;
        let msg = match source {
            Source::Cli => format!("resume {task_id}"),
            Source::Lazy => format!("lazy: resume {task_id}"),
        };
        proj.db.commit(&msg)?;
        return enrich(proj, task_id);
    }
    // --to STEP: deliberate transition. enter_step locks the writer itself, so
    // resolve the step and enter it outside any with_write closure.
    let st = proj
        .db
        .wf_find_step_by_name(&cur.workflow_id, to_step)
        .map_err(|e| match e {
            Error::NotFound => match source {
                Source::Cli => Error::Conflict(format!(
                    "step {to_step:?} not found in this task's workflow"
                )),
                Source::Lazy => {
                    Error::Conflict(format!("resume target step {to_step:?}: not found"))
                }
            },
            o => o,
        })?;
    if let Err(e) = wfengine::enter_step(&*proj.db, task_id, &st, None) {
        return Err(map_enter_step_error(&e, task_id));
    }
    let msg = match source {
        Source::Cli => format!("resume {task_id} --to {to_step}"),
        Source::Lazy => format!("lazy: resume {task_id} --to {to_step}"),
    };
    proj.db.commit(&msg)?;
    enrich(proj, task_id)
}

// ---- block / unblock ------------------------------------------------------

/// `block` — add blocker edges; CLI joins blockers with `,`, lazy commits one
/// edge with the `<-` syntax.
pub fn block(proj: &Project, source: Source, id: &str, blockers: &[String]) -> Result<()> {
    proj.db
        .with_write(|conn| block_translate(conn, id, blockers))?;
    let msg = match source {
        Source::Cli => format!("block {id} by {}", blockers.join(",")),
        Source::Lazy => format!("lazy: block {id}<-{}", blockers.join(",")),
    };
    proj.db.commit(&msg)?;
    Ok(())
}

/// `unblock` — remove specific blocker edges.
pub fn unblock(proj: &Project, source: Source, id: &str, blockers: &[String]) -> Result<()> {
    proj.db
        .with_write(|conn| deps::unblock(conn, id, blockers))?;
    let msg = match source {
        Source::Cli => format!("unblock {id} from {}", blockers.join(",")),
        Source::Lazy => format!("lazy: unblock {id}<-{}", blockers.join(",")),
    };
    proj.db.commit(&msg)?;
    Ok(())
}

/// `unblock --all` — remove every incoming blocker edge.
pub fn unblock_all(proj: &Project, id: &str) -> Result<i64> {
    let n = proj.db.with_write(|conn| deps::unblock_all(conn, id))?;
    proj.db.commit(&format!("unblock {id} --all"))?;
    Ok(n)
}

fn block_translate(conn: &Connection, id: &str, blockers: &[String]) -> Result<()> {
    deps::block(conn, id, blockers).map_err(|e| match e {
        deps::DepsError::SelfBlock => Error::Invalid(format!("a task cannot block itself: {id}")),
        deps::DepsError::Cycle => Error::Invalid(format!(
            "adding {} as blocker(s) of {id} would create a cycle",
            go_slice(blockers)
        )),
        deps::DepsError::TaskNotFound => Error::Conflict(format!("task not found: {id}")),
        deps::DepsError::BlockerNotFound => Error::Conflict(format!(
            "one of the blocker tasks does not exist: {}",
            go_slice(blockers)
        )),
        deps::DepsError::Core(e) => e,
    })
}

/// Renders a slice the way Go's `%v` does: `[a b c]`.
fn go_slice(v: &[String]) -> String {
    format!("[{}]", v.join(" "))
}

// ---- comment --------------------------------------------------------------

/// `comment add` — author from `$AUTOSK_AGENT` (no resolver gate; any author
/// auto-creates, mirroring the CLI/lazy comment path).
pub fn comment_add(
    proj: &Project,
    source: Source,
    task_id: &str,
    author: &str,
    text: &str,
) -> Result<wire::Comment> {
    if text.trim().is_empty() {
        return Err(Error::Invalid("comment text is empty".into()));
    }
    let author_name = if author.trim().is_empty() {
        agents_write::HUMAN_AGENT_NAME.to_string()
    } else {
        author.trim().to_string()
    };
    let c = proj.db.with_write(|conn| {
        let a = agents_write::ensure_by_name(conn, &author_name, None)
            .map_err(|e| Error::Invalid(format!("ensure author {author_name:?}: {e}")))?;
        comments_write::add(conn, task_id, &a.id, text)
    })?;
    let msg = match source {
        Source::Cli => format!("comment add {task_id}"),
        Source::Lazy => format!("lazy: comment {task_id}"),
    };
    proj.db.commit(&msg)?;
    Ok(c)
}

// ---- metadata -------------------------------------------------------------

/// `metadata set` result: the updated task + whether anything changed.
#[derive(Debug)]
pub struct MetaResult {
    pub task: wire::TaskView,
    pub changed: bool,
}

/// `metadata set` — set a dotted key (CLI) / wholesale replace (lazy
/// `SetMetadata`, via `replace=true`).
pub fn metadata_set(
    proj: &Project,
    source: Source,
    id: &str,
    key: &str,
    value: Value,
    replace_all: bool,
) -> Result<MetaResult> {
    if replace_all {
        // lazy SetMetadata: clear + copy the provided object.
        let obj = match value {
            Value::Object(m) => m,
            Value::Null => Map::new(),
            _ => return Err(Error::Invalid("metadata must be a JSON object".into())),
        };
        let upd = proj.db.with_write(|conn| {
            metaverbs::update_metadata(conn, id, |cur| {
                cur.clear();
                for (k, v) in &obj {
                    cur.insert(k.clone(), v.clone());
                }
                Ok(())
            })
        })?;
        if upd.changed {
            proj.db.commit(&format!("lazy: metadata {id}"))?;
        }
        return Ok(MetaResult {
            task: enrich(proj, id)?,
            changed: upd.changed,
        });
    }

    let path = metaverbs::split_metadata_key(key)?;
    if path[0] == meta::STEP_VISITS_KEY {
        metaverbs::validate_reserved_write(&path, &value)?;
    }
    let upd = proj.db.with_write(|conn| {
        metaverbs::update_metadata(conn, id, |m| {
            metaverbs::set_metadata_path(m, &path, value.clone())
        })
    })?;
    if upd.changed {
        let msg = match source {
            Source::Cli => format!("metadata set {id} --key {key}"),
            Source::Lazy => format!("lazy: metadata {id}"),
        };
        proj.db.commit(&msg)?;
    }
    Ok(MetaResult {
        task: enrich(proj, id)?,
        changed: upd.changed,
    })
}

/// `metadata unset` — remove a dotted key, pruning empty parents.
pub fn metadata_unset(proj: &Project, id: &str, key: &str) -> Result<MetaResult> {
    let path = metaverbs::split_metadata_key(key)?;
    let upd = proj.db.with_write(|conn| {
        metaverbs::update_metadata(conn, id, |m| {
            metaverbs::unset_metadata_path(m, &path);
            Ok(())
        })
    })?;
    if upd.changed {
        proj.db
            .commit(&format!("metadata unset {id} --key {key}"))?;
    }
    Ok(MetaResult {
        task: enrich(proj, id)?,
        changed: upd.changed,
    })
}

/// `metadata reset-visits` — clear all step counters, or just `step_id` /
/// `--step NAME` (resolved to its id; the commit records the resolved id).
pub fn metadata_reset_visits(
    proj: &Project,
    id: &str,
    step_name: &str,
    step_id: &str,
) -> Result<MetaResult> {
    if !step_name.is_empty() && !step_id.is_empty() {
        return Err(Error::Invalid(
            "--step and --step-id are mutually exclusive".into(),
        ));
    }
    let resolved = if !step_name.is_empty() {
        let tk = proj.db.task_get_row(id).map_err(|e| match e {
            Error::NotFound => Error::Conflict(format!("task not found: {id}")),
            o => o,
        })?;
        if tk.workflow_id.is_empty() {
            return Err(Error::Invalid(
                "--step requires the task to have a workflow_id; use --step-id ID for orphaned counters".into(),
            ));
        }
        proj.db
            .with_write(|conn| wfengine::find_step_by_name(conn, &tk.workflow_id, step_name))
            .map_err(|e| match e {
                Error::NotFound => Error::Conflict(format!(
                    "step {step_name:?} not found in this task's workflow"
                )),
                o => o,
            })?
            .id
    } else {
        step_id.to_string()
    };

    let resolved_c = resolved.clone();
    let upd = proj.db.with_write(|conn| {
        metaverbs::update_metadata(conn, id, |m| {
            meta::mutate_step_visits(m, |sv| {
                if resolved_c.is_empty() {
                    sv.clear();
                } else {
                    sv.remove(&resolved_c);
                }
            });
            Ok(())
        })
    })?;
    if upd.changed {
        let hint = if resolved.is_empty() {
            String::new()
        } else {
            format!(" --step-id {resolved}")
        };
        proj.db
            .commit(&format!("metadata reset-visits {id}{hint}"))?;
    }
    Ok(MetaResult {
        task: enrich(proj, id)?,
        changed: upd.changed,
    })
}

// ---- status verbs ---------------------------------------------------------

/// `done` — terminal status='done' (+ worktree cleanup).
pub fn done(
    proj: &Project,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    id: &str,
) -> Result<wire::TaskView> {
    proj.db
        .with_write(|conn| tasksvc::done(conn, ctx, id, &proj.root, worktrees).map(|_| ()))?;
    proj.db.commit(&format!("done {id}"))?;
    enrich(proj, id)
}

/// `cancel` — terminal status='cancel' (+ worktree cleanup).
pub fn cancel(
    proj: &Project,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    id: &str,
) -> Result<wire::TaskView> {
    proj.db
        .with_write(|conn| tasksvc::cancel(conn, ctx, id, &proj.root, worktrees).map(|_| ()))?;
    proj.db.commit(&format!("cancel {id}"))?;
    enrich(proj, id)
}

/// `reopen` — done|cancel → new.
pub fn reopen(proj: &Project, id: &str) -> Result<wire::TaskView> {
    proj.db
        .with_write(|conn| tasksvc::reopen(conn, id).map(|_| ()))?;
    proj.db.commit(&format!("reopen {id}"))?;
    enrich(proj, id)
}

/// `update --status` (CLI `update` verb, one commit `update <id>`). Also
/// applies title/description/priority field patches when set.
#[allow(clippy::too_many_arguments)]
pub fn update(
    proj: &Project,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    id: &str,
    title: Option<String>,
    description: Option<String>,
    priority: Option<i64>,
    status: Option<String>,
) -> Result<wire::TaskView> {
    let patch = TaskPatch {
        title: title.clone(),
        description: description.clone(),
        priority,
        ..Default::default()
    };
    let has_field_patch =
        patch.title.is_some() || patch.description.is_some() || patch.priority.is_some();
    if !has_field_patch && status.is_none() {
        return Err(Error::Invalid(
            "nothing to update (set --title/--description/--priority/--status)".into(),
        ));
    }
    if let Some(s) = &status {
        if !matches!(s.as_str(), "new" | "work" | "human" | "done" | "cancel") {
            return Err(Error::Invalid(format!(
                "invalid status {s:?} (valid: new, work, human, done, cancel)"
            )));
        }
    }
    if has_field_patch {
        proj.db
            .with_write(|conn| tasks::update_task(conn, id, &patch).map(|_| ()))
            .map_err(|e| match e {
                Error::NotFound => Error::Conflict(format!("task not found: {id}")),
                o => o,
            })?;
    }
    if let Some(s) = &status {
        proj.db.with_write(|conn| {
            tasksvc::set_status(conn, ctx, id, s, &proj.root, worktrees).map(|_| ())
        })?;
    }
    proj.db.commit(&format!("update {id}"))?;
    enrich(proj, id)
}

/// lazy `UpdateStatus` — routes through tasksvc; commit `lazy: status <id>=<s>`.
pub fn lazy_update_status(
    proj: &Project,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    id: &str,
    status: &str,
) -> Result<wire::TaskView> {
    proj.db.with_write(|conn| match status {
        "done" => tasksvc::done(conn, ctx, id, &proj.root, worktrees).map(|_| ()),
        "cancel" => tasksvc::cancel(conn, ctx, id, &proj.root, worktrees).map(|_| ()),
        "new" => tasksvc::reopen(conn, id).map(|_| ()),
        other => tasksvc::set_status(conn, ctx, id, other, &proj.root, worktrees).map(|_| ()),
    })?;
    proj.db.commit(&format!("lazy: status {id}={status}"))?;
    enrich(proj, id)
}

/// lazy `UpdateTitleDescription` — commit `lazy: edit <id>`.
pub fn lazy_update_title_description(
    proj: &Project,
    id: &str,
    title: &str,
    description: &str,
) -> Result<wire::TaskView> {
    let title = title.trim().to_string();
    if title.is_empty() {
        return Err(Error::Invalid("title required".into()));
    }
    proj.db.with_write(|conn| {
        tasks::update_task(
            conn,
            id,
            &TaskPatch {
                title: Some(title.clone()),
                description: Some(description.to_string()),
                ..Default::default()
            },
        )
        .map(|_| ())
    })?;
    proj.db.commit(&format!("lazy: edit {id}"))?;
    enrich(proj, id)
}

/// lazy `UpdatePriority` — commit `lazy: priority <id>=<p>`.
pub fn lazy_update_priority(proj: &Project, id: &str, p: i64) -> Result<wire::TaskView> {
    if !(MIN_PRIORITY..=MAX_PRIORITY).contains(&p) {
        return Err(Error::Invalid(format!(
            "priority must be in [{MIN_PRIORITY},{MAX_PRIORITY}]"
        )));
    }
    proj.db.with_write(|conn| {
        tasks::update_task(
            conn,
            id,
            &TaskPatch {
                priority: Some(p),
                ..Default::default()
            },
        )
        .map(|_| ())
    })?;
    proj.db.commit(&format!("lazy: priority {id}={p}"))?;
    enrich(proj, id)
}

// ---- workflow -------------------------------------------------------------

/// `workflow create` — parse (file or inline JSON), auto-install missing scoped
/// agents (unless `no_install`), persist, commit. Returns the workflow name.
pub fn workflow_create(
    proj: &Project,
    packages: &Registry,
    source: Source,
    file: &str,
    json: &str,
    no_install: bool,
) -> Result<String> {
    let def = if !file.is_empty() {
        wfcrud::parse_file(file)?
    } else {
        wfcrud::parse_reader(json)?
    };
    let name = proj.db.with_write(|conn| {
        if !no_install {
            bootstrap::auto_install_missing_agents(conn, packages, &def)?;
        }
        wfcrud::create(conn, &def, false)
    })?;
    let msg = match source {
        Source::Cli => format!("workflow create {name}"),
        Source::Lazy => format!("lazy: create workflow {name}"),
    };
    proj.db.commit(&msg)?;
    Ok(name)
}

/// `workflow delete` — refuse if referenced; commit.
pub fn workflow_delete(proj: &Project, source: Source, name: &str) -> Result<()> {
    proj.db.with_write(|conn| wfcrud::delete(conn, name))?;
    let msg = match source {
        Source::Cli => format!("workflow delete {name}"),
        Source::Lazy => format!("lazy: delete workflow {name}"),
    };
    proj.db.commit(&msg)?;
    Ok(())
}

/// `workflow update --isolation` — flip isolation; commit only on a real flip.
///
/// Always returns the (possibly partial) report alongside the result so the
/// caller can render the force-safety-matrix diagnostics even on the error path
/// (parity with Go: the CLI renders `toWorkflowUpdateJSON(rep)` and lazy returns
/// `(out, err)` even when the flip fails).
#[allow(clippy::too_many_arguments)]
pub fn workflow_update_isolation(
    proj: &Project,
    worktrees: &dyn WorktreeManager,
    ctx: &Ctx,
    source: Source,
    name: &str,
    mode: &str,
    force: bool,
    dry_run: bool,
) -> (wire::UpdateIsolationReport, Result<()>) {
    // The update touches DB rows AND shells git; run under the writer lock so
    // the column flip + ensures observe each other's writes.
    let (report, res) = match proj.db.with_writer(|conn| {
        wfcrud::update_isolation(conn, ctx, name, mode, force, dry_run, &proj.root, worktrees)
    }) {
        Ok(pair) => pair,
        Err(e) => return (wire::UpdateIsolationReport::default(), Err(e)),
    };
    if let Err(e) = res {
        return (report, Err(e));
    }
    if !dry_run && !report.noop {
        let msg = match source {
            Source::Cli => format!(
                "workflow update {name} isolation={}\u{2192}{}",
                report.from, report.to
            ),
            Source::Lazy => format!(
                "lazy: workflow update {name} isolation={}\u{2192}{}",
                report.from, report.to
            ),
        };
        if let Err(e) = proj.db.commit(&msg) {
            return (report, Err(e));
        }
    }
    (report, Ok(()))
}

// ---- agent ----------------------------------------------------------------

/// `agent install` — npm install + ensure agents row; commit
/// `agent install <name>@<version>`.
pub fn agent_install(
    proj: &Project,
    packages: &Registry,
    name: &str,
    version: &str,
    spec: &str,
) -> Result<wire::Agent> {
    packages
        .ensure_prefix()
        .map_err(|e| Error::Invalid(format!("ensure packages prefix: {e}")))?;
    // `spec` (when non-empty) is the explicit npm spec the front end resolved —
    // notably a local-path install (`agent install ./path`), where `name` is the
    // package.json name and `spec` is the absolute directory. Otherwise install
    // by registry name@version.
    let entry = if spec.is_empty() {
        packages.install(name, version)
    } else {
        packages.install_spec(name, spec)
    }
    .map_err(Error::Invalid)?;
    // Best-effort runtime install for custom-runner agents handled by Resolve;
    // we skip it here (the executor calls ensure_runtime lazily).
    let has = |n: &str| packages.has(n);
    proj.db.with_write(|conn| {
        agents_write::ensure_by_name(conn, &entry.name, Some(&has))
            .map_err(|e| Error::Invalid(format!("ensure DB row for {}: {e}", entry.name)))
            .map(|_| ())
    })?;
    proj.db
        .commit(&format!("agent install {}@{}", entry.name, entry.version))?;
    // Return the enriched agent view (re-read from agent.list filtered).
    let agents = proj.db.agent_list()?;
    agents
        .into_iter()
        .find(|a| a.name == entry.name)
        .ok_or(Error::NotFound)
}

/// `agent uninstall` — npm uninstall + registry removal. NO dolt_commit (the
/// agents row is intentionally retained, mirroring the CLI).
pub fn agent_uninstall(proj: &Project, packages: &Registry, name: &str, force: bool) -> Result<()> {
    if !force {
        // Propagate (never swallow) a probe failure: a transient DB/lock error
        // must NOT be read as "0 references → safe to delete".
        let refs: i64 = proj.db.with_write(|conn| {
            Ok(conn.query_row(
                "SELECT COUNT(*) FROM steps WHERE agent_id = (SELECT id FROM agents WHERE name = ?1)",
                params![name],
                |r| r.get(0),
            )?)
        })?;
        if refs > 0 {
            return Err(Error::Conflict(format!(
                "agent {name} is referenced by {refs} workflow step(s); pass --force to uninstall anyway"
            )));
        }
    }
    packages.uninstall(name).map_err(Error::Invalid)?;
    Ok(())
}

// ---- sql ------------------------------------------------------------------

/// `sql.query` result.
pub struct SqlRows {
    pub columns: Vec<String>,
    pub rows: Vec<Vec<Value>>,
}

/// `sql query` — read-only passthrough.
pub fn sql_query(proj: &Project, query: &str) -> Result<SqlRows> {
    proj.db.with_read(|conn| {
        let mut stmt = conn.prepare(query)?;
        let columns: Vec<String> = stmt.column_names().iter().map(|s| s.to_string()).collect();
        let n = columns.len();
        let mut rows_out: Vec<Vec<Value>> = Vec::new();
        let mut rows = stmt.query([])?;
        while let Some(r) = rows.next()? {
            let mut row = Vec::with_capacity(n);
            for i in 0..n {
                row.push(sqlite_value_to_json(r.get_ref(i)?));
            }
            rows_out.push(row);
        }
        Ok(SqlRows {
            columns,
            rows: rows_out,
        })
    })
}

/// `sql --write` — raw exec passthrough; returns rows affected. NO dolt_commit
/// (mirrors the CLI: `sql --write` deliberately does not commit).
pub fn sql_exec(proj: &Project, query: &str) -> Result<i64> {
    proj.db
        .with_write(|conn| Ok(conn.execute(query, [])? as i64))
}

// ---- step.next (agent-facing) --------------------------------------------

/// `step next <id> --to <to>` — record the agent's chosen workflow transition
/// for the task's active run (port of `cmd/autosk/step.go` + `internal/step`).
/// Resolves the active `daemon_runs` row, validates `to` against the current
/// step's outgoing transitions, inserts the `step_signals` row, and commits
/// `step next <id> --to <to>` (best-effort, mirroring the CLI's ignored
/// DoltCommit error). Agent-facing — CLI dialect only.
pub fn step_next(proj: &Project, id: &str, to: &str) -> Result<signals::Emitted> {
    let res = proj.db.with_writer(|conn| signals::emit(conn, id, to))?;
    let emitted = match res {
        Ok(e) => e,
        Err(se) => return Err(map_signal_error(se, id, to)),
    };
    let _ = proj.db.commit(&format!("step next {id} --to {to}"));
    Ok(emitted)
}

/// Maps a [`signals::SignalError`] onto the byte-identical CLI-final message
/// the pre-daemon `cmd/autosk/step.go` produced (the daemon is now the sole
/// writer, so its error text is what the CLI surfaces after stripping the
/// transport prefix).
fn map_signal_error(e: signals::SignalError, id: &str, to: &str) -> Error {
    use signals::SignalError as SE;
    match e {
        // CLI re-wraps ErrNoActiveRun with the task id + daemon hint.
        SE::NoActiveRun => Error::Invalid(format!(
            "no active run for task {id} (is the daemon running it?)"
        )),
        // CLI surfaces ErrUnknownTarget verbatim: `... : "<to>" (valid: <set>)`.
        SE::UnknownTarget(valid) => Error::Invalid(format!(
            "target not in current step's transitions: {to:?} (valid: {valid})"
        )),
        // CLI appends the once-per-run hint to ErrAlreadyEmitted.
        SE::AlreadyEmitted => Error::Conflict(
            "step_next_already_emitted (you can only call `step next` once per run)".to_string(),
        ),
        SE::Ambiguous(t) => Error::Invalid(format!("ambiguous target {t:?}")),
        SE::Core(err) => err,
    }
}

// ---- maint.compact (operator-facing gc) ----------------------------------

/// `gc` — run doltlite chunk-store compaction (`SELECT dolt_gc()`) on the
/// project DB and return the parsed stats (port of `cmd/autosk/gc.go`). The
/// client measures the wall-clock duration around the RPC.
pub fn compact(proj: &Project) -> Result<crate::store::GcStats> {
    proj.db.gc()
}

fn sqlite_value_to_json(v: rusqlite::types::ValueRef<'_>) -> Value {
    use rusqlite::types::ValueRef;
    match v {
        ValueRef::Null => Value::Null,
        ValueRef::Integer(i) => Value::from(i),
        ValueRef::Real(f) => Value::from(f),
        ValueRef::Text(t) => Value::from(String::from_utf8_lossy(t).to_string()),
        ValueRef::Blob(b) => Value::from(String::from_utf8_lossy(b).to_string()),
    }
}

// ---- project.init ---------------------------------------------------------

/// `project.init` — migrate (idempotent) + ensure packages prefix + bootstrap
/// `feature-dev-generic` (unless `skip_bootstrap`). Best-effort bootstrap:
/// failures are surfaced as a returned warning string, not a hard error
/// (mirror of the CLI's non-fatal bootstrap).
pub fn project_init(
    proj: &Project,
    packages: &Registry,
    skip_bootstrap: bool,
) -> Result<(i64, Option<String>)> {
    // migrate() returns the resulting schema version so the CLI `init` verb
    // can render `initialized <db> (schema_version=N)` without a second RPC.
    let version = proj.db.migrate()?;
    let _ = packages.ensure_prefix();
    if skip_bootstrap {
        return Ok((version, None));
    }
    let created = proj
        .db
        .with_write(|conn| bootstrap::bootstrap_default_workflow(conn, packages));
    match created {
        Ok(true) => {
            proj.db.commit(&format!(
                "init: bootstrap {} workflow",
                bootstrap::FEATURE_DEV_GENERIC_NAME
            ))?;
            Ok((
                version,
                Some(bootstrap::FEATURE_DEV_GENERIC_NAME.to_string()),
            ))
        }
        Ok(false) => Ok((version, None)),
        // Bootstrap is best-effort: surface the reason but don't fail init.
        Err(e) => Ok((version, Some(format!("bootstrap skipped: {e}")))),
    }
}

// ---- helpers --------------------------------------------------------------

/// Resolved workflow entry point (workflow id + step id + isolation).
struct WorkflowEntry {
    workflow_id: String,
    step_id: String,
    isolation: String,
}

/// Mirror of `resolveWorkflowEntry`: resolves `--workflow NAME [--step NAME]`
/// or `--agent NAME` to a `(workflow_id, step_id, isolation)`.
fn resolve_workflow_entry(
    conn: &Connection,
    packages: &Registry,
    workflow: &str,
    agent: &str,
    step: &str,
) -> Result<WorkflowEntry> {
    if !workflow.is_empty() {
        let row: Option<(String, String, Option<String>)> = conn
            .query_row(
                "SELECT id, first_step_id, isolation FROM workflows WHERE name = ?1",
                params![workflow],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
            )
            .optional()?;
        let (wf_id, first_step_id, iso) =
            row.ok_or_else(|| Error::Conflict(format!("workflow not found: {workflow}")))?;
        let isolation = wfcrud::normalize_isolation(&iso.unwrap_or_default());
        let step_id = if !step.is_empty() {
            step_id_by_name(conn, &wf_id, workflow, step)?
        } else {
            first_step_id
        };
        return Ok(WorkflowEntry {
            workflow_id: wf_id,
            step_id,
            isolation,
        });
    }
    // --agent: ensure the agent + the synthetic single:<agent> workflow.
    let has = |n: &str| packages.has(n);
    agents_write::ensure_by_name(conn, agent, Some(&has))
        .map_err(|e| Error::Invalid(format!("ensure agent {agent}: {e}")))?;
    let wf_name = wfcrud::ensure_single(conn, agent)?;
    let row: Option<(String, String)> = conn
        .query_row(
            "SELECT id, first_step_id FROM workflows WHERE name = ?1",
            params![wf_name],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()?;
    let (wf_id, first_step_id) =
        row.ok_or_else(|| Error::Migration(format!("single:{agent} missing after ensure")))?;
    Ok(WorkflowEntry {
        workflow_id: wf_id,
        step_id: first_step_id,
        isolation: "none".to_string(),
    })
}

/// Resolves a step name to its id within a workflow, with the CLI's
/// available-steps hint on miss.
fn step_id_by_name(conn: &Connection, wf_id: &str, wf_name: &str, name: &str) -> Result<String> {
    let mut stmt =
        conn.prepare("SELECT name, id FROM steps WHERE workflow_id = ?1 ORDER BY seq ASC")?;
    let rows = stmt.query_map(params![wf_id], |r| {
        Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?))
    })?;
    let mut names = Vec::new();
    for r in rows {
        let (n, id) = r?;
        if n == name {
            return Ok(id);
        }
        names.push(n);
    }
    Err(Error::Conflict(format!(
        "step {name:?} not found in workflow {wf_name} (available: {})",
        names.join(", ")
    )))
}

/// Mirror of `workflow.MapEnterStepError` for the max-visits hint.
fn map_enter_step_error(e: &Error, task_id: &str) -> Error {
    if let Error::MaxVisitsExceeded { step_name, max, .. } = e {
        return Error::Conflict(format!(
            "cannot enter step {step_name:?}: already at max_visits={max}; reset with `autosk metadata reset-visits {task_id} --step {step_name}` or resume to a different step"
        ));
    }
    // Clone the underlying message.
    match e {
        Error::NotFound => Error::NotFound,
        other => Error::Invalid(other.to_string()),
    }
}
