# autoskd v2 — Bun/TypeScript workspace

This directory hosts the v2 rewrite of the autosk daemon as a Bun + TypeScript
workspace. It replaces the Rust `autoskd` / `autosk-core` / `autosk-proto`
stack (and doltlite) once parity lands; see
`docs/plans/20260612-Bun-Daemon-Extensions.md` for the full design.

## Packages

- **`sdk/`** — `@autosk/sdk`: the public, extension-facing types
  (Task / Session / Workflow / Agent / Isolation / `AutoskAPI`), the
  pi-format transcript entry types, and the proto-v2 wire types. The
  proto-v2 types are the single source of truth that the Go and Tauri
  clients mirror (P7/P8).
- **`core/`** — `@autosk/core`: the daemon binary (file store, project
  manager, extension loader, scheduler, RPC server). P2 landed the **file
  store + project manager** (`src/store/`, `src/project/`) — the on-disk
  task/comment/session formats, atomic writes, mtime cache, watcher
  reconciliation, and the `~/.autosk/projects.json` registry + walk-up
  resolution. P3 landed the **extension loader + per-project registries**
  (`src/extensions/`) — pi-style discovery (project-local / global dirs + npm
  packages listed under `"extensions"` in `settings.json`), in-process factory
  loading with full error isolation (a broken extension — throwing factory,
  failed import, duplicate name, malformed `package.json` — is recorded as a
  load diagnostic and never crashes the daemon or blocks the rest of the
  registry), the on-demand `singleStep` builtin (never persisted, never listed),
  and the live-code hazard guard that parks any in-flight `work` task whose
  workflow/step has vanished from the registry to `human`. `ProjectManager.open()`
  builds each project's registry, logs load diagnostics, and runs the hazard
  guard before the scheduler can pick anything up. P4 landed the **engine**
  (`src/engine/`) — the scheduler (a single event-driven scan + a global FIFO
  worker pool, `--workers`, shared across projects, plus a slow safety rescan),
  the session lifecycle, `ctx.transit` (onTransit validation → atomic `task.json`
  commit → isolation acquire/release), the pi-format transcript writer, the
  `AgentRunContext` (tasks/workflows/log/comment/exec/spawn), steer/followup/abort
  routing, and crash recovery (running/queued sessions → `failed: daemon_restart`,
  task → `human`); the engine is RPC-independent — driven purely via the Engine
  API in tests. P5 landed the **JSON-RPC v2 server** (`src/rpc/`) — JSON-lines
  over UDS (default `~/.autosk/daemon.sock`, `$AUTOSK_SOCK`; parent dir `0700` /
  socket `0600`) plus an opt-in TCP transport with token auth
  (`~/.autosk/daemon-token`, `$AUTOSK_TOKEN_FILE`); single-instance via an atomic
  pidfile lock (`<sock>.lock`, dead-pid reclaim — Bun's `node:net` silently
  rebinds a unix path, so there is no `EADDRINUSE` to race on); a handler table
  whose key set is compile-time-pinned to `RPC_METHODS` (no-drift) and whose
  returns are bound to each method's `ResultOf<M>`; notification fan-out
  (`task-changed` from the store seam, `session-event` from the engine,
  `project-changed` from `ProjectManager`); `session.subscribe` replay-then-tail
  from `from_line`; idle-shutdown (no live connections + no queued/running
  sessions + no `status=work` tasks across loaded projects, window via
  `$AUTOSK_IDLE_SECS`, default 1800s / `0` disables, off in TCP mode); and
  `EngineError.code` → `RpcError.code` passthrough. The binary's `main()`
  (`src/index.ts`) parses `--sock` / `--tcp[=HOST:PORT]` / `--workers`, starts the
  daemon, exits `0` when it loses the single-instance race, and otherwise fails
  loud-but-clean (exit `1`, one-line `autoskd: <msg>`) on a startup error. Two
  RPC-layer semantics worth noting (full operator docs land in P9): `task.done` /
  `task.cancel` / `task.reopen` are administrative overrides that write via the
  store and do **not** run `workflow.onTransit` (they reject a live session with
  `CONFLICT`); `project.remove` is lazy — it forgets the project in the registry
  and emits `project-changed` but leaves an already-open handle running until the
  next daemon start. P6 added the **daemon-bundled extension discovery** seam
  that ships `@autosk/feature-dev` to every project: an opt-in, lowest-priority
  `bundledDir` source in the extension loader (`src/extensions/loader.ts`),
  overridable by any project-local / global / npm extension of the same name,
  which the default `ProjectManager` points at `daemon/extensions/`
  (`$AUTOSK_BUNDLED_EXTENSIONS` override) so a project can enroll into the
  bundled workflow with **no per-project files**. The seam is unset by default,
  so existing tests (which inject their own `projectManager`) are unaffected.
- **`extensions/worktree/`** — `@autosk/worktree`: the shipped **isolation
  provider** `worktreeIsolation()` — per-task git-worktree isolation attachable
  to any workflow, a byte-identical port of v1's worktree behaviour
  (deterministic `~/.autosk/worktrees/<slug>/<task-id>` path on branch
  `autosk/<task-id>`, branch-preserving terminal release, dir-kept on
  sibling/human-park, missing-dir re-allocation). See
  [`extensions/worktree/README.md`](extensions/worktree/README.md).
- **`extensions/pi-agent/`** — `@autosk/pi-agent`: the shipped **agent**
  `piAgent({...})` that drives `pi --mode rpc` over JSON-lines stdio, mirrors
  pi's transcript entries (messages / custom) into the autosk transcript 1:1,
  and bridges step transitions through an injected `autosk_transit` pi-tool
  observed on pi's RPC event stream (plus a private kickback/corrections loop
  and steer / followup / abort forwarding into the live pi). See
  [`extensions/pi-agent/README.md`](extensions/pi-agent/README.md).
- **`extensions/feature-dev/`** — `@autosk/feature-dev`: the shipped
  **reference workflow** `dev → review → docs → validator → accept` (with
  review→dev / validator→dev bounce-backs, a `ctx.visits("dev")` visit cap, and
  `worktreeIsolation()`), wired to four `@autosk/pi-agent` roles and replacing
  v1's `feature-dev-generic` bootstrap. It is the package the daemon discovers
  via the bundled-extension seam above. See
  [`extensions/feature-dev/README.md`](extensions/feature-dev/README.md).

## Scripts

Run from this directory:

- `bun install` — install + symlink the workspace packages.
- `bun test` — run every package's `*.test.ts` (pure unit tests, no daemon).
- `bun run typecheck` — `tsc --noEmit` across all workspace packages.
