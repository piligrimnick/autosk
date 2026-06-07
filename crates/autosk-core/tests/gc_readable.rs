//! Phase 0 acceptance: `dolt_commit` / `dolt_gc` succeed on doltlite 0.11.8 and
//! the DB stays readable afterwards.
//!
//! This pins the regression that forced the Go side off 0.10.11: that version
//! corrupted the schema cookie after `dolt_gc()`'s atomic rename, leaving the
//! file unreadable to a fresh open ("malformed database schema ... invalid
//! rootpage"). doltlite 0.11.2 fixed exactly this; here we prove a GC leaves
//! `tasks` readable on 0.11.8.
//!
//! Runs against a fresh 0.11.8-native db, not the Go-0.10.8 fixture (which
//! 0.11.8 cannot open — see `format_compat.rs` and task ask-8037b4).

mod common;

use autosk_core::Db;

#[test]
fn commit_then_gc_keeps_db_readable() {
    let (_guard, db_path) = common::fresh_native_db();
    let db = Db::open(&db_path).expect("open fresh 0.11.8 db");

    // Sanity: readable before any maintenance.
    let before = db.list_tasks().expect("read tasks before gc");
    assert_eq!(before.len(), 5, "seed should have 5 tasks");

    // Generate some churn so gc has chunks to reclaim, then commit it.
    db.with_write(|conn| {
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS spike_churn (id INTEGER PRIMARY KEY, payload TEXT);",
        )?;
        for i in 0..200 {
            conn.execute(
                "INSERT INTO spike_churn (payload) VALUES (?1)",
                [format!("row-{i}-{}", "x".repeat(64))],
            )?;
        }
        Ok(())
    })
    .expect("churn writes");

    let hash = db.commit("spike").expect("dolt_commit");
    assert!(!hash.is_empty(), "expected a non-empty changeset to commit");

    // The load-bearing call: dolt_gc() rewrites via atomic rename.
    let stats = db.gc().expect("dolt_gc() must succeed on 0.11.8");
    assert!(
        !stats.raw.is_empty(),
        "dolt_gc() returned empty output: {stats:?}"
    );
    assert!(
        stats.chunks_kept > 0,
        "expected a non-empty working set after gc: {stats:?}"
    );

    // The 0.10.11 corruption would surface right here as a read error after the
    // post-gc rename. On 0.11.8 the DB must stay fully readable.
    let after = db
        .list_tasks()
        .expect("read tasks AFTER gc (corruption check)");
    assert_eq!(
        after, before,
        "tasks changed across gc; the immutable rows must survive a rewrite"
    );

    // And a brand-new reader connection (fresh open at the post-rename path)
    // must also see a consistent DB — this is the cross-open check the 0.10.11
    // schema-cookie bug failed.
    let reopened = Db::open(&db_path).expect("reopen after gc");
    let after_reopen = reopened.list_tasks().expect("read tasks after reopen");
    assert_eq!(after_reopen, before, "reopened DB diverged after gc");

    // A second gc on the now-quiescent DB must be a no-op (matches the Go
    // TestCompact_FreshDB invariant: a regression that reclaims live chunks
    // would trip here).
    let stats2 = db.gc().expect("second dolt_gc()");
    assert_eq!(
        stats2.chunks_removed, 0,
        "quiescent gc removed {} chunks (raw={:?})",
        stats2.chunks_removed, stats2.raw
    );
}
