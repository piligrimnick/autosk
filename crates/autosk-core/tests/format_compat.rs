//! Phase 0 finding: doltlite's on-disk container format is **NOT** compatible
//! across the 0.10 -> 0.11 boundary, so a Go-produced (0.10.8) `.autosk/db`
//! does NOT open under Rust/0.11.8.
//!
//! This falsified the plan's original forward-compat assumption; the plan now
//! records the finding (`docs/plans/20260607-Rust-Daemon-Tauri-GUI.md`
//! §2.1/§9/§10, with the migration decision tracked in ask-8037b4). doltlite
//! 0.11.0 is a documented breaking on-disk change ("the on-disk refs format moves from
//! v6 to v7 ... Repositories created or rewritten under 0.11.0 cannot be opened
//! by earlier doltlite builds"); empirically the break is mutual — 0.11.8 also
//! refuses the older container.
//!
//! These tests are therefore **characterization tests**: they pin the actual
//! behaviour (the v11/v12 split and the hard open failure) so it can't silently
//! change, and so the gating migration decision (follow-up task ask-8037b4) is
//! anchored to a reproducible fact. See the task's "PHASE 0 FINDING" comment.

mod common;

use autosk_core::{Db, Error};

#[test]
fn linked_engine_is_doltlite_prolly() {
    // Guards the build wiring: if libdoltlite.a is not linked, this fails with
    // "no such function: doltlite_engine". Mirrors the Go TestDoltliteEngine.
    let engine = autosk_core::doltlite_engine().expect("query doltlite_engine()");
    assert_eq!(
        engine, "prolly",
        "doltlite engine not active (link wiring?)"
    );
}

#[test]
fn go_0_10_8_fixture_is_container_format_v11() {
    let v = common::container_format_version(&common::fixture_db())
        .expect("fixture is a doltlite CTLD container");
    assert_eq!(
        v, 11,
        "Go/0.10.8 fixture should be doltlite container format v11"
    );
}

#[test]
fn rust_0_11_8_writes_container_format_v12() {
    let (_guard, path) = common::fresh_native_db();
    let v = common::container_format_version(&path).expect("fresh db is a doltlite CTLD container");
    assert_eq!(
        v, 12,
        "Rust/0.11.8 should write doltlite container format v12"
    );
}

#[test]
fn go_0_10_8_db_is_not_readable_under_0_11_8() {
    // The load-bearing negative result: opening the v11 fixture under 0.11.8
    // fails with SQLITE_NOTADB. If a future doltlite bump makes this SUCCEED,
    // this test will fail loudly — which is the signal to revisit ask-8037b4
    // (forward-compat may have been restored, simplifying the migration story).
    let (_guard, db_path) = common::writable_fixture();
    match Db::open(&db_path) {
        Ok(db) => {
            // Open may be lazy; force a read to be certain.
            match db.list_tasks() {
                Ok(rows) => panic!(
                    "EXPECTED 0.11.8 to reject the v11 fixture, but it read {} task rows \
                     — forward-compat may have been restored; revisit ask-8037b4",
                    rows.len()
                ),
                Err(Error::Sqlite(e)) => assert_not_a_database(&e),
                Err(other) => panic!("unexpected error reading v11 fixture: {other}"),
            }
        }
        Err(Error::Sqlite(e)) => assert_not_a_database(&e),
        Err(other) => panic!("unexpected error opening v11 fixture: {other}"),
    }
}

fn assert_not_a_database(e: &rusqlite::Error) {
    let code = match e {
        rusqlite::Error::SqliteFailure(err, _) => err.code,
        _ => panic!("expected SqliteFailure, got: {e}"),
    };
    assert_eq!(
        code,
        rusqlite::ffi::ErrorCode::NotADatabase,
        "expected SQLITE_NOTADB opening a v11 db under 0.11.8, got {e}"
    );
}
