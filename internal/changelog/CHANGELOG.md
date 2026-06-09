# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **autoskd:** new Rust daemon binary serving the read-only task/job/workflow/agent/comment/signal surface over JSON-RPC on a Unix socket, with `autoskd serve` / `init` / `version` / `engine` subcommands (ask-d8f735).
- **autoskd:** now drives workflows end-to-end natively — a per-project poller, a global worker-pool scheduler, the step executor (turn loop, kickback, worktree isolation, idle-timeout), and the compactor — plus a live job surface (`job.subscribe` transcript streaming with replay+tail, `job.input` prompt/steer/follow_up, `job.abort`, `job.cancel`) (ask-2e7e27).
- **lazy:** the TUI now renders entirely from an auto-spawned `autoskd` daemon over JSON-RPC — reads, writes, and the live job transcript tail (`job.subscribe`) — instead of opening a local doltlite store; the single RPC-client datasource replaces the former offline/live/compose split (ask-c9251d). (Supersedes the experimental, never-released `autosk lazy --rpc` / `AUTOSK_RPC=1` opt-in, which is now the only behaviour and has been removed.)
- **autoskd:** the daemon is now the sole writer — it serves the full JSON-RPC write surface (task create/update/enroll/resume/block/unblock/comment/metadata, workflow create/delete/updateIsolation, agent install/uninstall, sql query/exec, project.init), adds an opt-in TCP listener (`--tcp HOST:PORT`) gated by a `~/.autosk/daemon-token` token (UDS stays auth-free), pushes `task-changed`/`project-changed` notifications to subscribers, and exposes a `shutdown` RPC plus an idle-shutdown policy (`AUTOSK_IDLE_SECS`, `0` disables; never on a TCP service daemon) (ask-5cae6c).
- **gui:** new app icon — a Liquid Glass icon (Apple Icon Composer) shipped as `icons/Assets.car` (`CFBundleIconName` via `src-tauri/Info.plist`, bundled to `Contents/Resources` through `bundle.macOS.files`) for macOS 26+, with a regenerated flat `.icns`/`.ico`/PNG set as the cross-platform fallback (macOS < 26, Windows, Linux).
- **gui:** new Tauri desktop app (`gui/`) at `autosk lazy` parity — local (auto-spawned `autoskd` over UDS) and remote (TCP + token) modes behind a transport-agnostic frontend, live task-timeline streaming via `job.subscribe`, a state-aware composer (steer/follow_up/abort/cancel, comment/resume, enroll), and projects/tasks/jobs/workflows/agents views (ask-9e5f8c).

### Changed
- **gui:** redesigned the desktop app into a **frameless 3-panel workspace** — a left **Sessions** list (every job, newest-first, with animated status dots), a polymorphic **center** (session transcript / task sheet with comments / read-only workflow definition) driven by one entity-aware composer, and a right **Tasks + Workflows** stack. A titlebar hosts the connection indicator and the Agents/Settings modals, and the center header carries a project switcher (switch / add / init / remove); the old top-nav + projects sidebar are gone.
- **build:** `make install` now also builds `autoskd` (release) and installs it alongside `autosk` in `$GOBIN`/`$GOPATH/bin`, so auto-spawn works for source installs out of the box; `make uninstall` removes both. New `make install-autoskd` target installs only the daemon.
- **bootstrap:** `feature-dev-generic` workflow now ships with `isolation: worktree` by default.
- **daemon:** `autosk daemon status | messages | cancel | list` now route to the `autoskd` daemon over JSON-RPC instead of the retired Go daemon (ask-2e7e27).
- **cli:** every `autosk` write verb — `create` / `enroll` / `resume` / `done` / `cancel` / `reopen` / `comment` / `metadata` / `block` / `unblock`, `workflow create|delete|update`, `agent install|uninstall`, `sql`, `init` — now routes through an auto-spawned `autoskd` over JSON-RPC instead of opening the local doltlite store. A fresh-dir `autosk create` still prompts (or honours `AUTOSK_NO_AUTOINIT` / `AUTOSK_AUTOINIT_*`) and then auto-inits via the daemon's `project.init` (which runs migrations + the `feature-dev-generic` bootstrap); no Go process ever opens `.autosk/db` (ask-913906).
- **build:** the Go `autosk` binary (CLI + lazy TUI) is now **CGO-free** — it no longer links `libdoltlite.a`, the `libsqlite3` build tag is gone, and `make build` / `install` / `vet` / `test` no longer depend on `make doctor` / the doltlite fetch. `autoskd` (Rust) is the sole doltlite consumer; the front ends build with plain `go build` (ask-913906).
- **cli:** `autosk version` now reads the project schema version from an already-running `autoskd` (best-effort; `version` never auto-spawns the daemon, so the schema line falls back to `-` when no daemon is up) (ask-913906).
- **cli:** `autosk version` now reports `backend: autoskd` — the Go binary links no storage engine of its own; persistence lives in the daemon (ask-60d40c).
- **lazy:** panels now refresh on the daemon's `task-changed`/`project-changed` push instead of the fixed 2-second client-side poll; a long safety re-sync (`--refresh`, floored to 30s) remains as a backstop (ask-913906).

### Removed
- **daemon:** the Go `autosk daemon serve` verb is retired — `autoskd` now drives workflows (ask-2e7e27).

### Fixed
- **daemon:** fix empty lazy transcript and `HTTP 410 session_missing` from `daemon messages <job>`
- **gui:** the Tasks panel now uses the same card-style selection (distinct `--bg-3` fill + full rounded border) as the Sessions and Workflows panels, instead of its inconsistent left-accent stripe over a hover-colored background.

## [0.1.5] — 2026-05-25

### Added
- **lazy:** two-pane workflow + step picker for `enroll` / `resume` actions.
- **lazy:** redesigned Workflow Detail pane.
- **lazy:** `?` opens a sectioned, filterable cheatsheet popup; Enter runs the highlighted binding (ask-ed8035).
- **lazy:** context-aware bindings hint row pinned across the bottom of the dashboard (ask-ed8035).
- **lazy:** changelog modal on first run of a new release; `ctrl+w` to re-open the full CHANGELOG.md; `--no-changelog` to suppress the auto-popup (ask-911ea0).

### Changed
- **lazy:** status bar collapsed to a single row with ` | ` separators; legacy `?=help` hint moved into the cheatsheet popup (ask-ed8035).
- **docs:** `CHANGELOG.md` now lives at `internal/changelog/CHANGELOG.md` (embedded into the binary via `go:embed` for the new changelog modal); the repo-root `CHANGELOG.md` is a relative symlink so release tooling, GitHub's auto-renderer, and `scripts/changelog-section.sh` keep working unchanged (ask-911ea0).

### Fixed
- **lazy:** fix cursor jumping to a different row when a refresh re-sorted or re-filtered a panel.
- **lazy:** hydrate the job transcript on focus change to the Jobs panel.

### Docs
- **lazy:** document the two-pane enroll / resume picker.
- **lazy:** describe the redesigned Workflow Detail pane.
- **lazy:** document the changelog modal, `ctrl+w` re-opener, `--no-changelog` flag, and the `~/.autosk/state.json` `last_seen_changelog` field (ask-911ea0).

## [0.1.4] — 2026-05-24

### Added
- **enroll:** allow enrolling tasks that are currently in `human`, `done`,
  or `cancel` status (previously enroll was limited to `new` / `work`).
- **lazy:** rework Jobs panel columns — worktime column, colored status glyph,
  right-aligned task id.

### Changed
- **cli:** drop `autosk assign` in favour of `autosk enroll --agent`.
  `enroll --agent <pkg>` is now the single way to bind an agent to a task,
  with or without a workflow.
- **examples:** move workflow JSON examples to `docs/examples/workflows/`.

### Fixed
- **lazy:** render the full comment thread in the Tasks → Detail pane
  (drop the previous "last 5 comments" cap).
- **enroll:** address review remarks on the human / done / cancel enroll path.

### Docs
- **enroll:** refresh `docs/workflows.md` and CLI docstrings for the new
  human / done / cancel enroll behaviour.

## [0.1.3] — 2026-05-23

### Added
- **workflow:** `autosk workflow update --isolation <mode>` to change a
  workflow's worktree isolation mode in place; lazy surfaces this via the
  new `i` binding on a selected workflow.

### Fixed
- **workflow update:** per-task confirmation body, JSON-on-error output, and
  sentinel-preserving error returns (review remarks WU1–WU4).

### Docs
- Cover `workflow update --isolation` and the lazy `i` binding.
- Simplify the workflow-creation doc; layout fix for the workflow doc.

## [0.1.2] — 2026-05-23

### Docs
- Rewrite `docs/lazy.md` for the current version of the TUI.
- Add the lazy-mode dashboard screenshot (`docs/lazy-mode.png`) to the docs.
- Clean up `README.md`, add more cross-links to the docs.

## [0.1.1] — 2026-05-23

### Added
- **README:** Homebrew install option (`brew install wierdbytes/autosk/autosk`).

### Changed
- **ci/release:** automatically bump the `wierdbytes/homebrew-autosk` formula
  on every tag publish.
- **ci/release:** bump `actions/download-artifact` v6 → v7 (Node.js 24).

## [0.1.0] — 2026-05-23

Initial public release of **autosk** — a local-first task manager and
workflow engine for AI coding agents. One DB per repo, one daemon per host,
one TUI to see it all.

### Added

#### Core / task tracker
- doltlite-backed `.autosk/db` per repository.
- Task ids in the canonical `ask-XXXXXX` format (6 hex chars).
- Blockers / blocked-by graph, priorities `0..3` (`0` highest).
- Short, persisted task / run status vocabulary: `new`, `work`, `human`,
  `done`, `cancel`.
- CLI verbs: `init`, `create`, `update`, `show`, `list`, `ready`, `done`,
  `cancel`, `enroll`.
- `--step NAME` flag on `create` / `enroll` to join a workflow at a specific
  step instead of its `first_step`.
- Implicit auto-init in fresh directories on the first write verb; opt-out
  with `--skip-bootstrap`; non-interactive auto-accept via
  `AUTOSK_AUTOINIT_*` env knobs.
- Auto-bootstrap of the canonical `feature-dev-generic` workflow on
  `autosk init` (and on implicit auto-init).

#### Workflow engine
- v0.2 step engine with directed-graph workflows, transitions, agents, and
  comments.
- `autosk_step next` as the canonical agent-side "I'm done with this step"
  signal.
- Per-step `agent.params` overrides for npm-packaged agents.
- Step visit limits + target-step parking on advance failures.
- Parked / failed runs render uniformly as `[id]: name` and surface all
  fields.
- v0.3 worktree isolation per workflow; executor auto-recovers a missing
  worktree dir; `AUTOSK_DB` is isolated in tests.

#### Agents
- npm-package agent system (replaces the earlier `.autosk/agents/*.toml`).
- Reference workflow `feature-dev-generic` (dev → review → docs → validator
  → human) shipped via `go:embed` from `internal/bootstrap`.

#### Daemon
- `autosk daemon` — multi-project orchestrator over a Unix-domain socket
  (one daemon per host, project resolved per request).
- HTTP API with closure verification and SSE event stream.
- `pi`-runner reader switched to `json.Decoder`; honours the cancellation
  signal on read-loop errors; `step_signal` honoured on wait-error recovery.

#### Lazy TUI (`autosk lazy`)
- gocui-based dashboard with Tasks / Jobs / Workflows / Agents / Detail
  panes and a smoke E2E.
- Datasource layer split into offline + live + compose.
- Lazygit-style task-compose popup editor; key bindings: `c` (edit
  title / description, two-pane compose), `m` (single-pane comment compose),
  `M` (JSON-validated metadata compose), `i` (workflow isolation), `?`
  (help), `n` (new task).
- Tokyo Night theme + dedicated `theme` package; rounded panel and popup
  corners; scroll-off-margin viewport that follows the cursor; mouse-wheel
  scroll across panels and inspector.
- Tasks panel redesigned with entity colours, spinner, job marker, and
  manual-scope toggle.
- Detail pane: labeled boxes, markdown rendering with a bounded LRU cache,
  signals box stacked as a 4-column table, blockers partitioned by status,
  full comment thread.
- Job Detail redesigned — Inspector removed, sticky-tail on the transcript,
  job-input nested inside Detail and gated on `Streaming`.
- Jobs panel: age column moved leftmost.

#### Extension (`@wierdbytes/pi-autosk`)
- TypeScript SDK + agent runtime, callable from `pi --mode rpc`.
- Mega-tool split into three focused tools: `autosk_task`,
  `autosk_comment`, `autosk_step`.

#### Docs
- End-user `README.md` (install, quick start, lazy mode, CLI walk-through).
- `docs/daemon.md`, `docs/lazy.md`, `docs/workflows.md`.
- Time-rendering policy: human-facing times route through
  `internal/timeformat` in the operator's local TZ; machine wire formats
  (JSON CLI, HTTP API, `RunContextSeed`, TS types) stay on RFC3339 UTC.
- `AGENTS.md` rewritten as a repo orientation guide.

#### Build & CI
- Go 1.25 toolchain; mandatory `-tags libsqlite3` build tag routed through
  doltlite-provided libsqlite3.
- `make doctor` checks the doltlite library; every build / test target
  depends on it.
- Auto-fetch `libdoltlite`; CI runs on linux + macOS.
- Publish release binaries on every semver tag push.
- Migrations 001..007 squashed into a single release migration.

### Changed
- **statuses:** shortened persisted task / run status vocabulary
  (e.g. `in_workflow` → `work`); CLI / TUI / docs all updated.
- **tasksvc:** unified human-driven status transitions across the CLI and
  the lazy TUI.

### Fixed
- **doltlite:** validate the file inode on every connection checkout to
  close a write-loss race.
- **doltlite:** rotate the `sql.DB` connection periodically to recover from
  cross-process garbage collection.
- **runstore:** retry on `database is locked` for every write, not just
  reads.
- **workflow:** `REINDEX workflows` after `Delete` to clear phantom
  unique-index entries.
- **migrations:** rebuild `tasks` / `task_deps` in migration 007 to dodge a
  doltlite UPDATE-PK quirk.
- **compactor + lazy:** bound chunk-store WAL replay so `autosk lazy`
  doesn't peg a CPU core.
- **lazy/tui:** assorted scope, focus, and sticky-tail fixes on the Detail
  pane; widen input visibility to `!IsTerminal`; pin overlay sticky-tail
  and popup z-order; clear `TextArea` (not just `v.lines`) on
  dispatch / cursor-move / Esc.

[Unreleased]: https://github.com/wierdbytes/autosk/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/wierdbytes/autosk/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/wierdbytes/autosk/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/wierdbytes/autosk/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/wierdbytes/autosk/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wierdbytes/autosk/releases/tag/v0.1.0
