//! Phase 0 acceptance: the RwLock GC discipline survives a `dolt_gc()` running
//! concurrently with a heavy read load — no errors, no corruption — over
//! >= 10k read iterations.
//!
//! This is the mechanism that replaces Go's best-effort inode revalidation and
//! closes the ~1e-7 mid-statement GC race outright (plan §1, §9 Phase 0). The
//! compactor holds the exclusive write guard for the full `dolt_gc()` (and then
//! retires pooled reader connections whose fds point at the orphaned pre-GC
//! inode); every read holds a shared read guard for its whole statement, so no
//! read can ever overlap the atomic rename.
//!
//! Runs against a fresh 0.11.8-native db (the Go-0.10.8 fixture is unreadable
//! under 0.11.8 — see `format_compat.rs` and task ask-8037b4).

mod common;

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::RecvTimeoutError;
use std::sync::{Arc, Barrier, Mutex};
use std::thread;
use std::time::Duration;

use autosk_core::Db;

const READER_THREADS: usize = 8;
const READS_PER_THREAD: u64 = 1_500; // 8 * 1500 = 12_000 reads (>= 10k required)
const GC_CYCLES: usize = 40;
const CHURN_ROWS_PER_CYCLE: usize = 50;
/// Watchdog ceiling for the whole stress run. It completes in ~1s on macOS arm64
/// and has generous headroom for slow CI. `std::sync::RwLock` fairness is
/// unspecified (see the discipline notes in `src/store.rs`): a reader-preferring
/// implementation could starve the GC writer, which would otherwise HANG the
/// `join()` forever. This bound turns that starvation regression into a test
/// FAILURE — a usable CI signal — instead of a hang.
const STRESS_TIMEOUT: Duration = Duration::from_secs(120);

#[test]
fn gc_under_concurrent_reads_is_race_free() {
    let (_guard, db_path) = common::fresh_native_db();
    let db = Arc::new(Db::open(&db_path).expect("open fresh 0.11.8 db"));

    // The immutable snapshot every reader must always observe, no matter how
    // many GCs/rewrites happen underneath. Read once up front.
    let golden = db.list_tasks().expect("baseline tasks");
    assert_eq!(golden.len(), 5, "seed should have 5 tasks");

    // Scratch table for churn so each gc has live work to reclaim.
    db.with_write(|conn| {
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS spike_churn (id INTEGER PRIMARY KEY, payload TEXT);",
        )?;
        Ok(())
    })
    .expect("create churn table");
    db.commit("spike: churn table").expect("commit churn table");

    let errors: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let reads_done = Arc::new(AtomicU64::new(0));
    let gc_done = Arc::new(AtomicU64::new(0));
    // +1 for the GC thread.
    let barrier = Arc::new(Barrier::new(READER_THREADS + 1));

    let mut handles = Vec::new();

    // Reader threads: hammer SELECTs under read guards and assert the golden
    // snapshot, plus a self-consistency check on the churn table.
    for t in 0..READER_THREADS {
        let db = Arc::clone(&db);
        let golden = golden.clone();
        let errors = Arc::clone(&errors);
        let reads_done = Arc::clone(&reads_done);
        let barrier = Arc::clone(&barrier);
        handles.push(thread::spawn(move || {
            barrier.wait();
            for i in 0..READS_PER_THREAD {
                // Full task read under a read guard (Db::list_tasks).
                match db.list_tasks() {
                    Ok(rows) => {
                        if rows != golden {
                            errors.lock().unwrap().push(format!(
                                "reader {t} iter {i}: tasks mismatch (got {} rows)",
                                rows.len()
                            ));
                            return;
                        }
                    }
                    Err(e) => {
                        errors
                            .lock()
                            .unwrap()
                            .push(format!("reader {t} iter {i}: list_tasks: {e}"));
                        return;
                    }
                }
                // A second read under the same discipline against the churning
                // table: a corrupt/half-renamed DB would error here.
                let churn = db.with_read(|conn| {
                    let n: i64 =
                        conn.query_row("SELECT count(*) FROM spike_churn", [], |r| r.get(0))?;
                    Ok(n)
                });
                if let Err(e) = churn {
                    errors
                        .lock()
                        .unwrap()
                        .push(format!("reader {t} iter {i}: churn count: {e}"));
                    return;
                }
                reads_done.fetch_add(1, Ordering::Relaxed);
            }
        }));
    }

    // GC thread: churn + commit + dolt_gc() under the write guard, repeatedly.
    {
        let db = Arc::clone(&db);
        let errors = Arc::clone(&errors);
        let gc_done = Arc::clone(&gc_done);
        let barrier = Arc::clone(&barrier);
        handles.push(thread::spawn(move || {
            barrier.wait();
            for c in 0..GC_CYCLES {
                let write = db.with_write(|conn| {
                    for r in 0..CHURN_ROWS_PER_CYCLE {
                        conn.execute(
                            "INSERT INTO spike_churn (payload) VALUES (?1)",
                            [format!("c{c}-r{r}-{}", "x".repeat(48))],
                        )?;
                    }
                    Ok(())
                });
                if let Err(e) = write {
                    errors
                        .lock()
                        .unwrap()
                        .push(format!("gc cycle {c}: churn: {e}"));
                    return;
                }
                if let Err(e) = db.commit(&format!("spike: cycle {c}")) {
                    errors
                        .lock()
                        .unwrap()
                        .push(format!("gc cycle {c}: commit: {e}"));
                    return;
                }
                match db.gc() {
                    Ok(_) => {
                        gc_done.fetch_add(1, Ordering::Relaxed);
                    }
                    Err(e) => {
                        errors
                            .lock()
                            .unwrap()
                            .push(format!("gc cycle {c}: dolt_gc: {e}"));
                        return;
                    }
                }
            }
        }));
    }

    // Join with a watchdog: hand the handles to a joiner thread that signals a
    // channel once every worker has finished, and bound the wait. A writer
    // starvation regression (GC never acquiring the write guard) would block the
    // joiner forever; the timeout converts that into a loud failure here.
    let (done_tx, done_rx) = std::sync::mpsc::channel();
    let joiner = thread::spawn(move || {
        for h in handles {
            h.join().expect("thread panicked");
        }
        let _ = done_tx.send(());
    });
    match done_rx.recv_timeout(STRESS_TIMEOUT) {
        Ok(()) => joiner.join().expect("joiner thread"),
        // The joiner dropped its sender without sending => a worker thread
        // panicked; re-surface that panic via join() for a precise message.
        Err(RecvTimeoutError::Disconnected) => joiner.join().expect("worker thread panicked"),
        Err(RecvTimeoutError::Timeout) => panic!(
            "stress run did not finish within {STRESS_TIMEOUT:?} \
             — likely GC-writer starvation (RwLock fairness regression); \
             completed {} of {GC_CYCLES} gc cycles",
            gc_done.load(Ordering::Relaxed)
        ),
    }

    let errs = errors.lock().unwrap();
    assert!(
        errs.is_empty(),
        "stress produced errors:\n{}",
        errs.join("\n")
    );

    let total_reads = reads_done.load(Ordering::Relaxed);
    assert!(
        total_reads >= 10_000,
        "expected >= 10k read iterations, got {total_reads}"
    );
    assert_eq!(
        gc_done.load(Ordering::Relaxed) as usize,
        GC_CYCLES,
        "not all gc cycles completed"
    );

    // Final consistency: tasks unchanged, churn fully landed, fresh open clean.
    let final_tasks = db.list_tasks().expect("final tasks read");
    assert_eq!(final_tasks, golden, "tasks diverged after the stress run");

    let final_churn: i64 = db
        .with_read(|conn| Ok(conn.query_row("SELECT count(*) FROM spike_churn", [], |r| r.get(0))?))
        .expect("final churn read");
    assert_eq!(
        final_churn as usize,
        GC_CYCLES * CHURN_ROWS_PER_CYCLE,
        "churn rows lost across the gc cycles"
    );

    // Reopen from scratch (post-rename path) — the ultimate corruption check.
    let reopened = Db::open(&db_path).expect("reopen after stress");
    assert_eq!(
        reopened.list_tasks().expect("reopen tasks"),
        golden,
        "reopened DB diverged after the stress run"
    );
}
