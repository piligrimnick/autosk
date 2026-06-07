//! `autoskd` — the Rust daemon, sole owner of `.autosk/db` (plan §3).
//!
//! Phase 1 surface:
//!   * `autoskd [serve] [--sock PATH]` — bind the single-instance UDS and serve
//!     the JSON-RPC read surface (the default when no subcommand is given).
//!   * `autoskd init [DIR]` — greenfield: create + migrate `<DIR>/.autosk/db`
//!     (a fresh v12 DB) and register it, so the read surface has something to
//!     serve and `autosk lazy` can be smoke-tested.
//!   * `autoskd version` — print the version.
//!   * `autoskd engine` — print the linked doltlite engine (link smoke test).

use std::path::PathBuf;
use std::sync::Arc;

use autosk_core::projectmgr::Manager;
use autosk_core::registry::Registry;

use autoskd::server::Server;
use autoskd::uds;

const VERSION: &str = match option_env!("AUTOSKD_VERSION") {
    Some(v) => v,
    None => "0.1.0-phase1",
};

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let cmd = args.get(1).map(String::as_str).unwrap_or("serve");
    let rest = if args.len() > 2 { &args[2..] } else { &[] };
    let code = match cmd {
        "serve" => cmd_serve(args.get(2..).map(<[String]>::to_vec).unwrap_or_default()),
        "init" => cmd_init(rest),
        "version" | "--version" | "-V" => {
            println!("{VERSION}");
            0
        }
        "engine" => cmd_engine(),
        "help" | "--help" | "-h" => {
            print_usage();
            0
        }
        other => {
            eprintln!("autoskd: unknown command {other:?}\n");
            print_usage();
            2
        }
    };
    std::process::exit(code);
}

fn print_usage() {
    eprintln!(
        "usage:\n  \
         autoskd [serve] [--sock PATH]   serve the JSON-RPC read surface (default)\n  \
         autoskd init [DIR]              create + migrate <DIR>/.autosk/db (greenfield)\n  \
         autoskd version                 print version\n  \
         autoskd engine                  print the linked doltlite engine"
    );
}

fn cmd_serve(args: Vec<String>) -> i32 {
    let mut sock_override: Option<String> = None;
    let mut it = args.into_iter();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--sock" => sock_override = it.next(),
            s if s.starts_with("--sock=") => sock_override = Some(s["--sock=".len()..].to_string()),
            other => {
                eprintln!("autoskd serve: unexpected argument {other:?}");
                return 2;
            }
        }
    }
    let sock = match resolve_sock(sock_override) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("autoskd: resolve socket: {e}");
            return 1;
        }
    };

    let listener = match uds::listen(&sock) {
        Ok(l) => l,
        Err(uds::ListenError::AlreadyRunning) => {
            // Single-instance: another daemon won the bind race. The connecting
            // client will use it; exit cleanly so a double-spawn is harmless.
            eprintln!("autoskd: already running at {}", sock.display());
            return 0;
        }
        Err(e) => {
            eprintln!("autoskd: bind {}: {e}", sock.display());
            return 1;
        }
    };

    let registry = match Registry::open_default() {
        Ok(r) => Arc::new(r),
        Err(e) => {
            eprintln!("autoskd: open registry: {e}");
            uds::cleanup(&sock);
            return 1;
        }
    };
    let mgr = Arc::new(Manager::new());
    eprintln!("autoskd {VERSION}: listening on {}", sock.display());
    let server = Arc::new(Server::new(mgr, registry));
    server.serve(listener);
    // serve() returns only when the listener closes.
    uds::cleanup(&sock);
    0
}

fn cmd_init(args: &[String]) -> i32 {
    let dir = args.first().cloned().unwrap_or_else(|| ".".to_string());
    match Manager::init(&dir) {
        Ok((root, db_path)) => {
            // Best-effort: register so project.list surfaces it.
            if let Ok(reg) = Registry::open_default() {
                let _ = reg.add(&root, &db_path);
            }
            println!("initialized autosk project");
            println!("  root: {root}");
            println!("  db:   {db_path}");
            0
        }
        Err(e) => {
            eprintln!("autoskd init: {e}");
            1
        }
    }
}

fn cmd_engine() -> i32 {
    match autosk_core::doltlite_engine() {
        Ok(engine) => {
            println!("doltlite engine = {engine}");
            0
        }
        Err(e) => {
            eprintln!("autoskd: query doltlite engine: {e}");
            1
        }
    }
}

/// Resolves the UDS path: explicit `--sock` → `$AUTOSK_SOCK` →
/// `~/.autosk/daemon.sock` (mirrors the Go client's `resolveSock`).
fn resolve_sock(explicit: Option<String>) -> std::io::Result<PathBuf> {
    if let Some(s) = explicit {
        if !s.is_empty() {
            return Ok(PathBuf::from(s));
        }
    }
    if let Ok(env) = std::env::var("AUTOSK_SOCK") {
        if !env.is_empty() {
            return Ok(PathBuf::from(env));
        }
    }
    let home = std::env::var_os("HOME")
        .ok_or_else(|| std::io::Error::new(std::io::ErrorKind::NotFound, "HOME not set"))?;
    Ok(PathBuf::from(home).join(".autosk").join("daemon.sock"))
}
