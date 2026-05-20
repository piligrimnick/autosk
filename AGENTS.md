
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
- `internal/projectdb/`, `internal/store/`, `internal/migrations/` — doltlite-backed task/workflow/agent store
- `extension/` — TypeScript SDK + agent runtime (`pi --mode rpc`)
- `agents/` — reference agent implementations (TS/JS)

## Additionsl docs

- `docs/palns/yyyymmdd-*.md` - pans for feature development
- `docs/daemon.md` - autosk daemon — multi-project pi orchestrator
- `docs/lazy.md` - autosk lazy interactive TUI
- `docs/workflows.md` - autosk workflow management engine


## Conventions

- Commits follow Conventional Commits: `feat:`, `fix:`, `chore:`, `refactor:`, `docs:`, `test:`, etc.
- Go 1.25 toolchain (`go.mod`).
- The TUI uses the `jesseduffield/gocui` fork (the lazygit one), not `awesome-gocui`.
