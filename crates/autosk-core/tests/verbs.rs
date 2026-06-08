//! Phase 3 write-verb integration tests: exercise `autosk_core::verbs` against
//! a fresh native (v12) DB and assert DB state + the byte-identical
//! `dolt_commit` messages the CLI/lazy front ends produced.

use std::sync::Arc;

use autosk_core::ctx::Ctx;
use autosk_core::pkg::Registry;
use autosk_core::projectmgr::Project;
use autosk_core::store::Db;
use autosk_core::verbs::{self, CreateParams, Source};
use autosk_core::wfcrud;
use autosk_core::worktree::Manager as WtManager;
use tempfile::TempDir;

struct Env {
    _dir: TempDir,
    _pkg_dir: TempDir,
    proj: Project,
    packages: Registry,
    wt: WtManager,
}

fn env() -> Env {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path().join("proj");
    std::fs::create_dir_all(root.join(".autosk")).unwrap();
    let db_path = root.join(".autosk").join("db");
    let db = Db::open_or_create(&db_path).unwrap();
    db.migrate().unwrap();
    let pkg_dir = tempfile::tempdir().unwrap();
    let proj = Project {
        root: root.to_string_lossy().to_string(),
        db_path: db_path.to_string_lossy().to_string(),
        db: Arc::new(db),
        opened_at: String::new(),
    };
    Env {
        _dir: dir,
        _pkg_dir: pkg_dir,
        proj,
        packages: Registry::open(tempfile::tempdir().unwrap().keep()),
        wt: WtManager::new(),
    }
}

/// Returns the most recent dolt commit message.
fn last_commit_msg(db: &Db) -> String {
    // dolt_log returns commits HEAD-first. Read via the WRITER connection: the
    // pooled reader connections hold a per-connection snapshot of the commit
    // graph and won't observe a fresh `dolt_commit` until reopened.
    db.with_write(|conn| {
        Ok(
            conn.query_row("SELECT message FROM dolt_log LIMIT 1", [], |r| {
                r.get::<_, String>(0)
            })?,
        )
    })
    .unwrap()
}

fn ctx() -> Ctx {
    Ctx::background()
}

#[test]
fn create_plain_cli_then_lazy() {
    let e = env();
    let view = verbs::create(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Cli,
        CreateParams {
            title: "  Build the thing  ".into(),
            description: "desc".into(),
            priority: 1,
            caller: "human".into(),
            ..Default::default()
        },
    )
    .unwrap();
    assert_eq!(view.title, "Build the thing");
    assert_eq!(view.status, "new");
    assert_eq!(view.priority, 1);
    assert_eq!(view.author_name, "human");
    assert_eq!(last_commit_msg(&e.proj.db), format!("create {}", view.id));

    // Lazy create uses its own commit dialect + clamps bad priority.
    let lv = verbs::create(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Lazy,
        CreateParams {
            title: "lazy task".into(),
            priority: 99,
            ..Default::default()
        },
    )
    .unwrap();
    assert_eq!(lv.priority, 2, "out-of-range priority clamps to default");
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("lazy: create task {}", lv.id)
    );
}

#[test]
fn block_unblock_cycle_self() {
    let e = env();
    let a = mk(&e, "a");
    let b = mk(&e, "b");
    // self-block rejected
    let err = verbs::block(&e.proj, Source::Cli, &a, std::slice::from_ref(&a)).unwrap_err();
    assert_eq!(err.to_string(), format!("a task cannot block itself: {a}"));
    // a blocked by b, commit message
    verbs::block(&e.proj, Source::Cli, &a, std::slice::from_ref(&b)).unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), format!("block {a} by {b}"));
    // cycle: b blocked by a would close a<->b
    let err = verbs::block(&e.proj, Source::Cli, &b, std::slice::from_ref(&a)).unwrap_err();
    assert!(err.to_string().contains("would create a cycle"), "{err}");
    // blocker not found
    let err = verbs::block(&e.proj, Source::Cli, &a, &["ask-zzzzzz".into()]).unwrap_err();
    assert!(
        err.to_string()
            .contains("one of the blocker tasks does not exist"),
        "{err}"
    );
    // unblock (lazy dialect)
    verbs::unblock(&e.proj, Source::Lazy, &a, std::slice::from_ref(&b)).unwrap();
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("lazy: unblock {a}<-{b}")
    );
}

#[test]
fn comment_add_and_empty() {
    let e = env();
    let a = mk(&e, "t");
    let c = verbs::comment_add(&e.proj, Source::Cli, &a, "ann", "hello\n").unwrap();
    assert_eq!(c.text, "hello");
    assert_eq!(c.author_name, "ann");
    assert_eq!(last_commit_msg(&e.proj.db), format!("comment add {a}"));
    let err = verbs::comment_add(&e.proj, Source::Cli, &a, "ann", "   ").unwrap_err();
    assert_eq!(err.to_string(), "comment text is empty");
}

#[test]
fn metadata_set_unset_reset_and_noop() {
    let e = env();
    let a = mk(&e, "t");
    // set a leaf
    let r = verbs::metadata_set(
        &e.proj,
        Source::Cli,
        &a,
        "tags.x",
        serde_json::json!(1),
        false,
    )
    .unwrap();
    assert!(r.changed);
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("metadata set {a} --key tags.x")
    );
    // setting the identical value is a no-op (no commit, changed=false)
    let before = last_commit_msg(&e.proj.db);
    let r2 = verbs::metadata_set(
        &e.proj,
        Source::Cli,
        &a,
        "tags.x",
        serde_json::json!(1),
        false,
    )
    .unwrap();
    assert!(!r2.changed);
    assert_eq!(
        last_commit_msg(&e.proj.db),
        before,
        "no-op set must not commit"
    );
    // reserved step_visits validation rejects a string leaf
    let err = verbs::metadata_set(
        &e.proj,
        Source::Cli,
        &a,
        "step_visits.st-x",
        serde_json::json!("no"),
        false,
    )
    .unwrap_err();
    assert!(
        err.to_string()
            .contains("step_visits leaves must be integers"),
        "{err}"
    );
    // unset prunes empty parents back to NULL
    let r3 = verbs::metadata_unset(&e.proj, &a, "tags.x").unwrap();
    assert!(r3.changed);
    assert!(
        r3.task.metadata.is_none(),
        "metadata should round-trip to NULL"
    );
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("metadata unset {a} --key tags.x")
    );
}

#[test]
fn workflow_create_delete_enroll_resume_status() {
    let e = env();
    // A simple human-agent workflow (no npm install needed).
    let name =
        verbs::workflow_create(&e.proj, &e.packages, Source::Cli, "", WF_JSON, true).unwrap();
    assert_eq!(name, "wf1");
    assert_eq!(last_commit_msg(&e.proj.db), "workflow create wf1");

    let a = mk(&e, "t");
    // enroll → status work + commit dialect
    let view = verbs::enroll(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Cli,
        &a,
        "wf1",
        "",
        "",
        "",
    )
    .unwrap();
    assert_eq!(view.status, "work");
    assert_eq!(view.step_name, "do");
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("enroll {a} --workflow wf1")
    );

    // delete refuses while a task references it.
    let err = verbs::workflow_delete(&e.proj, Source::Cli, "wf1").unwrap_err();
    assert!(err.to_string().contains("refuse delete"), "{err}");

    // done clears the step and commits "done <id>".
    let dv = verbs::done(&e.proj, &e.wt, &ctx(), &a).unwrap();
    assert_eq!(dv.status, "done");
    assert!(dv.current_step_id.is_empty());
    assert_eq!(last_commit_msg(&e.proj.db), format!("done {a}"));

    // reopen → new (preserves workflow_id, so wf1 stays referenced & undeletable)
    let rv = verbs::reopen(&e.proj, &a).unwrap();
    assert_eq!(rv.status, "new");
    assert_eq!(rv.workflow_id, view.workflow_id);
    assert_eq!(last_commit_msg(&e.proj.db), format!("reopen {a}"));
    assert!(verbs::workflow_delete(&e.proj, Source::Cli, "wf1").is_err());

    // An unreferenced workflow deletes cleanly (lazy dialect).
    let wf2 = WF_JSON.replace("wf1", "wf2");
    verbs::workflow_create(&e.proj, &e.packages, Source::Cli, "", &wf2, true).unwrap();
    verbs::workflow_delete(&e.proj, Source::Lazy, "wf2").unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), "lazy: delete workflow wf2");
}

/// resume: parks an enrolled task to `human`, then asserts the CLI + lazy
/// commit dialects with and without `--to STEP` (covers the regression in
/// review comment 643 — resume previously always wrote the CLI dialect).
#[test]
fn resume_cli_and_lazy_dialects() {
    let e = env();
    verbs::workflow_create(&e.proj, &e.packages, Source::Cli, "", WF_JSON, true).unwrap();
    let a = mk(&e, "t");
    verbs::enroll(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Cli,
        &a,
        "wf1",
        "",
        "",
        "",
    )
    .unwrap();

    // Park to 'human' (work→human is rejected by set_status, so flip the row
    // directly — current_step_id stays set, satisfying the CHECK invariant).
    // sql_exec deliberately does NOT commit, so commit the park explicitly:
    // otherwise the subsequent resume back to 'work' nets to no diff from HEAD.
    let park = |id: &str| {
        verbs::sql_exec(
            &e.proj,
            &format!("UPDATE tasks SET status='human' WHERE id='{id}'"),
        )
        .unwrap();
        e.proj.db.commit("park").unwrap();
    };

    // resume (no --to): CLI dialect.
    park(&a);
    let v = verbs::resume(&e.proj, Source::Cli, &a, "").unwrap();
    assert_eq!(v.status, "work");
    assert_eq!(last_commit_msg(&e.proj.db), format!("resume {a}"));

    // resume (no --to): lazy dialect.
    park(&a);
    verbs::resume(&e.proj, Source::Lazy, &a, "").unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), format!("lazy: resume {a}"));

    // resume --to do: CLI dialect.
    park(&a);
    verbs::resume(&e.proj, Source::Cli, &a, "do").unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), format!("resume {a} --to do"));

    // resume --to do: lazy dialect.
    park(&a);
    verbs::resume(&e.proj, Source::Lazy, &a, "do").unwrap();
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("lazy: resume {a} --to do")
    );
}

/// workflow.updateIsolation: the force-safety matrix, dry-run, both commit
/// dialects, and — critically — that the report is populated on the error path
/// (covers review comments 644 + 645, previously zero-tested).
#[test]
fn workflow_update_isolation_matrix() {
    let e = env();
    verbs::workflow_create(&e.proj, &e.packages, Source::Cli, "", WF_JSON, true).unwrap();

    // none→worktree (no tasks) — clean flip, CLI dialect (note the U+2192 arrow).
    let (rep, res) = verbs::workflow_update_isolation(
        &e.proj,
        &e.wt,
        &ctx(),
        Source::Cli,
        "wf1",
        "worktree",
        false,
        false,
    );
    res.unwrap();
    assert_eq!(rep.from, "none");
    assert_eq!(rep.to, "worktree");
    assert!(!rep.noop);
    assert_eq!(
        last_commit_msg(&e.proj.db),
        "workflow update wf1 isolation=none\u{2192}worktree"
    );

    // worktree→worktree — noop, no commit.
    let before = last_commit_msg(&e.proj.db);
    let (rep, res) = verbs::workflow_update_isolation(
        &e.proj,
        &e.wt,
        &ctx(),
        Source::Cli,
        "wf1",
        "worktree",
        false,
        false,
    );
    res.unwrap();
    assert!(rep.noop);
    assert_eq!(last_commit_msg(&e.proj.db), before, "noop must not commit");

    // worktree→none (no tasks) — clean flip, lazy dialect.
    let (_rep, res) = verbs::workflow_update_isolation(
        &e.proj,
        &e.wt,
        &ctx(),
        Source::Lazy,
        "wf1",
        "none",
        false,
        false,
    );
    res.unwrap();
    assert_eq!(
        last_commit_msg(&e.proj.db),
        "lazy: workflow update wf1 isolation=worktree\u{2192}none"
    );

    // dry_run none→worktree — report populated, NO commit, column stays none.
    let before = last_commit_msg(&e.proj.db);
    let (rep, res) = verbs::workflow_update_isolation(
        &e.proj,
        &e.wt,
        &ctx(),
        Source::Cli,
        "wf1",
        "worktree",
        false,
        true,
    );
    res.unwrap();
    assert!(rep.dry_run);
    assert_eq!(rep.from, "none");
    assert_eq!(rep.to, "worktree");
    assert_eq!(
        last_commit_msg(&e.proj.db),
        before,
        "dry-run must not commit"
    );

    // Error path: a non-terminal task referencing wf1 blocks a flip without
    // --force, and the report MUST carry non_terminal_tasks (comment 645).
    let a = mk(&e, "t");
    verbs::enroll(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Cli,
        &a,
        "wf1",
        "",
        "",
        "",
    )
    .unwrap();
    let (rep, res) = verbs::workflow_update_isolation(
        &e.proj,
        &e.wt,
        &ctx(),
        Source::Cli,
        "wf1",
        "worktree",
        false,
        false,
    );
    let err = res.unwrap_err();
    assert!(err.to_string().contains("non-terminal"), "{err}");
    assert_eq!(
        rep.non_terminal_tasks,
        vec![a],
        "report must be populated on the error path"
    );
}

/// The remaining lazy commit dialects the review flagged as unchecked
/// (status / edit / priority / metadata replace_all / enroll / comment).
#[test]
fn lazy_dialects_status_edit_priority_metadata() {
    let e = env();
    verbs::workflow_create(&e.proj, &e.packages, Source::Cli, "", WF_JSON, true).unwrap();
    let a = mk(&e, "t");

    // lazy enroll: `lazy: enroll <id> -> wf1`.
    verbs::enroll(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Lazy,
        &a,
        "wf1",
        "",
        "",
        "",
    )
    .unwrap();
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("lazy: enroll {a} -> wf1")
    );

    // lazy status: done → `lazy: status <id>=done`.
    verbs::lazy_update_status(&e.proj, &e.wt, &ctx(), &a, "done").unwrap();
    assert_eq!(
        last_commit_msg(&e.proj.db),
        format!("lazy: status {a}=done")
    );

    // reopen so the row is editable again.
    verbs::reopen(&e.proj, &a).unwrap();

    // lazy edit: `lazy: edit <id>`.
    verbs::lazy_update_title_description(&e.proj, &a, "new title", "new desc").unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), format!("lazy: edit {a}"));

    // lazy priority: `lazy: priority <id>=<p>` (3 differs from the default 0,
    // so the row actually changes and commits).
    verbs::lazy_update_priority(&e.proj, &a, 3).unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), format!("lazy: priority {a}=3"));

    // lazy metadata (replace_all): `lazy: metadata <id>`.
    let r = verbs::metadata_set(
        &e.proj,
        Source::Lazy,
        &a,
        "",
        serde_json::json!({"k": "v"}),
        true,
    )
    .unwrap();
    assert!(r.changed);
    assert_eq!(last_commit_msg(&e.proj.db), format!("lazy: metadata {a}"));

    // lazy comment: `lazy: comment <id>`.
    verbs::comment_add(&e.proj, Source::Lazy, &a, "ann", "hi").unwrap();
    assert_eq!(last_commit_msg(&e.proj.db), format!("lazy: comment {a}"));
}

#[test]
fn sql_query_and_exec() {
    let e = env();
    let a = mk(&e, "t");
    let rows = verbs::sql_query(&e.proj, "SELECT id, status FROM tasks").unwrap();
    assert_eq!(rows.columns, vec!["id".to_string(), "status".to_string()]);
    assert_eq!(rows.rows.len(), 1);
    assert_eq!(rows.rows[0][0], serde_json::json!(a));
    let n = verbs::sql_exec(&e.proj, "UPDATE tasks SET priority = 0").unwrap();
    assert_eq!(n, 1);
}

// ---- helpers --------------------------------------------------------------

fn mk(e: &Env, title: &str) -> String {
    verbs::create(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Cli,
        CreateParams {
            title: title.into(),
            caller: "human".into(),
            ..Default::default()
        },
    )
    .unwrap()
    .id
}

const WF_JSON: &str = r#"{
  "name": "wf1",
  "first_step": "do",
  "isolation": "none",
  "steps": {
    "do": {
      "agent": { "name": "human" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "done" } ]
    }
  }
}"#;

#[test]
fn compact_succeeds_and_returns_stats() {
    // `gc` / maint.compact must succeed and return parseable stats with a
    // verbatim dolt_gc() reply (chunk counts are non-negative).
    let e = env();
    let g = verbs::compact(&e.proj).expect("compact");
    assert!(g.chunks_removed >= 0 && g.chunks_kept >= 0);
    assert!(!g.raw.is_empty(), "dolt_gc() returns a non-empty reply");
}

#[test]
fn step_next_without_active_run_maps_cli_error() {
    // No daemon_runs row exists → emit returns NoActiveRun, which the verb maps
    // to the byte-identical CLI-final message (task id + daemon hint).
    let e = env();
    let view = verbs::create(
        &e.proj,
        &e.packages,
        &e.wt,
        &ctx(),
        Source::Cli,
        CreateParams {
            title: "t".into(),
            priority: 2,
            ..Default::default()
        },
    )
    .unwrap();
    let err = verbs::step_next(&e.proj, &view.id, "done").unwrap_err();
    assert_eq!(
        err.to_string(),
        format!(
            "no active run for task {} (is the daemon running it?)",
            view.id
        )
    );
}

#[test]
fn marshal_agent_params_omits_empty() {
    // Defensive check that empty arrays collapse away (byte-parity with Go's
    // omitempty) — see wfcrud::marshal_agent_params.
    let p = wfcrud::AgentParams {
        model: Some("sonnet".into()),
        ..Default::default()
    };
    let s = wfcrud::marshal_agent_params(Some(&p)).unwrap();
    assert_eq!(s, r#"{"model":"sonnet"}"#);
}
