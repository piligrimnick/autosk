
# autosk - task manager and workflow manager for coding agents

This file provides guidance when working with code in this repository.

## Build, test, lint

Use the `Makefile` targets — they set the required CGO flags and the `libsqlite3` build tag. Plain `go build`/`go test` won't link correctly.

- `make build` — compiles `bin/autosk` (entrypoint: `cmd/autosk`)
- `make test` — runs `go test -tags libsqlite3 ./...`
- `make test-short` — same with `-short`
- `make vet` — `go vet` with the build tag
- `make fmt` — `gofmt`
- `make lint` — `golangci-lint run --build-tags libsqlite3` (requires `golangci-lint` installed)
- `make doctor` — verifies the doltlite library is present at `$DOLTLITE_DIR` (default `$HOME/me/dev/doltlite/build`)

Every build/test target depends on `doctor`. If it fails, doltlite hasn't been built — surface the doctor message to the user; don't try to build doltlite yourself.

The `libsqlite3` build tag is mandatory: it routes `mattn/go-sqlite3` through the doltlite-provided libsqlite3 instead of the embedded amalgamation. Any direct `go` invocation Claude runs must pass `-tags libsqlite3`.

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
