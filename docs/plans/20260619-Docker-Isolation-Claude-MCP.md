# Docker isolation for `feature-dev-cc` (Claude Code) — MCP-over-socket plan

**Status:** plan (analysis only; no code changes yet).
**Date:** 2026-06-19.
**Owners:** autosk core.
**Predecessor:** [`20260618-Docker-Isolation.md`](20260618-Docker-Isolation.md) —
shipped `dockerIsolation()` + the `IsolationHandle` exec/spawn seam. That plan was
written around **`pi-agent`**; this plan covers the gap that surfaces when the
docker provider is paired with **`@autosk/claude-agent`** (the `feature-dev-cc`
workflow).
**Related code:**
`daemon/extensions/docker/src/index.ts` (the provider),
`daemon/extensions/feature-dev-cc/src/index.ts` (the workflow),
`daemon/extensions/claude-agent/src/index.ts` (`buildMcpConfig`, `resolveAutoskdBin`, `autoskEnv`),
`daemon/core/src/mcp/{server,tools,cli}.ts` (the `autoskd mcp` stdio server),
`daemon/core/src/rpc/uds.ts` (`listenUnix` — single-instance UDS infra),
`daemon/core/src/engine/session.ts` (`run` / `buildContext` / isolation lifecycle),
`daemon/sdk/src/workflow.ts` (the isolation contract).

---

## 1. Problem statement

`dockerIsolation()` already runs the agent's whole process tree inside a per-task
container: the engine routes `ctx.spawn(["claude", …])` through the handle's
`spawn`, rewriting it to `docker exec -i -w <wt> -e … <container> claude …`. That
works for the *top-level* command.

It does **not** work for `claude-agent`'s **tool surface**, because Claude Code's
tools come from a **nested child process** that `claude` itself spawns from the
`--mcp-config` JSON — and the seam cannot reach a nested command:

1. **The MCP server `command` is a host path.** `buildMcpConfig` bakes
   `command: resolveAutoskdBin()` (= `$AUTOSKD_BIN` / `process.execPath`, the
   **daemon host's** `autoskd`) into the `--mcp-config` JSON. Inside a Linux
   container that absolute path does not exist → `claude` fails to start the
   `autosk` MCP server → no `transit` / `task` / `comment` tools → the model
   never transits → the engine parks every task as `agent_did_not_transit`.
2. **Even if `autoskd` were in the container**, the `autoskd mcp` server then
   shells out to **`autosk --json`** (`daemon/core/src/mcp/cli.ts`), so the image
   would need **two** container-arch binaries (`autoskd` *and* `autosk`).

By contrast, `pi-agent` is self-contained under docker: its `@autosk/pi-tools`
run **inside pi's own process** and shell out only to `autosk` (on `PATH`), so the
predecessor plan's "image ships `pi` + `autosk`" is enough. `claude-agent` is the
odd one out because its tools are an out-of-process daemon-side binary with a
**host-resolved** path.

---

## 2. Current flow recap (what runs, what is created)

When a `work` task with no live session is dispatched (`SessionRuntime.run()`, in
the worker pool):

| Step | Call | Entities created / effect |
|---|---|---|
| 1. acquire | `wf.isolation.acquire({projectRoot, taskId})` → `dockerIsolation.acquire` | |
| 1a. inner | `inner.acquire` (worktree) | git **worktree** `~/.autosk/worktrees/<slug>/<task>` + **branch** `autosk/<task>` on the host |
| 1b. probe | `ensureDockerAvailable` + `docker inspect` | decides create / start / reuse |
| 1c. run | `docker run -d --name autosk-<slug>-<task> -v <wt>:<wt> -v <sock>:<sock> -e AUTOSK_SOCK=<sock> --entrypoint tail <image> -f /dev/null` | **container** (keep-alive), worktree + daemon UDS bind-mounted 1:1 |
| 1d. handle | return `{cwd: wt.cwd, meta:{container}, exec, spawn}` | `this.cwd` = worktree; `exec/spawn` → `docker exec` |
| 2. claim | `setHeaderCwd` + `patchMetaIf queued→running` | **session meta** + transcript header |
| 3. run | `agent.onRun(ctx)` → `claudeAgent.onRun` | |
| 3a. mcp cfg | `buildMcpConfig(ctx)` | `--mcp-config` JSON: `{command: resolveAutoskdBin(), args:["mcp"], env:{AUTOSK_CWD,AUTOSK_AGENT,AUTOSK_SOCK,AUTOSK_MCP_TRANSIT}}` |
| 3b. spawn | `ctx.spawn(["claude",…], {cwd, env})` → `handle.spawn` | `docker exec … claude …` → **claude runs in the container** |
| 3c. tools | claude spawns the MCP server: `command = resolveAutoskdBin()` | **❌ host path — does not exist in the container** |
| 4. transit | driver observes `mcp__autosk__transit` tool_use → `ctx.transit(to)` | task position + visit count + transcript |
| 5a. step→step | — | container stays RUNNING (no release, no reap) |
| 5b. →human | `release` | `docker stop` (DORMANT) |
| 5c. →done/cancel | `release` + `reap(force:true)` | `docker stop`, dirty-gate via worktree reap, `docker rm -f`, worktree removed (branch kept) |

The break is **step 3c**. Everything else already works.

> Note on transit: the real transition is driven by the **driver observing the
> `tool_use` on claude's stdout** (piped back through `docker exec`), *not* by the
> MCP server. So transit keeps working regardless of MCP transport — the MCP
> server only has to **advertise** the `transit` tool (ack-only) so the model can
> call it. `task` / `comment` are the tools that actually execute server-side.

---

## 3. Can we serve MCP over the mounted socket? (verified)

Yes — with three facts that shape the design:

1. **Claude Code transports = `stdio` / `http` (streamable, TCP) / `sse`
   (deprecated).** There is **no native unix-socket transport**; MCP-over-UDS is
   an open feature request, not shipped.
2. **MCP spec:** custom transports over a reliable byte stream (UDS or TCP)
   **SHOULD reuse the stdio framing** — newline-delimited JSON. So a `.sock`
   carrying MCP uses the *same* NDJSON the stdio server already speaks.
3. Therefore the bridge is trivial: a **`type:"stdio"`** MCP entry whose
   `command` is a byte-pump (`socat - UNIX-CONNECT:<mcp.sock>` or `nc -U
   <mcp.sock>`) baked into the operator image. `claude` ⇄ stdio ⇄ `socat` ⇄
   mounted UDS ⇄ **daemon-hosted MCP server**.

Consequences:

- The existing `daemon.sock` (proto-v2 JSON-RPC) **cannot** be reused as-is — it
  is a different protocol. Expose a **dedicated MCP socket**.
- If the daemon serves MCP **in-process** (calls the store/engine directly), the
  container needs **neither `autoskd` nor `autosk`** — only `claude` + `socat`.
  This eliminates the cross-arch binary problem entirely.

---

## 3b. Recommended transport: per-session MCP over TCP/HTTP (`ctx.newMCPServer()`) — PoC-validated

Rather than a UDS + `socat` bridge, the daemon hosts a **per-session Streamable
HTTP MCP server** and the agent connects to it over the network. This is the
recommended primary direction (it also unblocks remote/VM providers — the only
transport that works for host + docker + remote uniformly).

Shape:

```ts
const mcp = await ctx.newMCPServer();   // bound to {project, taskId, author=step, transit}
// mcp = { url, token, close() }; url is isolation-correct (see handle.hostEndpoint)
const cfg = { mcpServers: { autosk: {
  type: "http", url: mcp.url, headers: { Authorization: `Bearer ${mcp.token}` },
} } };
ctx.spawn(["claude", "--mcp-config", JSON.stringify(cfg), ...], { cwd: ctx.cwd });
```

- **Server:** `Bun.serve()` POST endpoint, reusing `handleMessage`/`callTool`
  (`mcp/server.ts`) with a direct-store run function (no `autosk` shell-out). A
  minimal Streamable HTTP — POST → `application/json` (no SSE), stateless (no
  `Mcp-Session-Id`). transit stays ack-only (observed on stdout by the driver).
- **Reachability:** extend the seam — `handle.hostEndpoint?(port): string` →
  `http://host.docker.internal:<port>` for docker, `http://127.0.0.1:<port>` for
  host/worktree — so `ctx.newMCPServer()` returns an isolation-correct URL. The
  docker provider also adds `--add-host=host.docker.internal:host-gateway`.
- **Auth:** a per-session random bearer token (TCP has no fs perms); validated
  per request. Bind an ephemeral port (`port: 0`).
- **Lifecycle:** minted in `run()`, `close()`d in the settle funnel
  (`commitTransit` + every finaliser). Fully host-owned — survives a `docker
  exec` orphan on abort.
- **Image contract (thin):** `claude` + the dev toolchain only. No `socat`, no
  `autosk`/`autoskd`.

### PoC results (validated 2026-06-19; `~/me/dev/autosk-mcp-poc/`)

Against `claude 2.1.181` + `docker 28.5.1`, macOS arm64 (Docker Desktop):

1. Claude's `type:"http"` client connects to a hand-rolled `Bun.serve` server
   answering POSTs with `application/json` — **no SSE** needed
   (`mcp_servers:[{name:"poc",status:"connected"}]`; both tools called).
2. The bearer token from `--mcp-config` `headers` **reaches the server** on every
   POST; a wrong token → `401`.
3. Stateless works — Claude sends `Accept: application/json, text/event-stream`
   and accepts JSON replies; no `Mcp-Session-Id` required.
4. Per-session `{project, author}` binding echoed by a `whoami` tool — zero env
   plumbing.
5. A containerised process reaches the host server via
   `host.docker.internal:<port>` (`--add-host=...:host-gateway`); bearer enforced
   from the container too.

6. **Validated end-to-end** (2026-06-19): a real `claude` 2.1.183 *inside* a thin
   container (node:22 + `@anthropic-ai/claude-code`; no autosk/autoskd/socat)
   reached the host MCP server via `host.docker.internal`, authed by
   **subscription**, and called both tools (`whoami` echoed the per-session
   binding; server log confirmed the request came from the container). The
   container `system/init` shows the http server `pending` (Claude connects http
   MCP lazily) then resolves — the tools list + call succeed.

macOS subscription note (proven): the token is in the **Keychain**, so export it
to a file and mount it — `security find-generic-password -w -s "Claude
Code-credentials" > .claude/.credentials.json` (rw, for refresh).

---

## 4. Alternative: in-daemon MCP over a per-session UDS

```
  ┌──────────────── daemon host (autoskd) ─────────────────┐
  │ session.run():                                          │
  │   acquire() → dockerIsolation (worktree + container)    │
  │   mint per-session MCP UDS  <mcpdir>/<sessionId>.sock   │
  │     bound to {project, taskId, author=step, transit}    │
  │   handler = handleMessage()  ── calls the STORE directly │
  │   ctx.spawn(["claude", …, --mcp-config {stdio:socat}])  │
  └───────────────┬─────────────────────────────────────────┘
                  │ docker exec (worktree + <mcpdir> mounted 1:1)
                  ▼
  ┌──────── container (image: claude + socat) ─────────────┐
  │ claude  ──stdio──►  socat - UNIX-CONNECT:<sock>  ───────┼──► daemon MCP
  │   tools: mcp__autosk__{transit,task,comment}            │
  └─────────────────────────────────────────────────────────┘
```

### 4.1 Daemon: an MCP-over-UDS listener (`daemon/core/src/mcp/`)

- Reuse the **transport-agnostic** `handleMessage()` / `callTool()` already in
  `mcp/server.ts` (they take a decoded JSON-RPC message and return a response).
  Add a `net.createServer()` NDJSON framing loop (mirror `runMcpServer`'s stdin
  loop, and the framing in `rpc/uds.ts`), one socket per session.
- **Swap the shell-out for direct store access.** Today `callTask` / `callComment`
  run `autosk --json` (`mcp/cli.ts`). For the in-daemon server, inject a
  `RunProcess`-equivalent that targets the daemon's own store/engine for
  `(project, author)` — no child process, no `autosk` binary. (Keep the
  shelling-out path for the standalone `autoskd mcp` stdio server so nothing
  regresses.)
- **Context binding** is the crux (today carried via the `--mcp-config` env
  `AUTOSK_CWD` / `AUTOSK_AGENT` / `AUTOSK_MCP_TRANSIT`). With an in-daemon socket,
  the *connection itself* is the identity: the engine mints a per-session socket
  pre-bound to `{projectRoot, taskId, author = step, transitEnabled = task-mode}`.
  No env plumbing into the container.
  - `transit` advertised only for task-mode sessions (known from the binding).
  - `comment` default author = the step (matches current behaviour).
  - `task` / `comment` still take explicit ids in the args.

### 4.2 Engine / session lifecycle (`engine/session.ts`)

- The per-session MCP socket is a **session-scoped** resource (author = step), so
  mint it in `run()` next to `setHeaderCwd`, and **remove it on settle** (every
  finaliser + `commitTransit`). The container is per-task (reused across steps),
  so the container must mount a **per-task directory** of sockets, not a single
  socket file (a running container cannot gain a new mount per step).
- Where does the socket dir live? A deterministic per-task path
  (`<mcpdir>/<slug>/<task>/`) the provider can mount 1:1, with the engine writing
  `<sessionId>.sock` into it per step.

### 4.3 `claude-agent` (`buildMcpConfig`)

- When the engine signals "MCP is served over a socket" (a new ctx field, e.g.
  `ctx.mcpSocket?: string`, or a `claudeAgent` option), build the `--mcp-config`
  as a **stdio bridge** instead of `autoskd mcp`:
  ```jsonc
  { "mcpServers": { "autosk": {
      "type": "stdio",
      "command": "socat",
      "args": ["-", "UNIX-CONNECT:<container-path-to-session.sock>"]
  } } }
  ```
- The transit/task/comment tool *names* the model sees are unchanged
  (`mcp__autosk__*`), so prompts, the driver's transit observation, and the
  allowlist all stay as-is.
- Keep `resolveAutoskdBin()` / the env-baked stdio path for the **host / worktree**
  case (no behaviour change off-docker).

### 4.4 `dockerIsolation` (`docker/src/index.ts`)

- Mount the per-task MCP socket directory 1:1 (same trick as the worktree + the
  daemon UDS): `-v <mcpdir>:<mcpdir>`.
- Drop the requirement that the image ship `autosk` for the claude path (still
  needed for the pi path).
- No change to the deterministic container naming / acquire / release / reap.

### 4.5 The operator image (thin)

For `feature-dev-cc` under docker the image must ship:

- **`claude`** (the Claude Code CLI), authenticated for unattended runs.
- **`socat`** (or `nc -U`) — the MCP byte-pump.
- The project's **build/test toolchain** (git, language runtimes, …) — this is
  the actual reason to sandbox.
- **No** `autosk` / `autoskd` needed for the tool surface (MCP is daemon-hosted).

### 4.6 A docker-enabled workflow

`feature-dev-cc` already accepts `isolation` via `FeatureDevCcWorkflowOptions`.
Add a sibling extension (or an env/opt switch) that registers
`featureDevCcWorkflow({ isolation: dockerIsolation({ image }) })` under a distinct
`name` (e.g. `feature-dev-cc-docker`). Inline agents are workflow-scoped (the step
key IS the agent name, resolved per dispatch), so reusing `dev`/`review`/`docs`/
`validator` across two workflows does **not** collide.

---

## 4c. Configuration injection (auth / skills / settings) — provider responsibility

The MCP control-plane config (url+token) rides on the `--mcp-config` **argv** the
agent builds, so it needs no mount/env. Everything else the agent runtime needs
(Claude auth, skills, settings, `CLAUDE.md`) is the **isolation provider's** job
— the agent stays isolation-agnostic. Model it in three layers (mirrors
sandcastle: image / env / mounts):

| Config | Layer | Mechanism |
|---|---|---|
| CLI + global skills/settings, system tooling | **image** | the operator image (the contract); UID/GID build-arg alignment |
| secrets / auth (`ANTHROPIC_API_KEY`, …) | **`env`** | `dockerIsolation({ env })` → `docker run -e`, inherited by every `docker exec` |
| host creds/skills/settings (`~/.claude`), caches | **mounts** | bind-mount (see proposed `mounts` option) |
| project `CLAUDE.md` / `.claude/` | (none) | already in the bind-mounted worktree (a checkout of the repo) |
| autosk MCP url+token | (none) | `--mcp-config` argv (§3b) |

**Gaps to close in `dockerIsolation` (currently `image` / `env` / raw `runArgs`
/ `autoskBin`):**

1. **First-class `mounts: {hostPath, sandboxPath, readonly}[]`** (with `~`-expand
   + single-file parent-dir create/chown), instead of stringly-typed
   `runArgs: ["-v", …]`. The canonical way to inject `~/.claude`
   (creds+skills+settings) and caches.
2. **UID/GID alignment** — default `--user $(hostuid):$(hostgid)` + an image
   build-arg convention, so bind-mounted worktree/creds are host-owned and
   writable without runtime chown.
3. **HOME convention** (e.g. `/home/agent`) so `~/.claude` mounts land where
   Claude looks.

**Claude auth specifics:**

- **API key** (simplest, headless-friendly): `env: { ANTHROPIC_API_KEY }`. Visible
  in `docker inspect` — acceptable for a sandbox.
- **Subscription/OAuth**: Claude in a Linux container reads
  `~/.claude/.credentials.json` → mount it `:ro`. **Caveat:** on a macOS host the
  token lives in the **Keychain**, not a file — needs export / a long-lived token
  / API key (sandcastle's open issue #191). Verify on the container e2e step.

**Skills:** project skills (`.claude/skills/`) travel in the worktree; personal /
global skills (`~/.claude/skills`, plugins) come from the image or a mount.

---

## 5. Alternatives considered

| Option | Container needs | Daemon change | Cross-arch | Verdict |
|---|---|---|---|---|
| **C-tcp. MCP over TCP/HTTP, `ctx.newMCPServer()`** | `claude` only | `Bun.serve` Streamable-HTTP listener + per-session bind + bearer | **fine** | **recommended (PoC-validated §3b)** |
| A (C-uds). MCP-over-socket, in-daemon | `claude` + `socat` | UDS MCP listener + per-session socket | **fine** | viable; needs a bridge tool in the image |
| B. Fat image: run `autoskd mcp` in container | `claude` + `autoskd` + `autosk` (container arch) + resolve in-container `autoskd` path | small (env override in `buildMcpConfig`) | must build/mount both binaries per arch | simplest code, heaviest image |
| E. sandcastle-style (no callback) | `claude` only | parse transit/comment off stdout in the driver | fine | rejected — needs synchronous `task` reads (decided 2026-06-19) |
| D. Bind-mount host `autoskd` 1:1 | `claude` + `autosk` | none | **breaks** (host arch ≠ container arch) | only Linux→Linux |

C-tcp is preferred: it uses Claude's first-class `http` transport (thinnest
image — no bridge tool), the per-session server solves context binding cleanly,
and it is the only transport that also serves remote/VM providers. C-uds is the
local fallback if avoiding a TCP port is required (it inherits the `0600`/`0700`
socket discipline in `rpc/uds.ts`). Option E was ruled out because feature-dev-cc
agents need synchronous `task` reads mid-step, which require a request/response
tool channel.

> **Decisions (2026-06-19):** transport = C-tcp (PoC-validated); `task` reads
> mid-step are required (so a real MCP channel is mandatory, E is out).

### How sandcastle (`~/me/dev/sandcastle`) avoids this entirely

Sandcastle is the reference for the exec seam (its `handle.exec()` model), but it
has **no host-callback tool surface**: agents run as `claude --print
--output-format stream-json … -p -` and the host only reads results. Its control
plane rides on (a) a **completion-signal sentinel** in stdout
(`<promise>COMPLETE</promise>`), (b) **structured output** (tagged XML in
stdout), (c) **git commits** on the bind-mounted branch (the deliverable), and
(d) **session JSONL copied in/out** for resume. No daemon socket is mounted, no
MCP is injected (`src/DockerLifecycle.ts` `docker run` mounts only the worktree +
user caches). That is why sandcastle never hits the MCP-in-container problem — it
has no synchronous host callback. autosk cannot fully copy this because it needs
synchronous `task` reads and a durable cross-agent `comment` channel, which is
exactly the request/response MCP the C-tcp server provides.

---

## 6. Lifecycle, failure, security

- **Socket lifetime.** Per-session: created in `run()`, unlinked on settle (and on
  detach/crash-recovery cleanup). The per-task dir is reaped with the container.
- **Permissions.** MCP socket `0600`, dir `0700` (reuse the `rpc/uds.ts`
  hardening). The bind-mounted socket is reachable only by the container user;
  document the uid/gid story (`runArgs: ["--user", …]`).
- **`socat` absence.** If the image lacks the bridge, `claude`'s MCP server fails
  to connect; surface it the same way the driver already surfaces a non-connected
  MCP server (`system/init` `mcp_servers` status warning) and document the image
  contract.
- **Abort orphan.** Unchanged from the predecessor plan (host `docker exec` kill
  is unreliable; the backstop is `release` → `docker stop`).
- **`AUTOSK_NO_SPAWN`.** With MCP daemon-hosted there is no in-container `autosk`
  to mis-auto-spawn `autoskd`, so the predecessor's optional Go hardening (§8) is
  moot for the claude path.

---

## 7. Test plan (when we build it)

- **Daemon unit:** the UDS MCP listener frames NDJSON and routes through
  `handleMessage`; the direct-store `task`/`comment` path returns the same
  `McpToolResult` shape as the shell-out path; per-session binding (author = step,
  transit gated by task-mode).
- **`claude-agent` unit:** `buildMcpConfig` emits the `socat` stdio bridge when an
  MCP socket is provided; falls back to `autoskd mcp` otherwise; tool names +
  allowlist unchanged.
- **`@autosk/docker` unit:** the per-task MCP socket dir is mounted 1:1.
- **Integration (gated on docker):** a thin image (`claude` stub + `socat`) runs a
  full `feature-dev-cc-docker` step: claude in the container reaches the daemon
  MCP over the mounted socket, calls `comment`/`task` (real store writes), and a
  `transit` is observed and committed.
- **Go:** unchanged (`WorkflowInfo.isolation` just renders `"docker"`); run
  `make test` to confirm wire/CLI/TUI/GUI untouched.

---

## 8. Decisions & remaining questions

**Resolved (2026-06-19):**

- **Transport = C-tcp** — per-session Streamable HTTP via `ctx.newMCPServer()`,
  PoC-validated (§3b). UDS (C-uds) kept only as a no-network fallback.
- **`task` reads mid-step are required** → a request/response MCP channel is
  mandatory; the sentinel-only Option E is out.
- **Binding granularity = per-session** — the server is minted per session bound
  to `{project, taskId, author=step, transit}`; the URL+token are the identity.

**Still open before implementation:**

1. **API surface:** `ctx.newMCPServer()` on the SDK `AgentRunContext` (touches
   `daemon/sdk/src/agent.ts` + `buildContext`; TS-only, no Go mirror) — confirm
   the exact return shape (`{ url, token, close() }`) and whether `close()` is
   explicit or auto-tied to settle.
2. **Reachability seam:** add `handle.hostEndpoint?(port): string` to
   `IsolationHandle` so the URL is isolation-correct, vs the engine hard-coding
   `host.docker.internal` for the docker tag.
3. **HTTP transport scope:** confirm POST→JSON-only (no SSE, no `Mcp-Session-Id`)
   is enough for our tools across Claude versions (PoC says yes on 2.1.181); keep
   it hand-rolled (no `@modelcontextprotocol/sdk`, for `bun build --compile`).
4. ~~Real-claude-in-container check~~ — **DONE (2026-06-19)**: validated e2e with a
   thin image + subscription auth (see §3b PoC #6). No blockers found.
5. **Direct-store run function:** factor `callTask`/`callComment` to target the
   daemon store directly (in-process) instead of shelling out to `autosk`, while
   keeping the shell-out path for the standalone `autoskd mcp` stdio server.
