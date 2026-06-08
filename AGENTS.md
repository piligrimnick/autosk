
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

`autoskd` fetches its own pinned doltlite via `crates/autosk-core/build.rs` (`scripts/fetch-doltlite.sh`) when you `cargo build` it — independent of the Go Makefile. (The legacy `internal/store/doltlite` package and its tests are gated behind `//go:build libsqlite3` and are slated for deletion in Phase 5.)

## Repo layout

- `cmd/autosk/` — Cobra CLI entrypoint
- `internal/lazy/` — lazygit-style TUI (`autosk lazy`), built on `jesseduffield/gocui`
- `internal/daemon/` — Unix-domain-socket daemon + job executor
- `internal/workflow/` — workflow step graph, transitions, executor
- `internal/bootstrap/` — canonical workflow JSON shipped with the binary; embedded via `go:embed` and applied by `autosk init`. Single source of truth for `feature-dev-generic` — do not duplicate it under `docs/` or anywhere else.
- `internal/projectdb/`, `internal/store/`, `internal/migrations/` — doltlite-backed task/workflow/agent store
- `extension/` — TypeScript SDK + agent runtime (`pi --mode rpc`)
- `agents/` — reference agent implementations (TS/JS)

## Additionsl docs

- `docs/palns/yyyymmdd-*.md` - pans for feature development
- `docs/daemon.md` - autosk daemon — multi-project pi orchestrator
- `docs/lazy.md` - autosk lazy interactive TUI
- `docs/workflows.md` - autosk workflow management engine


## Conventions

- All text in tasks / comments / docs should be in English.
- Commits follow Conventional Commits: `feat:`, `fix:`, `chore:`, `refactor:`, `docs:`, `test:`, etc.
- **Changelog policy.** Every PR that ships a user-visible change (CLI verb, flag, TUI hotkey, output format, env knob, …) updates `CHANGELOG.md` in the same PR. Format follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/): add or extend the top-of-file `## [Unreleased]` block with `### Added` / `### Changed` / `### Fixed` / `### Removed` / `### Deprecated` / `### Security` subsections; release tagging promotes the block to a dated `## [X.Y.Z] - YYYY-MM-DD` header. `CHANGELOG.md` at repo root is a symlink to `internal/changelog/CHANGELOG.md` (the canonical file the binary `go:embed`s for the lazy first-run popup) — edit either path, they resolve to the same file. Internal-only churn (refactors, test-only changes, build-system tweaks with no operator-visible effect) does NOT need an entry.
- Go 1.25 toolchain (`go.mod`).
- The TUI uses the `jesseduffield/gocui` fork (the lazygit one), not `awesome-gocui`.
- **Time rendering.** Anything user-facing (CLI text output, TUI panes, daemon-list output, flash toasts, command log) MUST format through `internal/timeformat` — `FormatDate` / `FormatTime` / `FormatDateTime` / `FormatDateTimeSmart`. These render in the operator's local timezone using the layouts `2006-01-02`, `15:04:05`, `2006-01-02 15:04:05`. Machine wire formats (JSON CLI output behind `--json`, daemon HTTP API in `internal/daemon/api`, `RunContextSeed` in `internal/daemon/executor`, `comment.RenderForPrompt` in `internal/comments`, TS types in `extension/src`) stay on **RFC3339 UTC** and must NOT route through `timeformat`.
