//! Shared helpers for the Phase 0 integration tests.

#![allow(dead_code)] // each integration test uses a different subset of these.

use std::io::Read;
use std::path::{Path, PathBuf};

use rusqlite::Connection;
use tempfile::TempDir;

/// Path to the committed Go-0.10.8 forward-compat fixture db (doltlite container
/// format v11). Used by the characterization test that pins the v11/v12 break.
pub fn fixture_db() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/go-0.10.8/db")
}

/// The canonical SEED row set for freshly created 0.11.8 dbs (see
/// [`fresh_native_db`]), as TSV: id, title, status, priority.
///
/// It is *incidentally* the original contents of the Go-0.10.8 fixture (both
/// were produced together by `scripts/make-forward-compat-fixture.sh`), but
/// since the criterion-#2 pivot NO test reads the fixture db's rows or compares
/// them against this file: `format_compat.rs` only checks the fixture's `CTLD`
/// header and that the open fails. So this is seed data, not a snapshot any test
/// verifies against the fixture — the two can drift without a test catching it.
pub const TASKS_GOLDEN: &str = include_str!("../fixtures/go-0.10.8/tasks.golden");

/// Parses [`TASKS_GOLDEN`] into `(id, title, status, priority)` tuples.
pub fn golden_rows() -> Vec<(String, String, String, i64)> {
    TASKS_GOLDEN
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|line| {
            let mut cols = line.split('\t');
            let id = cols.next().expect("golden: id").to_string();
            let title = cols.next().expect("golden: title").to_string();
            let status = cols.next().expect("golden: status").to_string();
            let priority = cols
                .next()
                .expect("golden: priority")
                .parse()
                .expect("golden: priority int");
            (id, title, status, priority)
        })
        .collect()
}

/// Copies the read-only committed fixture into a fresh temp dir. (Only useful
/// for tests that expect the open to FAIL — 0.11.8 cannot read the v11 file.)
pub fn writable_fixture() -> (TempDir, PathBuf) {
    let dir = tempfile::tempdir().expect("create tempdir");
    let dst = dir.path().join("db");
    std::fs::copy(fixture_db(), &dst).expect("copy fixture db");
    (dir, dst)
}

/// Creates a fresh doltlite-0.11.8 (container format v12) `.autosk/db`-shaped
/// database in a temp dir, seeded with [`golden_rows`] and committed. The
/// gc/stress tests run against this rather than the v11 fixture, which 0.11.8
/// cannot open (see `format_compat.rs`). The schema is a narrow stand-in for the
/// real `tasks` table — Phase 0 deliberately does not port migrations (Phase 1).
pub fn fresh_native_db() -> (TempDir, PathBuf) {
    let dir = tempfile::tempdir().expect("create tempdir");
    let path = dir.path().join("db");
    let conn = Connection::open(&path).expect("create 0.11.8 db");
    let engine: String = conn
        .query_row("SELECT doltlite_engine()", [], |r| r.get(0))
        .expect("doltlite_engine");
    assert_eq!(engine, "prolly", "fresh db is not doltlite-backed");
    conn.execute_batch(
        "CREATE TABLE tasks (\n             id       TEXT PRIMARY KEY,\n             title    TEXT NOT NULL,\n             status   TEXT NOT NULL,\n             priority INTEGER NOT NULL\n         );",
    )
    .expect("create tasks");
    for (id, title, status, priority) in golden_rows() {
        conn.execute(
            "INSERT INTO tasks (id, title, status, priority) VALUES (?1, ?2, ?3, ?4)",
            rusqlite::params![id, title, status, priority],
        )
        .expect("seed task");
    }
    let _: String = conn
        .query_row("SELECT dolt_commit('-A','-m','seed')", [], |r| r.get(0))
        .expect("seed commit");
    drop(conn);
    (dir, path)
}

/// Reads doltlite's `CTLD` container header and returns the format version
/// (the little-endian u32 at byte offset 4), or `None` if the file is not a
/// doltlite container.
pub fn container_format_version(path: &Path) -> Option<u32> {
    let mut f = std::fs::File::open(path).ok()?;
    let mut buf = [0u8; 8];
    f.read_exact(&mut buf).ok()?;
    if &buf[0..4] != b"CTLD" {
        return None;
    }
    Some(u32::from_le_bytes([buf[4], buf[5], buf[6], buf[7]]))
}
