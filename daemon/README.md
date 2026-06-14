# autoskd — Bun/TypeScript daemon

This directory is the **autoskd** daemon: a Bun + TypeScript workspace that owns
each project's `.autosk/` directory and drives tasks through their workflows.
It is the sole writer of the on-disk task / comment / session files (there is no
database), and it loads workflows + agents as **code** from extensions. The Go
CLI / lazy TUI and the Tauri GUI are pure JSON-RPC clients of it.

See [`../docs/daemon.md`](../docs/daemon.md) for the operator-facing daemon
guide, [`../docs/workflows.md`](../docs/workflows.md) for the workflow/agent
contracts, [`../docs/extensions.md`](../docs/extensions.md) for the extension
system, and [`../docs/plans/20260612-Bun-Daemon-Extensions.md`](../docs/plans/20260612-Bun-Daemon-Extensions.md)
for the full design.

## Packages

- **`sdk/`** — `@autosk/sdk`: the public, extension-facing types
  (Task / Session / Workflow / Agent / Isolation / `AutoskAPI`), the pi-format
  transcript entry types, and the proto-v2 JSON-RPC wire types. The proto-v2
  types are the single source of truth that the Go (`internal/daemon/api`) and
  Tauri (`gui/src-tauri`) clients mirror.

- **`core/`** — `@autosk/core`: the daemon binary itself. Its pieces:
  - **file store** (`src/store/`) — the on-disk task/comment/session formats,
    atomic writes (tmp + rename), an mtime cache, and a watcher + startup scan
    that reconcile external (human/script) edits.
  - **project manager** (`src/project/`) — the `~/.autosk/projects.json`
    registry, walk-up resolution by `{cwd}`, and lazy per-project open (file
    store + extension registry + scheduler).
  - **extension loader** (`src/extensions/`) — pi-style discovery (project-local
    `.autosk/extensions/` ▸ global `~/.autosk/extensions/` ▸ npm packages listed
    under `"extensions"` in `settings.json` ▸ daemon-bundled), in-process factory
    loading with full error isolation (a broken extension is recorded as a load
    diagnostic and never crashes the daemon), the on-demand `singleStep` builtin,
    and the live-code hazard guard that parks any in-flight task whose
    workflow/step has vanished from the registry to `human`.
  - **engine** (`src/engine/`) — the scheduler (a single event-driven scan + a
    global FIFO worker pool, `--workers`, shared across projects, plus a slow
    safety rescan), the session lifecycle, `ctx.transit` (onTransit validation →
    atomic `task.json` commit → isolation acquire/release), the pi-format
    transcript writer, the `AgentRunContext` (tasks/workflows/log/comment/exec/
    spawn), steer/followup/abort routing, and crash recovery (interrupted
    sessions → `failed: daemon_restart`, task → `human`).
  - **JSON-RPC v2 server** (`src/rpc/`) — JSON-lines over UDS (default
    `~/.autosk/daemon.sock`, `$AUTOSK_SOCK`) plus an opt-in TCP transport with
    token auth; single-instance via an atomic pidfile lock; notification fan-out
    (`task-changed`, `session-event`, `project-changed`); `session.subscribe`
    replay-then-tail; and idle-shutdown.

- **`extensions/worktree/`** — `@autosk/worktree`: the shipped **isolation
  provider** `worktreeIsolation()` — per-task git-worktree isolation attachable
  to any workflow (deterministic `~/.autosk/worktrees/<slug>/<task-id>` path on
  branch `autosk/<task-id>`, branch-preserving terminal release, dir-kept on
  sibling/human-park, missing-dir re-allocation). See
  [`extensions/worktree/README.md`](extensions/worktree/README.md).

- **`extensions/pi-agent/`** — `@autosk/pi-agent`: the shipped **agent**
  `piAgent({...})` that drives `pi --mode rpc` over JSON-lines stdio, mirrors
  pi's transcript entries (messages / custom) into the autosk transcript 1:1,
  and bridges step transitions through an injected `autosk_transit` pi-tool
  observed on pi's RPC event stream (plus a private kickback/corrections loop and
  steer / followup / abort forwarding into the live pi). See
  [`extensions/pi-agent/README.md`](extensions/pi-agent/README.md).

- **`extensions/feature-dev/`** — `@autosk/feature-dev`: the shipped **reference
  workflow** `dev → review → docs → validator → accept` (with review→dev /
  validator→dev bounce-backs, a `ctx.visits("dev")` visit cap, and
  `worktreeIsolation()`), wired to four `@autosk/pi-agent` roles. It is
  discovered by the daemon via the bundled-extension seam, so every project can
  enroll into it with no per-project files. See
  [`extensions/feature-dev/README.md`](extensions/feature-dev/README.md).

## Scripts

Run from this directory:

- `bun install` — install + symlink the workspace packages.
- `bun run typecheck` — `tsc --noEmit` across all workspace packages.
- `bun test` — run every package's `*.test.ts` (pure unit tests, no daemon).

To produce the distributable daemon (compiled standalone binary + bundled
extensions), use the repo-root targets `make build-autoskd` / `make install` or
`scripts/package-autoskd.sh <out-dir>` — they wrap `bun build --compile` and the
extension bundler. The compiled binary embeds the Bun runtime, so no global
`bun` is required at runtime.
