# Abolish isolation providers — agents own isolation, cleanup is a workflow step

**Status:** plan (analysis only; no code changes yet).
**Date:** 2026-06-22.
**Owners:** autosk core.
**Supersedes:** [`20260619-Docker-Isolation-Claude-MCP.md`](20260619-Docker-Isolation-Claude-MCP.md)
(the MCP-over-socket / `hostEndpoint`-seam plan) and reframes the predecessor
[`20260618-Docker-Isolation.md`](20260618-Docker-Isolation.md): instead of
extending the `IsolationProvider` contract to make Docker work for Claude Code,
we **remove the isolation-provider abstraction entirely** and let agents own the
isolation they need.
**Related code:**
`daemon/sdk/src/workflow.ts` (`IsolationProvider` / `IsolationHandle` — to delete),
`daemon/sdk/src/agent.ts` (`AgentRunContext` — add `newMCPServer`),
`daemon/core/src/engine/session.ts` (`run` / `buildContext` / isolation lifecycle — to gut),
`daemon/core/src/rpc/daemon.ts` (`taskTerminal` / `reapIsolation` — to simplify),
`daemon/core/src/extensions/registry.ts` (`isolation` render — to drop),
`daemon/extensions/worktree/`, `daemon/extensions/docker/` (providers → folded into one new `@autosk/sandbox`),
`daemon/extensions/pi-agent/`, `daemon/extensions/claude-agent/` (gain a `sandbox` option + a per-session http MCP tool surface),
`daemon/extensions/feature-dev/`, `daemon/extensions/feature-dev-cc/` (gain a cleanup step),
`internal/daemon/api`, `cmd/autosk/{workflow,status_verbs}.go`, `internal/lazy`, `gui/` (drop the `isolation` view + `--force` dirty-gate),
`daemon/sdk/src/proto.ts` (`ErrorCodes.ENVIRONMENT_DIRTY` — retire).

---

## 1. Why replace the predecessor plan

The 06-19 plan tried to make `dockerIsolation()` work for `@autosk/claude-agent`
by *growing* the provider contract: a per-session daemon-hosted MCP server plus a
new `IsolationHandle.hostEndpoint?(port)` reachability seam so the engine could
hand the agent an isolation-correct URL. That works, but it pushes the
provider/engine machinery further into territory the **agent** is better placed
to own: the agent is the only component that simultaneously knows "I am running
`claude` inside Docker with `--add-host=host.docker.internal:host-gateway`" **and**
"`claude` must reach the host MCP server." Splitting that knowledge across an
engine seam (`hostEndpoint`) and an agent (`buildMcpConfig`) is the smell.

Stepping back, the `IsolationProvider` abstraction carries a lot of weight for
two shipped providers (`worktree`, `docker`):

- an engine-driven lifecycle state machine (`acquire` / `release` / `reap`)
  scoped to a task's *active run* (many step-sessions, many agents);
- an **execution seam** (`handle.exec` / `handle.spawn`) whose entire purpose is
  to rewrite `ctx.spawn(["claude", …])` into `docker exec … claude` so the agent
  can stay isolation-agnostic;
- a keep-alive container (`tail -f /dev/null`) with a `run` / `start` / `stop` /
  reuse / `assertRunning` state machine;
- a session-free `reap`-by-identity + a dirty-gate (`ENVIRONMENT_DIRTY`) wired
  into `task.done` / `task.cancel`.

This plan deletes that abstraction. Isolation becomes a userspace concern owned
by agents; the engine and SDK stop knowing about it. The result is dramatically
less code in core, a thinner Docker model (`docker run -i --rm` instead of an
`exec`-into-keepalive container), and the MCP reachability problem collapses into
the one place that has all the context (the agent).

---

## 2. Decision summary

| Concern | Before (provider model) | After (this plan) |
|---|---|---|
| **Abstraction** | `IsolationProvider` + `IsolationHandle` attached to `WorkflowDefinition.isolation` (an SDK/engine contract) | **deleted from the SDK and engine**; isolation lives in a userspace library |
| **Runtime (container)** | provider composes worktree + routes `ctx.exec/spawn` through `docker exec`; keep-alive container + start/stop/reuse | the **agent** spawns `docker run -i --rm --name <det> -v ws:ws -w ws … <harness>`; `onAbort` → `docker stop <name>` |
| **Workspace (worktree)** | engine `acquire` before each step sets `ctx.cwd` | the **agent** calls a shared, deterministic, idempotent `workspace()` helper and runs the harness there |
| **MCP reachability** | engine seam `handle.hostEndpoint?(port)` | the **agent** rewrites the host via its sandbox: `endpointFor(port)` |
| **Cleanup** | engine `reap({projectRoot, taskId}, {force})` on terminal + manual `done`/`cancel` | a **normal workflow step** whose `onRun` tears the env down, then transits to `done`/`cancel` |
| **`done`/`cancel`** | reap-by-identity + dirty-gate (`ENVIRONMENT_DIRTY`, `--force`) | **raw status flip** — no engine teardown, no dirty-gate (env may leak if the task was not routed through cleanup) |
| **`ctx.cwd`** | the worktree path under isolation | **always `projectRoot`**; the agent owns its own run dir |
| **SDK additions** | — | `ctx.newMCPServer()` (per-session host-side MCP server; *MCP serving, not isolation*) |

**Trade-offs explicitly accepted** (see §7): no engine guarantee that a terminal
transition tears the env down, and no dirty-gate protecting uncommitted work.
Both are the price of maximal author freedom; the reference workflows mitigate
the first by shipping a correct cleanup step, and the worktree branch is always
preserved so committed work is never lost.

---

## 3. SDK surface changes (`daemon/sdk/`)

**Delete** (no replacement in the SDK — isolation is no longer an engine
contract):

- `IsolationProvider`, `IsolationHandle`, `IsolationExecOptions`,
  `IsolationSpawnOptions`, `IsolationReapResult` (`workflow.ts`);
- `WorkflowDefinition.isolation` (`workflow.ts`);
- `runChild` / `spawnChild` stay exported (the sandbox lib still uses them), but
  the `Isolation*` types they were paired with go.

**Add** to `AgentRunContext` (`agent.ts`) — this is about MCP serving, not
isolation, and is the only new SDK surface:

```ts
interface AgentRunContext {
  // …existing…
  /**
   * Mints a per-session, host-side MCP server bound to this session's
   * { projectRoot, taskId, author = step, transit = task-mode }. Returns the
   * loopback URL, the bound port, and a bearer token; the agent rewrites the
   * host for its own isolation topology (e.g. host.docker.internal) using the
   * port. Closed automatically on settle; `close()` is an explicit early-release.
   */
  newMCPServer(): Promise<{ url: string; port: number; token: string; close(): Promise<void> }>;
}
```

`ctx.cwd` keeps its type but its *contract* changes: it is now **always the
project root** (no isolation rewrites it). Update the doc comment accordingly.

---

## 4. The userspace sandbox library — one new `@autosk/sandbox` (decided §11)

Isolation moves out of the SDK/engine into **a single new userspace package,
`@autosk/sandbox`**, that the agents import. It absorbs the old `@autosk/worktree`
and `@autosk/docker` providers (which are removed — see the migration note below)
and owns the `Sandbox` shape, both `worktreeSandbox()` / `dockerSandbox()`
factories, and `sandboxCleanupStep()`. The shape below is **structural** (decided
§11): agents accept any object with these methods, so `@autosk/sandbox` documents
it but there is **no nominal `Sandbox` interface to import** — an operator can
hand-roll an exotic sandbox without depending on the package's type.

```ts
// @autosk/sandbox — NOT in @autosk/sdk; this shape is STRUCTURAL (no nominal import)
type Sandbox = {
  /** Ensure the per-task workspace exists (idempotent + deterministic). The dir the harness runs in. */
  workspace(id: TaskIdentity): Promise<{ cwd: string }>;
  /** Wrap the harness argv to run inside the sandbox (docker run …). Identity for host/worktree. */
  wrap(cmd: string[], o: { cwd: string; env?: Record<string, string>; id: TaskIdentity }): string[];
  /** Isolation-correct host endpoint for an in-sandbox process to reach a host port (replaces hostEndpoint). */
  endpointFor(port: number): string;            // host/worktree → 127.0.0.1; docker → host.docker.internal
  /** Best-effort stop of the running sandbox process tree (agent onAbort). */
  stop(id: TaskIdentity): Promise<void>;
  /** Terminal teardown for the cleanup step: remove workspace (+ any stray container). */
  cleanup(id: TaskIdentity, opts: { force: boolean }): Promise<{ removed: boolean; dirty: boolean; detail?: string }>;
};
type TaskIdentity = { projectRoot: string; taskId: string };

export function worktreeSandbox(opts?: { home?: string; gitBin?: string }): Sandbox;
export function dockerSandbox(opts: { image: string; env?: Record<string, string>;
  mounts?: { hostPath: string; sandboxPath: string; readonly?: boolean }[]; /* +user/HOME, see §8 */ }): Sandbox;
export function sandboxCleanupStep(sandbox: Sandbox, opts?: { to?: StepTarget; force?: boolean }): AgentDefinition;
```

**Package migration.** `@autosk/worktree` and `@autosk/docker` are **removed**;
their consumers (the reference workflows, agents) move to `@autosk/sandbox`. The
first-run bootstrap / `settings.json` references and the install docs update to
the new package name. The deterministic derivations move verbatim.

Implementations reuse the existing, **byte-identical** deterministic derivations
so already-allocated worktrees/branches/containers still resolve:

- `worktreeSandbox`: `workspace()` = today's worktree `acquire` body
  (create | reuse on branch `autosk/<task>`); `wrap()` = identity; `endpointFor`
  = `http://127.0.0.1:<port>`; `stop()` = no-op; `cleanup()` = today's worktree
  `reap` body (rm dir, **preserve branch**, dirty detection).
- `dockerSandbox`: composes `worktreeSandbox` for `workspace()`; `wrap()` =
  `docker run -i --rm --name <containerName(id)> --add-host=host.docker.internal:host-gateway
  -v <ws>:<ws> -w <ws> -e … <image> <cmd…>`; `endpointFor` =
  `http://host.docker.internal:<port>`; `stop()` = `docker stop <name>` (covers a
  SIGKILL orphan); `cleanup()` = worktree cleanup **plus** a defensive
  `docker rm -f <name>` (a `--rm` container normally self-removes on exit; this
  only catches a daemon-crash orphan). `containerName`/`slugFor` keep today's
  formula.

The `tail -f /dev/null` keep-alive, `docker exec`, `docker start`,
`assertRunning`, and the whole RUNNING/DORMANT state machine are **gone** — a
container lives exactly as long as one harness process (`docker run -i --rm`).

---

## 5. Agent integration (`pi-agent`, `claude-agent`)

Both shipped agents gain an optional `sandbox?: Sandbox` (a plain object the
workflow author passes per step). **Both** also move their tool surface onto the
per-session http MCP server (decided §11), so neither needs `autosk`/`autoskd`/a
mounted socket in the container — the image is thin for both harnesses.

### 5.1 claude-agent

`onRun` (task mode) becomes:

```ts
const id = { projectRoot: ctx.projectRoot, taskId: ctx.tasks.currentId };
const ws = opts.sandbox ? await opts.sandbox.workspace(id) : { cwd: ctx.cwd };
const mcp = await ctx.newMCPServer();
const url = opts.sandbox ? opts.sandbox.endpointFor(mcp.port) : mcp.url; // isolation-correct
const cfg = { mcpServers: { autosk: { type: "http", url, headers: { Authorization: `Bearer ${mcp.token}` } } } };
const cmd = buildClaudeCommand(opts, { mcpConfig: JSON.stringify(cfg) });
const argv = opts.sandbox ? opts.sandbox.wrap(cmd, { cwd: ws.cwd, env: autoskEnv(ctx), id }) : cmd;
const child = ctx.spawn(argv, { cwd: ws.cwd, env: autoskEnv(ctx) });
// …drive to a single transit (unchanged)…
```

- **MCP config is always `type:"http"`** now (the per-session host server),
  whether or not there is a sandbox — the stdio `autoskd mcp` path is retired for
  the agent surface (the standalone `autoskd mcp` subcommand stays for external
  use). Tool names (`mcp__autosk__*`), prompts, the driver's transit observation,
  and the allowlist are unchanged.
- **`onAbort`** calls `opts.sandbox?.stop(id)` before/with the existing driver
  shutdown, closing the SIGKILL-orphan gap.
- **Interactive (taskless) mode** ignores `sandbox` for the workspace (no task ⇒
  no per-task worktree); a sandbox may still `wrap` the chat harness into a
  `--rm` container if desired (new capability, optional). `newMCPServer()` in
  interactive mode binds with `transit:false`.

### 5.2 pi-agent (http tool surface — net-new, flagged risk)

pi does **not** consume MCP natively: its tools are pi-extensions — the in-repo
`pi-transit-extension.ts` (transit) plus the external `@autosk/pi-tools`
(`autosk_task` / `autosk_comment`, which shell out to `autosk --json`). To put pi
on the same per-session http server (so its docker image is thin too), ship a
**new in-repo pi-extension that is an HTTP client** of the MCP server: the agent
builds the endpoint with `sandbox.endpointFor(mcp.port)` + `mcp.token` and injects
it as env (`AUTOSK_MCP_URL` / `AUTOSK_MCP_TOKEN`); the extension registers
`autosk_transit` / `autosk_task` / `autosk_comment` as pi-tools that POST to that
endpoint instead of shelling out. It runs inside pi (in the container) and reaches
the host over `host.docker.internal`, so the image needs neither `autosk` nor the
mounted daemon socket.

> **Risk / dependency.** This is net-new (it replaces the `@autosk/pi-tools` +
> `autosk` shell-out under the sandbox) and assumes pi can `-e` an extension that
> performs `fetch()`. If that proves blocked, the fallback is the predecessor's
> pi-under-docker model (image ships `autosk`, `dockerSandbox` mounts the daemon
> socket) — `dockerSandbox` keeps an optional `mountSocket` for exactly this
> escape hatch. Off-docker (host/worktree) pi keeps its current pi-tools path
> unchanged.

---

## 6. Cleanup as a workflow step

No `onCleanup` agent hook and no engine `reap`. Instead the sandbox lib ships a
helper that builds a normal agent step:

```ts
export function sandboxCleanupStep(sandbox: Sandbox, opts?: { to?: StepTarget; force?: boolean }): AgentDefinition;
// onRun(ctx):
//   const r = await sandbox.cleanup({ projectRoot: ctx.projectRoot, taskId: ctx.tasks.currentId },
//                                   { force: opts?.force ?? true });
//   await ctx.comment(r.removed ? `cleaned up isolation env${r.detail ? ` (${r.detail})` : ""}`
//                               : "no isolation env to clean up");
//   await ctx.transit(opts?.to ?? { status: "done" });
```

It runs as an ordinary session (full `AgentRunContext`, host `exec`/`spawn`, no
special context), at `projectRoot` (so it never sits inside the dir it removes).
Workflow authors wire it into the graph wherever teardown should happen; users
can also route a task into it on demand (`autosk resume <id> --to cleanup`). The
"which agent owns cleanup" problem from the hook approach disappears — it is just
a step the engine schedules like any other.

Crash recovery is unchanged (interrupted sessions → `failed` → task parked
`human`); a parked task's env is reaped only when it is later routed through the
cleanup step (or torn down by hand). A `--rm` container orphaned by a daemon
SIGKILL is mopped up by the cleanup step's defensive `docker rm -f`.

---

## 7. Engine changes (`daemon/core/`)

**`engine/session.ts`** — remove the entire isolation concern:

- delete the `isolation` field/acquire in `run()`; `this.cwd` stays `projectRoot`;
- delete the exec/spawn seam routing in `buildContext` — `ctx.exec` / `ctx.spawn`
  always use the host helpers at `this.cwd`;
- delete `quiesceIsolation`, `reapIsolationTerminal`, `isolationQuiesced`,
  `isolationReaped`, and the `release`/`reap` branches in `commitTransit` and the
  finalisers; a `step` / `human` / `done` / `cancel` target now only writes
  position + seals the session;
- add `ctx.newMCPServer()` wiring: mint the per-session HTTP MCP server (bound to
  `{ projectRoot, taskId, author = step, transit = task-mode }`), track it, and
  `close()` it in the settle funnel + detach/crash-recovery cleanup (host-owned
  so it survives an aborted harness child). The returned `close()` is an
  **explicit early-release** for the agent; the engine backstop closes on **every**
  settle/finaliser/detach regardless (decided §11) so a forgetful agent never
  leaks a port across steps.

**`rpc/daemon.ts`** — `taskTerminal` (`task.done` / `task.cancel`) becomes a
**raw status flip**: keep the live-session conflict check and the claim-race
rollback; delete `reapIsolation` and the `ENVIRONMENT_DIRTY` branch.

**`extensions/registry.ts`** — drop `isolation: wf.isolation?.tag ?? "none"` from
the rendered `WorkflowInfo`.

**Per-session MCP server (`core/src/mcp/`)** — the one piece carried over from the
06-19 plan, minus the `hostEndpoint` seam:

- a hand-rolled `Bun.serve()` Streamable-HTTP endpoint (POST → `application/json`,
  stateless, no `Mcp-Session-Id`, ephemeral `port: 0`), reusing the
  transport-agnostic `handleMessage` / `callTool` from `mcp/server.ts`; per-request
  bearer validation (wrong/missing → 401); no `@modelcontextprotocol/sdk` (must
  survive `bun build --compile`);
- a **direct-store** run function for `task` / `comment` (target the daemon's own
  store/engine for `(project, author)`; no `autosk` child). **Keep** the
  shell-out path for the standalone `autoskd mcp` stdio server so it does not
  regress;
- per-session binding: `author = step`; `transit` advertised only for task-mode
  (ack-only, still observed on stdout by the driver); `comment` default author =
  the step; explicit ids honoured in args.

---

## 8. `dockerSandbox` runtime contract (image, mounts, UID/GID, HOME)

Carried from the 06-19 config-injection analysis, now owned by the agent's
sandbox object rather than the provider:

- **First-class `mounts: { hostPath, sandboxPath, readonly }[]`** (with `~`-expand
  + single-file parent-dir create/chown) instead of stringly-typed `-v` args —
  the canonical way to inject `~/.claude` (creds + skills + settings) and caches.
- **UID/GID alignment** — default `--user $(hostuid):$(hostgid)` + an image
  build-arg convention so the bind-mounted worktree/creds are host-owned and
  writable without runtime chown.
- **HOME convention** (e.g. `/home/agent`) so `~/.claude` mounts land where Claude
  looks.
- **Thin image (both agents):** the harness (`claude`, authenticated for
  unattended runs; or `pi`) + the build/test toolchain. **No** `socat`, **no**
  `autosk`/`autoskd`, **no** mounted daemon socket — both agents' tool surface is
  the host-hosted http MCP server reached via `host.docker.internal` (§5). An
  optional `mountSocket` remains only for the pi fallback in §5.2.
- **Claude auth:** API key via `dockerSandbox({ env: { ANTHROPIC_API_KEY } })`, or
  subscription via mounting `~/.claude/.credentials.json:ro` (macOS host: the
  token is in the Keychain — export with `security find-generic-password -w -s
  "Claude Code-credentials"`).

---

## 9. Reference-workflow migration + Go/CLI/TUI/GUI surface

**Workflows** (`feature-dev`, `feature-dev-cc`): replace `isolation:
worktreeIsolation()` with (a) a `sandbox` passed to each agent step
(`dev: claudeAgent({ sandbox, firstMessage })`, …) and (b) a new `cleanup` step
(`sandboxCleanupStep(sandbox)`), rewiring `accept (human) → cleanup → done` (and
any `cancel` path) through it. A `feature-dev-cc-docker` sibling wires
`dockerSandbox({ image })` instead of `worktreeSandbox()` under a distinct
workflow `name`. **This is mandatory, not optional:** with `done`/`cancel` now a
raw flip, a workflow without a cleanup step leaks its worktree on every task.

**Go / proto / CLI / TUI / GUI** (user-visible — the direction explicitly accepts
wire changes):

- remove the `isolation` field from `WorkflowInfo` (proto-v2 + the `internal/daemon/api`
  mirror) and from `autosk workflow show` / the TUI / the GUI workflow views;
- retire the dirty-gate: **hard-remove** the `-f/--force` flag from `autosk done`/
  `cancel` (decided §11 — a force that no longer forces anything is misleading, so
  an unknown-flag error is clearer than a silent no-op), stop emitting
  `ENVIRONMENT_DIRTY`, and drop the TUI/GUI force-confirm prompt;
- leave `ErrorCodes.ENVIRONMENT_DIRTY = 1005` reserved in `proto.ts` (don't reuse
  the number) but mark it retired.

---

## 10. Test plan

- **SDK/types:** `bun run typecheck` is green after the `Isolation*` deletions;
  no dangling imports.
- **Sandbox lib unit:** `worktreeSandbox.workspace` create/reuse + `cleanup`
  (dirty detection, branch preserved) match today's worktree provider tests;
  `dockerSandbox.wrap` emits `docker run -i --rm --add-host … -v ws:ws -w ws …`;
  `endpointFor` → `host.docker.internal` (docker) / `127.0.0.1` (worktree);
  `containerName`/`slugFor` byte-identical to the previous provider.
- **Agent unit:** `claude-agent` emits `type:"http"` MCP config with the bearer;
  the new pi-agent http pi-extension POSTs to `AUTOSK_MCP_URL` with the bearer
  (transit/task/comment) instead of shelling out to `autosk`; with a `sandbox` the
  endpoint host is rewritten via `endpointFor(mcp.port)` and the harness argv is
  wrapped; without one each runs on the host at `ctx.cwd`; `onAbort` calls
  `sandbox.stop`; tool names + allowlist unchanged.
- **Daemon unit:** the HTTP MCP listener frames/answers (POST→JSON, 401 on bad
  bearer); direct-store `task`/`comment` parity with the shell-out shape;
  per-session binding (author = step, transit gated by task-mode); `taskTerminal`
  is a pure flip (no reap, no `ENVIRONMENT_DIRTY`).
- **Engine unit:** no acquire/release/reap calls remain; `ctx.cwd` = projectRoot;
  the per-session MCP server is minted on run and closed on every settle/finaliser.
- **`sandboxCleanupStep` unit:** removes the env, comments, and transits to the
  configured target; idempotent on a missing env.
- **Integration (gated on docker):** a thin image (harness + toolchain; no
  autosk/autoskd/socat, no socket mount) runs a full `feature-dev-cc-docker` task:
  the harness in a `--rm` container reaches the host MCP over
  `host.docker.internal` (bearer), calls `comment`/`task` (real store writes), a
  `transit` is observed and committed, and the `cleanup` step removes the worktree
  (branch preserved). A pi sibling (`feature-dev-docker`) exercises the pi http
  pi-extension over the same path.
- **Go:** `make test` green after the `WorkflowInfo.isolation` + `--force` view
  changes.
- **CHANGELOG/docs:** `## [Unreleased]` records the removed `isolation` view +
  retired dirty-gate (Changed/Removed) and the agent-owned-sandbox model
  (Added/Changed); `docs/workflows.md` + `docs/extensions.md` rewrite the
  "Isolation" sections to the sandbox-library + cleanup-step model.

---

## 11. Open questions / decisions

**Resolved (in conversation, 2026-06-22):**

- **Abolish the provider abstraction** (full, not a thin-provider hybrid).
- **Cleanup is a normal workflow step**, not an agent hook or an engine reap.
- **`done`/`cancel` are raw flips** — no teardown guarantee, no dirty-gate.

**Resolved (2026-06-22) — the former open questions:**

1. **Package layout → one new `@autosk/sandbox`.** It owns the `Sandbox` shape,
   `worktreeSandbox()` / `dockerSandbox()`, and `sandboxCleanupStep()`;
   `@autosk/worktree` and `@autosk/docker` are removed (§4). Cleaner home for the
   shared shape than wedging the base type into `@autosk/worktree`.
2. **`Sandbox` is structural, not a nominal export.** `@autosk/sandbox`
   documents the shape; agents type it structurally so a hand-rolled sandbox
   needs no dependency on the package's type (§4).
3. **`newMCPServer()` close = explicit `close()` + engine backstop.** The agent
   may release early; the engine closes on every settle/finaliser/detach
   regardless, so no port leaks across steps (§7).
4. **`--force` is hard-removed** from `done`/`cancel` (not a deprecated no-op):
   with the dirty-gate gone the flag means nothing, and an unknown-flag error is
   clearer than a silent no-op (§9).
5. **pi-agent adopts the http tool surface now** (not deferred): ship a new
   in-repo pi-extension HTTP client so pi's docker image is thin too (§5.2). Net-
   new work with a flagged pi-`fetch()` dependency + a documented socket-mount
   fallback.
