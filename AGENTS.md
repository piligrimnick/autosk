
# autosk - task manager and workflow manager for coding agents

This file provides guidance when working with code in this repository.

## Build, test, lint

The Go binary (CLI + lazy TUI) is a pure JSON-RPC client of `autoskd` and **no longer links doltlite**: it builds CGO-free with plain `go build` / `go test` — no `-tags libsqlite3`, no `libdoltlite.a`. `autoskd` (the Rust daemon) is the sole doltlite consumer.

- `make build` — compiles `bin/autosk` (entrypoint: `cmd/autosk`), CGO-free
- `make build-autoskd` — compiles the Rust daemon (`cargo build -p autoskd`) into `target/debug/autoskd`
- `make test` — builds `autoskd`, then runs `go test ./...` with `AUTOSKD_BIN` pointed at it
- `make test-short` — same with `-short`
- `make vet` — `go vet ./...`
- `make fmt` — `gofmt`
- `make lint` — `golangci-lint run ./...` (requires `golangci-lint` installed)

No Go target depends on a `doctor` / doltlite fetch anymore. The `cmd/autosk` verb tests are RPC clients and need a live `autoskd` on disk, which they auto-spawn after locating it via `$AUTOSKD_BIN` (set by `make test`/`make test-short`, which build it first). A direct `go test ./...` works without the `libsqlite3` tag, but the verb tests `t.Skip` unless `autoskd` has been built (`make build-autoskd`) and `$AUTOSKD_BIN` is set.

`autoskd` fetches its own pinned doltlite via `crates/autosk-core/build.rs` (`scripts/fetch-doltlite.sh`) when you `cargo build` it — independent of the Go Makefile. The Go tree no longer contains any doltlite/SQL store: the storage engine, migrations, and workflow/run engine all live in `autoskd` (`autosk-core`); the Go packages keep only the view types the front ends render over RPC.

### Rust workspace (`crates/`)

`autoskd` and its supporting crates live in a Cargo workspace at the repo root. Only `autosk-core` links doltlite; `autosk-proto` and `autoskd` are plain serde.

- `cargo build --workspace` — compile `autosk-core` + `autosk-proto` + `autoskd` (first build runs `build.rs`, which fetches + statically links doltlite 0.11.8)
- `cargo test --workspace` — run every Rust test, including the three conformance gates: executor behavioural parity (`autosk-core --test executor`, plan §8.1), protocol golden round-trips (`autosk-proto --test golden`, §8.2), and the migration/schema golden (`autosk-core --test schema_golden`, §8.3)
- `cargo fmt --all --check` — formatting gate
- `cargo clippy --workspace --all-targets -- -D warnings` — lint gate

### Tauri GUI (`gui/`)

The desktop app is a React/Vite front end over a thin Tauri (Rust) backend that is a **pure JSON-RPC client of `autoskd`** — `gui/src-tauri` is a standalone cargo crate, excluded from the root workspace, and links no doltlite. Front-end checks run from `gui/`:

- `npm ci` — install (uses `gui/package-lock.json`)
- `npm run typecheck` — `tsc --noEmit` **plus** the `scripts/check-ipc-discipline.mjs` guard (one `invoke` site in `src/services/ipc.ts`, one `listen` site in `src/services/events.ts`)
- `npm run build` — `tsc && vite build` (production bundle)
- `npm run test` — `vitest` (pure reducer/selector/ipc logic; no browser or daemon)

Backend check runs from `gui/src-tauri/`:

- `cargo check` — typecheck the Tauri backend (independent of the daemon's doltlite fetch since the crate links none)

App icons: sources live in `gui/src-tauri/icons/src/` (`autosk.icon` + `autosk.png`) and all binary icon artifacts under `icons/` are stored in Git LFS (run `git lfs install` after cloning). Regenerate the Liquid Glass `Assets.car` + flat `.icns`/`.ico`/PNG fallback with `scripts/update-gui-icons.sh` (no args = repo sources; pass `<App.icon> <icon-1024.png>` to adopt a new icon); full usage is in the script header.

## Repo layout

- `cmd/autosk/` — Cobra CLI entrypoint
- `internal/lazy/` — lazygit-style TUI (`autosk lazy`), built on `jesseduffield/gocui`
- `internal/daemon/` — JSON-RPC client of `autoskd` (`rpcclient`, auto-spawn) plus shared wire/view types (`api`, `runstore`)
- `internal/workflow/` — workflow-JSON parser + materialised view types (the step-graph engine lives in `autoskd`/`autosk-core`)
- `internal/projectdb/`, `internal/store/` — `.autosk/db` path resolution + the shared task/workflow/agent view types the front ends render (no DB is opened in Go)
- `crates/` — the Rust Cargo workspace:
  - `crates/autosk-core/` — the domain: store/doltlite + migrations, workflow engine, executor, poller/scheduler/compactor, projectmgr, project registry (sole doltlite consumer)
  - `crates/autosk-proto/` — serde wire types + JSON-RPC method/notification definitions (the single source of truth the Go and Tauri clients mirror)
  - `crates/autoskd/` — the daemon binary: JSON-RPC server over UDS (+ opt-in TCP/token), single-instance + auto-spawn, idle-shutdown
- `gui/` — the Tauri desktop app: `src/` (React + Vite front end), `src-tauri/` (thin Tauri backend, a pure JSON-RPC client of `autoskd`; standalone cargo crate, doltlite-free)
- `extension/` — TypeScript SDK + agent runtime (`pi --mode rpc`)
- `agents/` — reference agent implementations (TS/JS)

## Additionsl docs

- `docs/palns/yyyymmdd-*.md` - pans for feature development
- `docs/daemon.md` - autoskd — the Rust daemon (JSON-RPC, auto-spawn, sole `.autosk/db` owner)
- `docs/lazy.md` - autosk lazy interactive TUI
- `docs/workflows.md` - autosk workflow management engine
- `gui/README.md` - the Tauri desktop GUI (architecture, scripts, local vs remote modes)


## Conventions

- All text in tasks / comments / docs should be in English.
- Commits follow Conventional Commits: `feat:`, `fix:`, `chore:`, `refactor:`, `docs:`, `test:`, etc.
- **Changelog policy.** Every PR that ships a user-visible change (CLI verb, flag, TUI hotkey, output format, env knob, …) updates `CHANGELOG.md` in the same PR. Format follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/): add or extend the top-of-file `## [Unreleased]` block with `### Added` / `### Changed` / `### Fixed` / `### Removed` / `### Deprecated` / `### Security` subsections; release tagging promotes the block to a dated `## [X.Y.Z] - YYYY-MM-DD` header. `CHANGELOG.md` at repo root is a symlink to `internal/changelog/CHANGELOG.md` (the canonical file the binary `go:embed`s for the lazy first-run popup) — edit either path, they resolve to the same file. Internal-only churn (refactors, test-only changes, build-system tweaks with no operator-visible effect) does NOT need an entry.
- Go 1.25 toolchain (`go.mod`).
- The TUI uses the `jesseduffield/gocui` fork (the lazygit one), not `awesome-gocui`.
- **Time rendering.** Anything user-facing (CLI text output, TUI panes, daemon-list output, flash toasts, command log) MUST format through `internal/timeformat` — `FormatDate` / `FormatTime` / `FormatDateTime` / `FormatDateTimeSmart`. These render in the operator's local timezone using the layouts `2006-01-02`, `15:04:05`, `2006-01-02 15:04:05`. Machine wire formats (JSON CLI output behind `--json`, the daemon JSON-RPC wire types in `crates/autosk-proto` mirrored by the Go view types in `internal/daemon/api`, `RunContextSeed` + comment-prompt rendering in `crates/autosk-core`, TS types in `extension/src`) stay on **RFC3339 UTC** and must NOT route through `timeformat`.
