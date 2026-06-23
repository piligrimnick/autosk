# Docker isolation provider (`dockerIsolation()`) — implementation plan

**Status:** plan (not yet started).
**Date:** 2026-06-18.
**Owners:** autosk core.
**Predecessor:** status-driven isolation lifecycle
([`20260615-Isolation-Lifecycle-StatusDriven.md`](20260615-Isolation-Lifecycle-StatusDriven.md))
— shipped `acquire` / `release` / `reap` and explicitly deferred the *execution
seam* and the `dockerIsolation()` provider to a separate plan (its §11). This is
that plan.
**Related code:**
`daemon/sdk/src/workflow.ts` (the `IsolationHandle` / `IsolationProvider` contract),
`daemon/sdk/src/agent.ts` (`ExecOptions` / `ExecResult` / `SpawnOptions` / `ChildHandle`),
`daemon/core/src/engine/child.ts` (the host `execOneShot` / `spawnChild` plumbing),
`daemon/core/src/engine/session.ts` (`buildContext` wires `ctx.exec` / `ctx.spawn`),
`daemon/extensions/worktree/src/index.ts` (the FS provider we compose with),
`daemon/extensions/pi-agent/src/index.ts` (`ctx.spawn(["pi", …], {cwd, env})` + `autoskEnv`),
`internal/daemon/rpcclient/{client,connector}.go` (socket resolution + auto-spawn),
`docs/workflows.md`, `docs/extensions.md`, `CHANGELOG.md`.

---

## 1. Motivation

The shipped `worktreeIsolation()` gives each task its own git worktree and runs
the agent there — but the agent (and every command it spawns) still runs on the
**host**. `pi-agent` does `ctx.spawn(["pi", "--mode", "rpc", …], { cwd: ctx.cwd })`
and `ctx.spawn` is `Bun.spawn(cmd, { cwd })` on the daemon host
(`daemon/core/src/engine/child.ts`). `IsolationHandle` carries only a `cwd`
string, so there is **no way to make the agent run inside a container**.

We want a `dockerIsolation()` provider where **pi and all of its tool/shell
commands execute inside a per-task Docker container** (a real sandbox), while the
git branch / review-merge story from worktree isolation is preserved.

This needs three layers of work, in order:

1. **An execution seam in the SDK** — `IsolationHandle` gains optional
   `exec` / `spawn` so a provider can *own process creation* and run commands
   inside its environment.
2. **Engine wiring** — `ctx.exec` / `ctx.spawn` route through the handle seam
   when present, else fall back to the host spawn.
3. **The `dockerIsolation()` provider** — a new `@autosk/docker` extension that
   composes `worktreeIsolation()` for the filesystem and adds the container
   lifecycle + the `docker exec` seam on top.

---

## 2. Locked decisions

From the design interview; do not relitigate these.

| Topic | Choice | Rationale |
|---|---|---|
| Execution model | **Full sandbox** — pi runs *inside* the container (`docker exec`) | the only model that actually isolates the agent's process tree |
| Seam shape | **`handle.exec` / `handle.spawn`** (provider owns the process) | most general (extends to remote/VM later); sandcastle's `handle.exec()` model |
| Avoid duplication | **Extract** the generic Bun stdio/abort plumbing into a shared module both core and the provider import | the seam owner must not reimplement `LineDispatcher` / abort wiring |
| Workspace | **bind-mount a per-task git worktree** (compose with `@autosk/worktree`) | git-branch isolation *and* container process isolation; edits land on `autosk/<id>` on the host, review/merge unchanged |
| Image | **operator-supplied image with pi preinstalled** (`image` option, required) | the provider just `docker run`s it; no build step |
| Daemon access | **bind-mount the daemon UDS into the container** | full `@autosk/pi-tools` parity (`autosk_comment` / `autosk_task` / transit) inside the sandbox |

---

## 3. Architecture overview

```
            ┌───────────────────────── daemon host ──────────────────────────┐
            │  autoskd (engine)                                               │
            │   session.run(): acquire() ─────────────► dockerIsolation       │
            │                                            ├─ worktreeIsolation  │
            │   ctx.spawn(["pi",…],{cwd,env}) ──┐         │   (host worktree)  │
            │                                   │         └─ docker run -d …   │
            │   handle.spawn rewrites to:       ▼                             │
            │     docker exec -i -w <wt> -e …  <container>  pi --mode rpc      │
            │            │ stdio (JSON-lines) piped back via the host client   │
            └────────────┼────────────────────────────────────────────────────┘
                         ▼
            ┌──────────── container (operator image: pi + autosk) ───────────┐
            │  cwd = <same host worktree path> (bind-mounted 1:1)             │
            │  pi --mode rpc  ──tools──►  autosk … (UDS bind-mounted) ─────────┼──► back to autoskd
            └─────────────────────────────────────────────────────────────────┘
```

Key tricks:

- **Identical-path bind mount.** The worktree's host path
  (`~/.autosk/worktrees/<slug>/<task>`) is bind-mounted into the container at the
  *same absolute path*, so `ctx.cwd` is a valid `-w` workdir inside the container
  with **zero path translation** in the seam.
- **The agent stays isolation-agnostic.** `pi-agent` keeps calling
  `ctx.spawn(["pi", …], { cwd, env })`; the engine transparently routes it
  through `handle.spawn` → `docker exec`. No `pi-agent` change is required beyond
  ensuring its env is forwarded into the container (it already is, via
  `opts.env`).

---

## 4. Layer 1 — SDK contract (`daemon/sdk`)

### 4.1 The execution seam on `IsolationHandle` (`daemon/sdk/src/workflow.ts`)

```ts
import type { ExecOptions, ExecResult, SpawnOptions, ChildHandle } from "./agent.ts";

/** Options the engine hands a handle's exec seam: the agent's ExecOptions with
 *  cwd + signal already resolved (handle.cwd, or an opts override). */
export interface IsolationExecOptions extends ExecOptions {
  cwd: string;
  signal: AbortSignal;
}
export interface IsolationSpawnOptions extends SpawnOptions {
  cwd: string;
  signal: AbortSignal;
}

export interface IsolationHandle {
  cwd: string;
  meta?: Record<string, unknown>;

  /**
   * Optional: run a one-shot command INSIDE the environment (e.g. `docker exec`).
   * When present the engine routes `ctx.exec` through it; when absent the engine
   * runs the command on the host at `cwd` (today's behaviour). MUST honour
   * `opts.signal` (abort/shutdown) and return the same {@link ExecResult} shape.
   */
  exec?(cmd: string[], opts: IsolationExecOptions): Promise<ExecResult>;

  /**
   * Optional: spawn a long-lived command INSIDE the environment. When present
   * the engine routes `ctx.spawn` through it (this is how `pi --mode rpc` ends up
   * inside the container). MUST stream line-buffered stdio and kill on
   * `opts.signal`, returning the same {@link ChildHandle} shape.
   */
  spawn?(cmd: string[], opts: IsolationSpawnOptions): ChildHandle;
}
```

`IsolationProvider` (`tag` / `acquire` / `release?` / `reap?`) is **unchanged** —
the seam lives on the *handle* that `acquire` returns, so a provider opts in by
returning a handle with `exec` / `spawn`. `worktreeIsolation()` returns a handle
*without* them → host spawn, exactly as today.

### 4.2 Shared process plumbing (`daemon/sdk/src/process.ts`, new)

Move the generic, pi-free Bun process helpers out of
`daemon/core/src/engine/child.ts` into the SDK so **both** the engine and the
out-of-tree `@autosk/docker` provider import one implementation (the
no-duplication requirement). New exports from `@autosk/sdk`:

```ts
// generic, cwd/env/signal-parameterised; no engine "defaults" injection.
export function runChild(cmd: string[], opts: {
  cwd?: string; env?: Record<string, string>;
  input?: string | Uint8Array; signal: AbortSignal; timeoutMs?: number;
}): Promise<ExecResult>;

export function spawnChild(cmd: string[], opts: {
  cwd?: string; env?: Record<string, string>; signal: AbortSignal;
}): ChildHandle;
```

This carries the `LineDispatcher`, `readLines`, abort-wiring and `mergedEnv`
logic verbatim from `child.ts`. (`@autosk/sdk` already ships runtime helpers —
`statusStep`, ids — and targets Bun, so a `Bun.spawn`-based module is in keeping;
it is runtime-only, never a wire type, so the Go mirror is untouched.)

> **Variant:** if we'd rather keep `@autosk/sdk` free of process spawning, put
> these in a tiny new `@autosk/proc` package depended on by core + the provider.
> Recommended default: `@autosk/sdk/process` (one fewer package).

`daemon/core/src/engine/child.ts` becomes a thin adapter over the SDK helpers
(keeping its `ExecDefaults` / `SpawnDefaults` injection so `buildContext` is
unchanged), or is deleted with `buildContext` calling the SDK helpers directly
(see §5).

---

## 5. Layer 2 — engine wiring (`daemon/core/src/engine/session.ts`)

`acquire` already runs in the worker and sets `this.isolation` (the handle) +
`this.cwd`. Only `buildContext`'s `base.exec` / `base.spawn` change to prefer the
handle seam:

```ts
exec: (cmd: string[], opts?: ExecOptions) => {
  const signal = opts?.signal ?? this.controller.signal;
  const cwd = opts?.cwd ?? this.cwd;
  return this.isolation?.exec
    ? this.isolation.exec(cmd, { ...opts, cwd, signal })
    : runChild(cmd, { ...opts, cwd, signal });
},
spawn: (cmd: string[], opts?: SpawnOptions) => {
  const cwd = opts?.cwd ?? this.cwd;
  const signal = this.controller.signal;
  return this.isolation?.spawn
    ? this.isolation.spawn(cmd, { ...opts, cwd, signal })
    : spawnChild(cmd, { ...opts, cwd, signal });
},
```

Notes:
- Interactive (taskless) sessions have no workflow → no isolation → always the
  host path. No change there.
- Abort/shutdown still fires `this.controller.signal`; the seam contract requires
  the provider to honour it (see the `docker exec` orphan caveat in §10).
- `release` / `reap` lifecycle (§5.2 of the predecessor plan) is untouched: the
  engine calls `release(handle)` on leaving `work` and `reap(identity,{force:true})`
  on terminal. `dockerIsolation` implements both (unlike worktree).

---

## 6. Layer 3 — the `dockerIsolation()` provider (`daemon/extensions/docker`, new `@autosk/docker`)

Package layout mirrors `@autosk/worktree` exactly (root `index.ts` re-export shim
for the compiled-daemon resolver, sources under `src/`, `package.json` with
`"autosk-extension"` keyword + `@autosk/sdk` and `@autosk/worktree` deps,
`README.md`, `test/`).

### 6.1 Options

```ts
export interface DockerIsolationOptions {
  /** Operator image with `pi` (and a compatible `autosk`) preinstalled. Required. */
  image: string;
  /** docker binary; defaults to "docker" (honours podman/nerdctl via this knob). */
  dockerBin?: string;
  /** Filesystem provider to compose; defaults to worktreeIsolation(). */
  inner?: IsolationProvider;
  /** Bind-mount the daemon UDS into the container (default true). */
  mountSocket?: boolean;
  /** Daemon UDS path; default $AUTOSK_SOCK → <home>/.autosk/daemon.sock. */
  socketPath?: string;
  /** Optional host `autosk` binary to bind-mount (see cross-arch note §9). */
  autoskBin?: string;
  /** Extra `docker run` args (e.g. --network, --cpus, --memory, --user). */
  runArgs?: string[];
  /** Extra container env baked at `docker run` (inherited by every `docker exec`). */
  env?: Record<string, string>;
  /** Forwarded to the inner worktree provider (test injection). */
  home?: string;
}
```

### 6.2 Deterministic identity

Container name derived from the *same* `(projectRoot, taskId)` identity the inner
worktree uses (reuse `@autosk/worktree`'s exported `slugFor`):

```ts
const containerName = `autosk-${slugFor(canonRoot(projectRoot))}-${taskId}`;
```

Deterministic so `acquire` (re-use/restart), `reap` (by identity, session-free),
and crash recovery all resolve the same container with no in-memory state.

### 6.3 `acquire` — ensure-ready (create | start | reuse)

1. `const inner = opts.inner ?? worktreeIsolation({ home })`;
   `const wt = await inner.acquire({ projectRoot, taskId })` → host worktree path
   `wt.cwd`.
2. `await ensureDockerAvailable(dockerBin)` (cache a successful `docker version`,
   mirroring worktree's `ensureGitAvailable`).
3. `docker inspect <containerName>`:
   - **running** → reuse;
   - **exists, stopped** (DORMANT) → `docker start <containerName>`;
   - **absent** → `docker run -d --name <containerName>`:
     - `-v <wt.cwd>:<wt.cwd>` (identical-path mount),
     - if `mountSocket`: `-v <socketPath>:<socketPath>` and
       `-e AUTOSK_SOCK=<socketPath>` (so the in-container `autosk` connects to the
       host daemon),
     - optional `-v <autoskBin>:/usr/local/bin/autosk:ro` (§9),
     - `opts.env` as `-e`, `opts.runArgs`,
     - the image, plus a keep-alive entrypoint (`sleep infinity`) so the container
       stays hot across steps.
4. Return:
   ```ts
   return {
     cwd: wt.cwd,
     meta: { ...wt.meta, container: containerName },
     exec: (cmd, o) => runChild(dockerExec(containerName, cmd, o), { signal: o.signal }),
     spawn: (cmd, o) => spawnChild(dockerExec(containerName, cmd, o), { signal: o.signal }),
   };
   ```

`dockerExec` rewrites the command:

```ts
function dockerExec(name, cmd, o) {
  const envFlags = Object.entries(o.env ?? {}).flatMap(([k, v]) => ["-e", `${k}=${v}`]);
  return [dockerBin, "exec", "-i", "-w", o.cwd, ...envFlags, name, ...cmd];
}
```

`o.env` carries `pi-agent`'s `AUTOSK_CWD` (= host `projectRoot`, valid daemon-side)
and `AUTOSK_AGENT`, so the in-container `autosk` targets the right project and
attributes comments correctly — exactly as on the host today.

### 6.4 `release` — quiesce-on-exit (NEW; worktree omits this)

`docker stop <containerName>` (keep the container → DORMANT, cheap to
`docker start` on resume). Tolerate "already stopped / gone". The inner worktree
provider has no `release`, so nothing else to quiesce.

### 6.5 `reap` — destroy-on-terminal (by identity)

1. Recompute `containerName` + the inner worktree path from identity.
2. **Dirty gate** delegates to the worktree notion of dirty: call
   `inner.reap({projectRoot, taskId}, { force })`.
   - If it returns `{ dirty: true, removed: false }` (force=false) → return the
     same; **leave the container in place** too (so the operator can inspect),
     surfaced as `ENVIRONMENT_DIRTY` by the manual terminal path.
   - Else (clean, or force) the worktree is removed (branch preserved); proceed.
3. `docker rm -f <containerName>` (tolerate "already gone").
4. Return the inner `IsolationReapResult` (`removed` reflects container+worktree
   teardown; the branch is always preserved).

Idempotent and recovery-safe: a vanished container or worktree is a no-op.

---

## 7. pi-agent — what changes (almost nothing)

`pi-agent` already spawns `ctx.spawn(["pi", "--mode", "rpc", …], { cwd: ctx.cwd, env: autoskEnv(ctx) })`
and `autoskEnv` returns `{ AUTOSK_CWD: ctx.projectRoot, AUTOSK_AGENT: <step> }`.
Under docker isolation the engine routes this through `handle.spawn`, which
forwards `opts.env` as `docker exec -e …`. So:

- pi runs inside the container (the image's `pi` on `PATH`; the default `piBin`
  `"pi"` is correct — an absolute *host* `AUTOSK_PI_BIN` path would NOT resolve in
  the container, document that constraint).
- The `autosk_transit` tool keeps working because it is observed on pi's stdout,
  which is piped back through `docker exec` regardless of where pi runs.
- `autosk_comment` / `autosk_task` work because the in-container `autosk` reaches
  the mounted UDS with `AUTOSK_SOCK` + `AUTOSK_CWD` set.

No code change to `pi-agent` is expected; add a test asserting `autoskEnv` is
forwarded through the seam.

---

## 8. Daemon access from inside the container

- **Socket:** resolve `socketPath` (`opts.socketPath ?? $AUTOSK_SOCK ?? <home>/.autosk/daemon.sock`),
  bind-mount it 1:1, set container `AUTOSK_SOCK` to it. The host daemon owns
  `.autosk/`; the in-container `autosk` is a pure RPC client that sends
  `AUTOSK_CWD` (a host path) over the wire, so the **project tree need not be
  mounted** — only the socket.
- **Auto-spawn hazard (optional Go hardening).** If the socket were briefly
  unreachable the in-container `autosk` would try to *auto-spawn* `autoskd serve`
  (`internal/daemon/rpcclient/connector.go`) — wrong inside a container. There is
  a `NoAutoSpawn` option but no env switch today. Optional, low-risk hardening:
  honour `AUTOSK_NO_SPAWN` in `resolveSock`/client construction and set it in the
  container env so the in-container CLI is strictly connect-only. Not required for
  the happy path (the daemon running the session is up and the socket is mounted).

---

## 9. Getting a runnable `autosk` into the container

The in-container `autosk` must match the **container** OS/arch (a macOS host
binary will not run in a Linux container). Two supported routes:

1. **Image ships `autosk`** (recommended; pairs with the chosen "image has pi"
   decision). Default: mount only the socket; `autosk` comes from the image.
2. **Bind-mount a compatible build** via `opts.autoskBin` (e.g. a linux/amd64
   `autosk`) → `-v <bin>:/usr/local/bin/autosk:ro`. For same-OS hosts (Linux
   daemon, Linux container) the host binary works directly.

Default: `mountSocket: true`, no autosk mount; document that the operator image
should provide a compatible `autosk` alongside `pi`.

---

## 10. Idempotency, failure, recovery, caveats

- **`acquire` idempotent / recovery-safe** (load-bearing): deterministic
  container name → `docker inspect` decides create/start/reuse. After a daemon
  crash the in-memory handle is gone but the container survives (no `release`
  ran); the next dispatch re-acquires it.
- **Terminal = release then reap:** `docker stop` then `docker rm -f`; `reap`
  tolerates "already stopped/gone".
- **`docker exec` orphan caveat.** Killing the *host* `docker exec` client on
  abort does not reliably kill the in-container process. Bounded mitigation: an
  abort parks the task → `release` → `docker stop`, which kills any orphan. For
  intra-step aborts, enhance `handle.spawn`'s `kill` to also `docker stop`/`docker
  exec … kill` the container's pi. Flag as a known limitation; container teardown
  is the backstop.
- **Permissions.** The bind-mounted worktree must be writable by the container
  user; expose `--user` / `runArgs` and document the uid/gid story.
- **Failure wrapping.** As with worktree, `acquire`/`reap` throw plain
  descriptive messages; the engine wraps them as
  `isolation_acquire_failed:` / `isolation_reap_failed:` and parks to `human`.

---

## 11. Test plan

### Daemon (`bun test` / `bun run typecheck`)

- **`@autosk/sdk` `process.test.ts`** — port the existing `child.ts`-level
  expectations (line buffering, abort kills the child, timeout, env merge) onto
  the moved `runChild` / `spawnChild`.
- **engine seam routing** (`daemon/core/test/`) — a recording fake handle
  exposing `exec`/`spawn`: assert `ctx.exec`/`ctx.spawn` route through the handle
  when present and fall back to the host helpers when absent; assert the resolved
  `cwd` + `signal` are passed.
- **`@autosk/docker` unit tests** (no docker needed) — `containerName` derivation
  is deterministic + matches the inner worktree slug; `dockerExec` command rewrite
  (workdir `-w`, `-e` env flags, `-i`).
- **`@autosk/docker` integration** (gated on a `dockerOk` `docker version`
  probe, like worktree's `gitOk`) — `acquire` runs/reuses/starts a container;
  `exec`/`spawn` actually run a command *inside* it (`pwd` == worktree path; a
  `-e` var is visible); `release` stops it; `reap` removes container + worktree
  and preserves the branch; dirty-refuse vs `force`.
- **Composition** — `dockerIsolation` delegates the FS lifecycle to
  `worktreeIsolation` (branch created on `acquire`, worktree gone + branch kept on
  `reap`).

### Go

No Go change unless we adopt the optional `AUTOSK_NO_SPAWN` hardening (§8); then a
small `connector`/`client` test. Run `make test` to confirm the wire/CLI/TUI/GUI
are untouched (the seam is daemon-internal; `WorkflowInfo.isolation` just renders
the new `"docker"` tag).

---

## 12. Docs & changelog

- **`docs/workflows.md`** — document the optional `exec` / `spawn` seam on
  `IsolationHandle` and add `dockerIsolation()` alongside `worktreeIsolation()`.
- **`docs/extensions.md`** — add `@autosk/docker` to the extensions list as an
  **opt-in** provider (NOT bootstrapped by default; the operator adds it with
  `autosk ext add npm:@autosk/docker` and attaches `dockerIsolation({ image })`
  to a workflow).
- **`daemon/extensions/docker/README.md`** — new (usage, image requirements,
  socket mount, security/permissions notes).
- **`CHANGELOG.md`** — user-visible: `### Added` a Docker isolation provider +
  the `IsolationHandle` exec/spawn seam (extension-author surface).

---

## 13. Out of scope / follow-ups

- **`copy-volume` workspace** (chosen `bind-worktree` instead) — fully isolated FS
  with an extraction/diff step; a later option.
- **Build-from-Dockerfile image** (chosen operator-image) — a later `image`
  alternative.
- **Orphan container sweep (`pruneStale`)** — periodic/at-acquire removal of
  containers for tasks that no longer exist (defence-in-depth); mirrors
  sandcastle's `WorktreeManager.pruneStale`.
- **Remote / VM providers** — the same handle seam supports them; out of scope
  here but the reason we chose `handle.exec/spawn` over a host-side command prefix.
- **Windows containers** — not targeted.

---

## 14. Implementation checklist

1. `daemon/sdk/src/process.ts` (new): `runChild` / `spawnChild` (moved from
   `child.ts`); export from `@autosk/sdk` index. Port `child.ts` to re-use them.
2. `daemon/sdk/src/workflow.ts`: add `IsolationExecOptions` / `IsolationSpawnOptions`
   and optional `exec` / `spawn` on `IsolationHandle`; import process types from
   `agent.ts`.
3. `daemon/core/src/engine/session.ts`: route `buildContext` `exec`/`spawn` through
   `this.isolation?.exec` / `?.spawn` with host fallback (§5).
4. `daemon/extensions/docker/` (new `@autosk/docker`): `dockerIsolation()` with
   `acquire` (run/start/reuse), `release` (stop), `reap` (rm + inner reap), the
   `docker exec` seam, deterministic naming, `ensureDockerAvailable`.
5. (optional) Go: `AUTOSK_NO_SPAWN` connect-only switch + set it in the container
   env (§8).
6. Tests (§11): SDK process tests, engine seam-routing, docker unit + gated
   integration, composition.
7. `bun run typecheck` + `bun test` green; `make test` green (Go untouched, or +1
   test if the hardening lands).
8. Docs (§12) + `CHANGELOG.md` entry.
```