//! Build script for `autosk-core`.
//!
//! Links the Rust core against doltlite **0.11.8** (the pin the Rust side
//! targets — see `docs/plans/20260607-Rust-Daemon-Tauri-GUI.md` §2.1). It
//! reuses `scripts/fetch-doltlite.sh` so a plain `cargo build` needs no manual
//! doltlite setup: the prebuilt `libdoltlite.a` + `sqlite3.h` are downloaded
//! into the in-tree cache `<workspace>/.doltlite/<version>-<platform>` on first
//! build (identical layout to the Go side's `make fetch-doltlite`).
//!
//! ## How the link is wired
//!
//! `rusqlite`/`libsqlite3-sys` is built with the `bundled` amalgamation
//! DISABLED. With `SQLITE3_STATIC=1` + `SQLITE3_LIB_DIR` set (see
//! `.cargo/config.toml`), libsqlite3-sys bundles a `libsqlite3.a` into its own
//! rlib. That happens at libsqlite3-sys's *own* compile time — which is before
//! this build script can download doltlite, and there is no way to order a
//! build script ahead of a transitive dependency's compilation. So
//! libsqlite3-sys bundles an empty committed shim (`doltlite-shim/`,
//! contributing no symbols), and this script emits
//! `cargo:rustc-link-lib=static=doltlite`: the real `libdoltlite.a` supplies
//! both the standard sqlite3 C API *and* the dolt SQL functions (`dolt_commit`,
//! `dolt_gc`, …) at the final link, with no duplicate symbols. We also add the
//! auxiliary system libraries doltlite needs (`-lz`, and on Linux
//! `-lpthread -lm`), mirroring the Go Makefile's
//! `CGO_LDFLAGS := $(DOLTLITE_DIR)/libdoltlite.a -lz -lpthread -lm`.

use std::path::{Path, PathBuf};
use std::process::Command;

/// doltlite release the Rust side links against. Overridable via the
/// `DOLTLITE_VERSION` env for forward/back testing, but Phase 0 pins 0.11.8.
const DEFAULT_DOLTLITE_VERSION: &str = "0.11.8";

fn main() {
    let manifest_dir = PathBuf::from(env("CARGO_MANIFEST_DIR"));
    // crates/autosk-core -> crates -> <workspace root>
    let workspace_root = manifest_dir
        .parent()
        .and_then(Path::parent)
        .expect("autosk-core must live at <workspace>/crates/autosk-core")
        .to_path_buf();

    let version =
        std::env::var("DOLTLITE_VERSION").unwrap_or_else(|_| DEFAULT_DOLTLITE_VERSION.to_string());

    // The artifact cache (honours the plan's default layout; DOLTLITE_DIR lets a
    // developer point at a locally-built doltlite, like the Go Makefile).
    //
    // Platform detection is *lazy*: it is only needed to name the default cache
    // dir and to tell the fetch script which release asset to download. An
    // explicit DOLTLITE_DIR therefore skips platform detection entirely — that
    // is how a hand-built doltlite on an otherwise-unsupported target (e.g.
    // macOS x86_64) is consumed without tripping the unsupported-target panic.
    let dolt_dir = match std::env::var_os("DOLTLITE_DIR") {
        Some(dir) => PathBuf::from(dir),
        None => workspace_root
            .join(".doltlite")
            .join(format!("{version}-{}", doltlite_platform())),
    };

    ensure_doltlite(&workspace_root, &version, &dolt_dir);

    // Link the real doltlite archive. rustc appends native libs after all
    // rlibs, so `-ldoltlite` resolves the sqlite3 C API symbols referenced by
    // rusqlite/libsqlite3-sys (whose bundled shim is empty), as well as the
    // dolt SQL functions. `libdoltlite.a` exists by now because this build
    // script (which always runs before autosk-core's own compilation) fetched
    // it just above.
    //
    // The fetched cache also ships `libdoltlite.dylib`, and macOS `ld` prefers a
    // `.dylib` over a `.a` of the same name on the same search path — which
    // would link doltlite dynamically (install_name `/usr/local/lib/...`) and
    // crash at runtime. So we point the search at a clean dir that contains
    // ONLY `libdoltlite.a`, forcing the static archive everywhere.
    let link_dir = PathBuf::from(env("OUT_DIR")).join("doltlite-link");
    std::fs::create_dir_all(&link_dir)
        .unwrap_or_else(|e| panic!("autosk-core: mkdir {}: {e}", link_dir.display()));
    symlink_force(
        &dolt_dir.join("libdoltlite.a"),
        &link_dir.join("libdoltlite.a"),
    );
    println!("cargo:rustc-link-search=native={}", link_dir.display());
    println!("cargo:rustc-link-lib=static=doltlite");

    // Auxiliary libraries doltlite pulls in. Matches the Go Makefile. Emitted
    // after `-ldoltlite` so GNU ld (left-to-right) resolves doltlite's
    // references into them.
    println!("cargo:rustc-link-lib=z");
    let target_os = env("CARGO_CFG_TARGET_OS");
    if target_os == "linux" {
        // libdoltlite.a references sqlite3 math functions (log/pow/sin/...) and
        // pthread; on glibc those live in libm/libpthread. macOS folds both
        // into libSystem, so they are no-ops there.
        println!("cargo:rustc-link-lib=pthread");
        println!("cargo:rustc-link-lib=m");
    }

    // Expose the resolved dir to dependents/tests for diagnostics.
    println!("cargo:rustc-env=AUTOSK_DOLTLITE_DIR={}", dolt_dir.display());

    println!("cargo:rerun-if-env-changed=DOLTLITE_DIR");
    println!("cargo:rerun-if-env-changed=DOLTLITE_VERSION");
    println!("cargo:rerun-if-env-changed=DOLTLITE_PLATFORM");
    println!(
        "cargo:rerun-if-changed={}",
        workspace_root.join("scripts/fetch-doltlite.sh").display()
    );
    println!(
        "cargo:rerun-if-changed={}",
        dolt_dir.join("libdoltlite.a").display()
    );
}

/// Resolves the doltlite release asset suffix used by `scripts/fetch-doltlite.sh`
/// (osx-arm64 | linux-x64 | linux-arm64).
///
/// A `DOLTLITE_PLATFORM` env override wins, so a host on an unsupported target
/// triple can still fetch a compatible asset (this is the escape hatch the
/// unsupported-target panic advertises). Otherwise the Cargo target triple is
/// mapped to the asset suffix.
fn doltlite_platform() -> String {
    if let Ok(p) = std::env::var("DOLTLITE_PLATFORM") {
        if !p.is_empty() {
            return p;
        }
    }
    let os = env("CARGO_CFG_TARGET_OS");
    let arch = env("CARGO_CFG_TARGET_ARCH");
    match (os.as_str(), arch.as_str()) {
        ("macos", "aarch64") => "osx-arm64".to_string(),
        ("linux", "x86_64") => "linux-x64".to_string(),
        ("linux", "aarch64") => "linux-arm64".to_string(),
        (other_os, other_arch) => panic!(
            "autosk-core: unsupported doltlite target {other_os}/{other_arch}; \
             set DOLTLITE_DIR to a prebuilt doltlite, or DOLTLITE_PLATFORM to a \
             release-asset platform (osx-arm64 | linux-x64 | linux-arm64)"
        ),
    }
}

/// Ensures `dolt_dir` contains `libdoltlite.a` + `sqlite3.h`, fetching via the
/// shared shell script if absent. Idempotent and network-free once cached.
///
/// Platform detection happens here, lazily, because it is only consulted when a
/// fetch is actually required — a populated cache (or DOLTLITE_DIR) never needs
/// it.
fn ensure_doltlite(workspace_root: &Path, version: &str, dolt_dir: &Path) {
    if dolt_dir.join("libdoltlite.a").is_file() && dolt_dir.join("sqlite3.h").is_file() {
        return;
    }
    let platform = doltlite_platform();
    let script = workspace_root.join("scripts/fetch-doltlite.sh");
    assert!(
        script.is_file(),
        "autosk-core: {} not found; cannot fetch doltlite {version}",
        script.display()
    );
    eprintln!(
        "autosk-core: fetching doltlite {version} ({platform}) into {}",
        dolt_dir.display()
    );
    let status = Command::new("bash")
        .arg(&script)
        .arg(version)
        .arg(dolt_dir)
        .env("DOLTLITE_PLATFORM", &platform)
        .status()
        .unwrap_or_else(|e| panic!("autosk-core: failed to spawn {}: {e}", script.display()));
    assert!(
        status.success(),
        "autosk-core: {} {version} {} exited with {status}",
        script.display(),
        dolt_dir.display()
    );
    assert!(
        dolt_dir.join("libdoltlite.a").is_file(),
        "autosk-core: fetch reported success but {}/libdoltlite.a is missing",
        dolt_dir.display()
    );
}

/// Creates (or refreshes) a symlink at `link` pointing to `target`. No-ops when
/// the link already resolves to `target`.
fn symlink_force(target: &Path, link: &Path) {
    if let Ok(existing) = std::fs::read_link(link) {
        if existing == target {
            return;
        }
    }
    let _ = std::fs::remove_file(link);
    std::os::unix::fs::symlink(target, link).unwrap_or_else(|e| {
        panic!(
            "autosk-core: symlink {} -> {}: {e}",
            link.display(),
            target.display()
        )
    });
}

fn env(key: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| panic!("autosk-core: missing build env {key}"))
}
