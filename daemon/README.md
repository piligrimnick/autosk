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
  (Task / Session / Workflow / Agent / `AutoskAPI`), the pi-format
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
    under `"extensions"` in `settings.json`), in-process factory loading with
    full error isolation (a broken extension — or one with an invalid step shape
    — is recorded as a load diagnostic and never crashes the daemon), the
    first-run **bootstrap** that npm-installs the default `@autosk/feature-dev`
    workflow into `~/.autosk/packages/` when `~/.autosk/settings.json` is absent,
    and the live-code hazard guard that parks any in-flight task whose
    workflow/step has vanished from the registry to `human`.
  - **engine** (`src/engine/`) — the scheduler (a single event-driven scan + a
    global FIFO worker pool, `--workers`, shared across projects, plus a slow
    safety rescan), the session lifecycle, `ctx.transit` (onTransit validation →
    atomic `task.json` commit), the pi-format transcript writer, the
    `AgentRunContext` (tasks/workflows/log/comment/exec/spawn/newMCPServer — the
    per-session host MCP server is minted on run and closed by an engine
    backstop on every settle), steer/followup/abort routing, and crash recovery
    (interrupted
    sessions → `failed: daemon_restart`, task → `human`).
  - **JSON-RPC v2 server** (`src/rpc/`) — JSON-lines over UDS (default
    `~/.autosk/daemon.sock`, `$AUTOSK_SOCK`) plus a TCP transport with token auth
    (on by default at `0.0.0.0:7077`, override via `--tcp [HOST:]PORT`);
    single-instance via an atomic pidfile lock; notification fan-out
    (`task-changed`, `session-event`, `project-changed`); `session.subscribe`
    replay-then-tail; and idle-shutdown.

- **`extensions/sandbox/`** — `@autosk/sandbox`: the userspace **sandbox
  library** — the structural `Sandbox` shape plus `worktreeSandbox()` /
  `dockerSandbox({ image })` / `sandboxCleanupStep()`. Isolation is no longer an
  engine/SDK concern: agents own the isolation they need by wrapping their
  harness with a `Sandbox`, and teardown is a normal workflow step. Absorbs the
  retired `@autosk/worktree` + `@autosk/docker` providers (byte-identical slug /
  branch / container-name derivations, so an already-allocated worktree/branch
  resolves to the same place). See
  [`extensions/sandbox/README.md`](extensions/sandbox/README.md).

- **`extensions/pi-agent/`** — `@autosk/pi-agent`: the shipped **agent**
  `piAgent({...})` that drives `pi --mode rpc` over JSON-lines stdio, mirrors
  pi's transcript entries (messages / custom) into the autosk transcript 1:1,
  and bridges step transitions through an injected `autosk_transit` pi-tool
  observed on pi's RPC event stream (plus a private kickback/corrections loop and
  steer / followup / abort forwarding into the live pi). See
  [`extensions/pi-agent/README.md`](extensions/pi-agent/README.md).

- **`extensions/claude-agent/`** — `@autosk/claude-agent`: the shipped **agent**
  `claudeAgent({...})`, the structural twin of `@autosk/pi-agent` that drives
  [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude -p`
  headless stream-json) instead of `pi --mode rpc`. It mirrors Claude's stream
  entries into the autosk transcript 1:1 and exposes its transit / task / comment
  tools over the per-session host HTTP MCP server the engine mints for the run
  (`ctx.newMCPServer()`, advertised to Claude via `--mcp-config type:"http"` with
  a per-session bearer). The standalone `autoskd mcp` stdio server survives for
  external use. Not provisioned by the first-run bootstrap — an
  opt-in alternative harness. See
  [`extensions/claude-agent/README.md`](extensions/claude-agent/README.md).

- **`extensions/feature-dev/`** — `@autosk/feature-dev`: the **reference
  workflow** `dev → review → docs → validator → accept (human) → cleanup → done`
  (with review→dev / validator→dev bounce-backs and a `ctx.visits("dev")` visit
  cap), wired to four `@autosk/pi-agent` roles. Each agent step runs in a per-task
  `worktreeSandbox()`, and the `cleanup` step (`sandboxCleanupStep()`) tears the
  worktree down (branch preserved) before `done`. It is published to npm and
  provisioned by the daemon's first-run bootstrap, so every project can enroll
  into it with no per-project files. See
  [`extensions/feature-dev/README.md`](extensions/feature-dev/README.md).

## Scripts

Run from this directory:

- `bun install` — install + symlink the workspace packages.
- `bun run typecheck` — `tsc --noEmit` across all workspace packages.
- `bun test` — run every package's `*.test.ts` (pure unit tests, no daemon).

To produce the distributable daemon (compiled standalone binary), use the
repo-root targets `make build-autoskd` / `make install` or
`scripts/package-autoskd.sh <out-dir>` — they wrap `bun build --compile`. The
compiled binary embeds the Bun runtime, so no global `bun` is required at
runtime. The extensions ship separately as npm packages (published from
`sdk/` + `extensions/*`; see `scripts/publish-extensions.sh`).
