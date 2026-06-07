//! Schema golden (plan §8.3, acceptance criterion #3).
//!
//! The Rust migrator, run on a FRESH (Rust/0.11.8-created, container v12) DB,
//! must yield the **exact** schema the Go `001_init.sql` produced: the same
//! `schema_version`, the same table/index DDL, and the same CHECK constraints.
//!
//! autoskd is greenfield — there is no shared on-disk DB to diff against Go —
//! so this is a port-correctness gate: every CREATE statement in the embedded
//! `001_init.sql` (run through the same comment-strip + split pipeline the
//! migrator uses) must appear verbatim (modulo whitespace) in the migrated DB's
//! `sqlite_master`, and the migrated DB must report `schema_version = 1`.

use std::collections::BTreeSet;

use autosk_core::{migrate, Db};

/// The schema source, embedded the same way the migrator embeds it.
const INIT_SQL: &str = include_str!("../../../internal/migrations/001_init.sql");

fn fresh_migrated_db() -> (tempfile::TempDir, Db) {
    let dir = tempfile::tempdir().expect("tempdir");
    let path = dir.path().join("db");
    let db = Db::open_or_create(&path).expect("create v12 db");
    db.migrate().expect("migrate");
    (dir, db)
}

/// Strips whitespace that is OUTSIDE single-quoted string literals. doltlite
/// re-serialises the CREATE text when it stores it in `sqlite_master` (it drops
/// the spaces around `(`/`)`/`<>`, e.g. `agents (` becomes `agents(` and
/// `IN (0,1)` becomes `IN(0,1)`), and it does so deterministically — so a
/// Go-created v12 DB would store the identical text. The only differences
/// between the embedded `001_init.sql` and the stored DDL are therefore
/// whitespace, and removing it makes the comparison faithful to "same DDL +
/// same CHECK".
///
/// Whitespace inside a quoted literal is PRESERVED so the comparison cannot
/// silently equate two distinct literals: today no CHECK literal contains a
/// space, but a future `IN ('foo bar')` would otherwise collapse to `'foobar'`
/// and pass a golden it should fail. SQL escapes a quote inside a literal by
/// doubling it (`''`), which this scanner handles by toggling on every quote —
/// a doubled quote toggles off then on, leaving us correctly "inside".
fn normalize(sql: &str) -> String {
    let mut out = String::with_capacity(sql.len());
    let mut in_quote = false;
    for c in sql.chars() {
        if c == '\'' {
            in_quote = !in_quote;
            out.push(c);
        } else if in_quote || !c.is_whitespace() {
            out.push(c);
        }
    }
    out
}

#[test]
fn migrator_reports_schema_version_1() {
    let (_g, db) = fresh_migrated_db();
    assert_eq!(
        db.schema_version().expect("schema_version"),
        migrate::LATEST_VERSION
    );
    assert_eq!(migrate::LATEST_VERSION, 1);
}

#[test]
fn migrated_ddl_matches_001_init_sql() {
    let (_g, db) = fresh_migrated_db();

    // What the fresh DB actually stored, from sqlite_master (explicit DDL only;
    // auto unique-indexes have NULL sql; sqlite_* internal objects excluded).
    let actual: BTreeSet<String> = db
        .with_read(|conn| {
            let mut stmt = conn.prepare(
                "SELECT sql FROM sqlite_master \
                   WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' \
                   ORDER BY type, name",
            )?;
            let rows = stmt.query_map([], |r| r.get::<_, String>(0))?;
            let mut out = BTreeSet::new();
            for r in rows {
                out.insert(normalize(&r?));
            }
            Ok(out)
        })
        .expect("read sqlite_master");

    // What 001_init.sql says, via the migrator's own split pipeline.
    let expected_init: BTreeSet<String> = migrate::split_statements(INIT_SQL)
        .into_iter()
        .map(|s| normalize(&s))
        .filter(|s| s.starts_with("CREATE"))
        .collect();

    // Every CREATE in 001_init.sql must be present verbatim in the migrated DB.
    for stmt in &expected_init {
        assert!(
            actual.contains(stmt),
            "001_init.sql statement missing from migrated schema:\n  {stmt}\n\nactual schema:\n{}",
            actual.iter().cloned().collect::<Vec<_>>().join("\n")
        );
    }

    // The only extra DDL the migrator adds beyond 001_init.sql is the
    // schema_migrations tracking table (EnsureTrackingTable, by design kept out
    // of 001_init.sql). Nothing else may differ.
    let extra: Vec<String> = actual.difference(&expected_init).cloned().collect();
    assert_eq!(
        extra,
        vec![normalize(
            "CREATE TABLE schema_migrations ( version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL )"
        )],
        "migrated schema has unexpected extra DDL vs 001_init.sql + schema_migrations"
    );

    // Spot-check the load-bearing CHECK invariant survived (plan §2.1). The
    // stored text is whitespace-stripped, so compare against the stripped form.
    let tasks_ddl = actual
        .iter()
        .find(|s| s.contains("CREATETABLEtasks"))
        .expect("tasks DDL present");
    assert!(
        tasks_ddl.contains(&normalize(
            "status = 'work' AND current_step_id IS NOT NULL"
        )),
        "tasks CHECK invariant not preserved: {tasks_ddl}"
    );
    assert!(
        tasks_ddl.contains(&normalize(
            "CHECK (status IN ('new','work','human','done','cancel'))"
        )),
        "tasks status enum CHECK not preserved: {tasks_ddl}"
    );
}

#[test]
fn migrate_is_idempotent() {
    let (_g, db) = fresh_migrated_db();
    // A second migrate must be a no-op (schema_version stays 1, no error).
    assert_eq!(db.migrate().expect("second migrate"), 1);
    assert_eq!(db.schema_version().expect("schema_version"), 1);
    // The human agent is seeded exactly once.
    let humans: i64 = db
        .with_read(|conn| {
            Ok(conn.query_row(
                "SELECT COUNT(*) FROM agents WHERE name = 'human' AND is_human = 1",
                [],
                |r| r.get(0),
            )?)
        })
        .expect("count human");
    assert_eq!(humans, 1, "human agent seeded exactly once");
}
