//! `autoskd` — the Rust daemon that will become the sole owner of `.autosk/db`.
//!
//! Phase 0 stub. The JSON-RPC server, poller/scheduler/compactor and executor
//! (plan §3-§5, §7.2) are implemented in later phases. This entrypoint only
//! reports the linked doltlite engine so the daemon binary is proven to link
//! `autosk-core` (and through it `libdoltlite.a`) end to end.

fn main() {
    match autosk_core::doltlite_engine() {
        Ok(engine) => println!("autoskd (phase-0 stub): doltlite engine = {engine}"),
        Err(err) => {
            eprintln!("autoskd: failed to query doltlite engine: {err}");
            std::process::exit(1);
        }
    }
}
