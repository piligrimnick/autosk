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
  task → `human`). The engine is RPC-independent — driven purely via the Engine
  API in tests. The RPC v2 server is still to come (P5).
- **`extensions/pi-agent/`** — `@autosk/pi-agent`: the shipped extension that
  drives `pi --mode rpc` as an autosk agent. Placeholder in P1 (built in P6).

## Scripts

Run from this directory:

- `bun install` — install + symlink the workspace packages.
- `bun test` — run every package's `*.test.ts` (pure unit tests, no daemon).
- `bun run typecheck` — `tsc --noEmit` across all workspace packages.
