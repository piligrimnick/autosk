# doltlite-shim

`libsqlite3.a` here is an **intentionally empty** static archive (the bare 8-byte
`!<arch>` magic, no object members).

## Why this exists

`autosk-core` links the doltlite-provided sqlite (doltlite 0.11.8) instead of a
bundled amalgamation. `rusqlite`/`libsqlite3-sys` is built with `bundled`
disabled, and with `SQLITE3_STATIC=1` + `SQLITE3_LIB_DIR` (see
`../../../.cargo/config.toml`) it bundles a `libsqlite3.a` into its own rlib **at
its own compile time** — which happens *before* `autosk-core`'s `build.rs` can
download doltlite into `.doltlite/`. There is no way to make a build script run
ahead of a transitive dependency's compilation, so a real archive can't be
staged in time on a cold checkout.

The fix: let `libsqlite3-sys` bundle this empty shim (always present, committed),
and have `autosk-core/build.rs` emit `cargo:rustc-link-lib=static=doltlite` so
the *real* `libdoltlite.a` (fetched into `.doltlite/<ver>-<plat>/`) supplies the
sqlite3 C API and the dolt SQL functions at the final link step. The empty shim
contributes no symbols, so there are no duplicates.

Do not put a real library here.
