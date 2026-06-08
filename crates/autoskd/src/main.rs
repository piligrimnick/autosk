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

use autoskd::daemon::{Daemon, DaemonConfig};
use autoskd::server::Server;
use autoskd::uds;

const VERSION: &str = match option_env!("AUTOSKD_VERSION") {
    Some(v) => v,
    None => "0.2.0-phase2",
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
    let mut tcp_addr: Option<String> = None;
    let mut cfg = DaemonConfig::default();
    let mut it = args.into_iter();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--sock" => sock_override = it.next(),
            "--tcp" => tcp_addr = it.next(),
            s if s.starts_with("--tcp=") => tcp_addr = Some(s["--tcp=".len()..].to_string()),
            s if s.starts_with("--sock=") => sock_override = Some(s["--sock=".len()..].to_string()),
            "--workers" => {
                if let Some(v) = it.next().and_then(|s| s.parse().ok()) {
                    cfg.workers = v;
                }
            }
            s if s.starts_with("--workers=") => {
                if let Ok(v) = s["--workers=".len()..].parse() {
                    cfg.workers = v;
                }
            }
            "--gc-interval" => cfg.gc_interval = it.next().and_then(|s| parse_gc(&s)),
            s if s.starts_with("--gc-interval=") => {
                cfg.gc_interval = parse_gc(&s["--gc-interval=".len()..])
            }
            "--pi-bin" => {
                if let Some(v) = it.next() {
                    cfg.pi_bin = v;
                }
            }
            s if s.starts_with("--pi-bin=") => cfg.pi_bin = s["--pi-bin=".len()..].to_string(),
            "--no-exec" => cfg.no_exec = true,
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
    // `AUTOSK_NO_EXEC=1` is the env equivalent of `--no-exec` (test harness):
    // serve reads+writes but never auto-dispatch workflow steps. Auto-spawned
    // daemons inherit it via the client's os.Environ().
    if matches!(
        std::env::var("AUTOSK_NO_EXEC").ok().as_deref(),
        Some("1") | Some("true")
    ) {
        cfg.no_exec = true;
    }
    let mgr = Arc::new(Manager::new());
    let daemon = Daemon::new(mgr, registry, cfg);
    eprintln!("autoskd {VERSION}: listening on {}", sock.display());

    // TCP transport (remote, token auth) is opt-in via --tcp HOST:PORT.
    let token = match autoskd::token::ensure_token() {
        Ok(t) => Some(t),
        Err(e) => {
            eprintln!("autoskd: token: {e}");
            None
        }
    };
    let server = Arc::new(Server::new(Arc::clone(&daemon)).with_token(token));

    // Idle-shutdown watchdog (plan §4.2): exit once no clients AND no running
    // jobs AND no `status='work'` tasks persist past the idle window. Disabled
    // for the TCP-service mode (a remote daemon is a long-lived service).
    if tcp_addr.is_none() {
        start_idle_watchdog(Arc::clone(&daemon), idle_window());
    }
    if let Some(addr) = tcp_addr {
        match std::net::TcpListener::bind(&addr) {
            Ok(tl) => {
                eprintln!("autoskd: TCP listening on {addr} (token auth)");
                let srv = Arc::clone(&server);
                std::thread::spawn(move || srv.serve_tcp(tl));
            }
            Err(e) => eprintln!("autoskd: bind tcp {addr}: {e}"),
        }
    }
    server.serve(listener);
    // serve() returns only when the listener closes.
    daemon.shutdown();
    uds::cleanup(&sock);
    0
}

/// The idle-shutdown window. Default 30 min; override via `AUTOSK_IDLE_SECS`
/// (`0` disables). Mirrors the poller-grace style of the Go daemon.
fn idle_window() -> Option<std::time::Duration> {
    match std::env::var("AUTOSK_IDLE_SECS")
        .ok()
        .and_then(|s| s.parse::<u64>().ok())
    {
        Some(0) => None,
        Some(n) => Some(std::time::Duration::from_secs(n)),
        None => Some(std::time::Duration::from_secs(30 * 60)),
    }
}

/// Spawns the idle-shutdown watchdog thread. When the daemon has no connected
/// clients and no pending work for a full `window`, it shuts down and exits so
/// the next client transparently respawns it.
fn start_idle_watchdog(daemon: Arc<Daemon>, window: Option<std::time::Duration>) {
    let Some(window) = window else { return };
    std::thread::spawn(move || {
        let mut idle_since: Option<std::time::Instant> = None;
        loop {
            std::thread::sleep(std::time::Duration::from_secs(10));
            // "No connected clients" counts EVERY live connection (UDS + TCP),
            // not just the notification-subscribed subset — a client holding a
            // bare connection (e.g. a finished job.subscribe stream) must keep
            // the daemon alive (plan §4.2).
            let busy = daemon.live_connections() > 0 || daemon.has_pending_work();
            if busy {
                idle_since = None;
                continue;
            }
            match idle_since {
                None => idle_since = Some(std::time::Instant::now()),
                Some(t) if t.elapsed() >= window => {
                    eprintln!("autoskd: idle for {:?}; shutting down", window);
                    daemon.shutdown();
                    std::process::exit(0);
                }
                Some(_) => {}
            }
        }
    });
}

/// Parses a `--gc-interval` value: `0` (and negatives) → disabled (`None`);
/// `<n>` or `<n>s` → seconds; `<n>m` → minutes; `<n>h` → hours.
fn parse_gc(s: &str) -> Option<std::time::Duration> {
    let s = s.trim();
    if s.is_empty() {
        return Some(std::time::Duration::from_secs(30 * 60));
    }
    if s.starts_with('-') || s == "0" {
        return None;
    }
    let (num, mult) = if let Some(n) = s.strip_suffix('h') {
        (n, 3600)
    } else if let Some(n) = s.strip_suffix('m') {
        (n, 60)
    } else if let Some(n) = s.strip_suffix('s') {
        (n, 1)
    } else {
        (s, 1)
    };
    num.trim()
        .parse::<u64>()
        .ok()
        .map(|v| std::time::Duration::from_secs(v * mult))
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
