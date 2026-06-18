# @autosk/docker

The **opt-in** isolation provider for autoskd v2: run pi **and every command it
spawns** inside a per-task **Docker container** — a real sandbox — while keeping
the git-branch / review-merge story from worktree isolation. It implements the
v2 [`IsolationProvider`](../../sdk/src/workflow.ts) contract on top of the
[execution seam](../../../docs/workflows.md#the-execution-seam) and
**composes [`@autosk/worktree`](../worktree/README.md)** for the filesystem
(design `docs/plans/20260618-Docker-Isolation.md`).

Unlike `worktreeIsolation()` — where the agent runs on the **host** in a git
worktree — `dockerIsolation()` runs the agent's whole process tree **inside a
container**: `pi-agent`'s `ctx.spawn(["pi", "--mode", "rpc", …])` is transparently
rewritten to `docker exec … <container> pi --mode rpc`. The agent code is
unchanged; the engine routes `ctx.exec` / `ctx.spawn` through the handle's
`exec` / `spawn` seam.

> **Not bootstrapped by default.** This package is not part of the first-run
> bootstrap (unlike `@autosk/worktree` / `@autosk/pi-agent` / `@autosk/feature-dev`).
> An operator opts in explicitly — `autosk ext add npm:@autosk/docker` — and
> attaches `dockerIsolation({ image })` to a workflow.

## Usage

```ts
import { statusStep } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { dockerIsolation } from "@autosk/docker";

autosk.registerWorkflow({
  name: "sandboxed-dev",
  firstStep: "dev",
  // Agents are inline step values (the step key is the agent name).
  steps: { dev: piAgent({ firstMessageFile: ".../dev.md" }), accept: statusStep("human") },
  // The ONLY required option is the operator image (pi + a compatible autosk).
  isolation: dockerIsolation({ image: "my-org/autosk-runtime:latest" }),
});
```

`docker` (or `podman`/`nerdctl` via `dockerBin`) must be on `PATH` and its daemon
reachable; the project root must be a git repo (the inner worktree provider's
requirement).

## The operator image

The provider does **not** build anything — it just `docker run`s the image you
pass. That image is the contract; it **must** ship:

- **`pi` on `PATH`.** `pi-agent`'s default `piBin` is `"pi"`, resolved **inside
  the container**. An absolute *host* path (`AUTOSK_PI_BIN=/Users/me/.../pi`)
  will **not** resolve in the container — leave `piBin` as `"pi"`.
- **A container-compatible `autosk` on `PATH`** (so the `@autosk/pi-tools` tools
  `autosk_comment` / `autosk_task` work from inside the sandbox). It must match
  the **container** OS/arch, not the host's — a macOS host `autosk` will not run
  in a Linux container. If your image can't ship one, bind-mount a compatible
  build with the [`autoskBin`](#configuration) option.
- The provider supplies its own keep-alive entrypoint (`tail -f /dev/null`), so
  the image's own `CMD`/`ENTRYPOINT` is irrelevant — it only has to keep
  `tail`/`sh` available. `acquire` verifies the container actually stayed running
  and fails fast (`docker run …: container is not running …`, with the container
  logs) if PID 1 exited immediately.

## Behaviour

Reuses the **same `(projectRoot, taskId)` identity** as the inner worktree, so
the container name is deterministic — `acquire` (reuse/restart), `reap` (by
identity), and crash recovery all resolve the same container with no in-memory
state:

```text
container = autosk-<slug>-<task-id>     # <slug> is @autosk/worktree's slugFor(canonRoot)
workspace = the inner worktree, bind-mounted 1:1   (-v <wt.cwd>:<wt.cwd>)
branch    = autosk/<task-id>            # owned by the inner worktree provider
```

The container moves through the isolation state machine (the worktree FS state
machine plus container moves):

- **acquire** (ensure-ready) — `inner.acquire` allocates the per-task git
  worktree on `autosk/<task-id>`, then `docker inspect` decides: **running** →
  reuse; **stopped** (DORMANT) → `docker start`; **absent** → `docker run -d`
  with the worktree bind-mounted at the **same absolute path** (so `ctx.cwd` is a
  valid `-w` workdir with zero path translation), the daemon UDS mounted (see
  below), and the keep-alive entrypoint. Idempotent + recovery-safe. Returns a
  handle whose `exec`/`spawn` run **inside** the container.
- **release** (quiesce-on-exit) — `docker stop` (keep the container → DORMANT,
  cheap to `docker start` on resume). Fires only when the task **leaves `work`**
  (a `human` park, or a `done`/`cancel` terminal); never on step→step. Tolerates
  "already stopped / gone".
- **reap** (destroy-on-terminal, `done`/`cancel`) — keyed by `(projectRoot,
  taskId)`, so it works with no live handle. The dirty gate delegates to the
  inner worktree reap: with `force:false` a dirty worktree is **refused**
  (`{ removed:false, dirty:true }`) and the container is **left in place** too so
  you can inspect it; otherwise the worktree is removed (branch **preserved**)
  and `docker rm -f` destroys the container. Idempotent / recovery-safe (a
  vanished container or worktree is a no-op).

### The exec / spawn seam

The handle's `exec` / `spawn` rewrite the argv to
`docker exec -i -w <cwd> -e <env…> <container> <cmd…>` and run it through the
shared [`runChild` / `spawnChild`](../../sdk/src/process.ts) helpers from
`@autosk/sdk` (the same plumbing the host path uses — no duplicated stdio/abort
wiring). The seam honours the **same `ExecOptions` fields the host path does**:

- **`input`** — `docker exec -i` pipes stdin, so the bytes flow host client →
  in-container process (e.g. a `git apply` patch on stdin).
- **`timeoutMs`** — kills the host `docker exec` client and resolves the same
  non-zero `ExecResult` the host path returns. (Timeouts SIGKILL the client
  because the `docker exec` CLI traps SIGTERM and would otherwise exit 0; the
  in-container process may orphan — see the caveat below.)
- **`env`** — each entry becomes a `-e KEY=VALUE`. This carries pi-agent's
  `AUTOSK_CWD` (the **host** project root, valid daemon-side over the wire) and
  `AUTOSK_AGENT` (the step name), so the in-container `autosk` targets the right
  project and attributes comments correctly — exactly as on the host today.

## Daemon access from inside the container

The host daemon owns `.autosk/`; the in-container `autosk` is a pure RPC client.
By default (`mountSocket: true`) the provider bind-mounts the daemon's Unix
socket **1:1** and sets `AUTOSK_SOCK` in the container, so `autosk_transit`
(observed on pi's stdout, piped back through `docker exec`) and
`autosk_comment` / `autosk_task` (over the mounted socket) all work from inside
the sandbox. The **project tree itself need not be mounted** — only the worktree
(for edits) and the socket (for RPC); `autosk` sends `AUTOSK_CWD` (a host path)
over the wire.

## Configuration

`dockerIsolation(options)`:

| Option        | Default                                   | Description                                                                                  |
| ------------- | ----------------------------------------- | -------------------------------------------------------------------------------------------- |
| `image`       | **required**                              | Operator image with `pi` (and a container-compatible `autosk`) preinstalled.                 |
| `dockerBin`   | `"docker"`                                | Container CLI to shell out to (honours `podman` / `nerdctl`).                                 |
| `inner`       | `worktreeIsolation({ home, gitBin })`     | The filesystem `IsolationProvider` to compose for the per-task workspace.                     |
| `mountSocket` | `true`                                    | Bind-mount the daemon UDS into the container and set `AUTOSK_SOCK`.                           |
| `socketPath`  | `$AUTOSK_SOCK` → `<home>/.autosk/daemon.sock` | Daemon UDS path to mount.                                                                 |
| `autoskBin`   | _(none)_                                  | Host `autosk` binary to bind-mount at `/usr/local/bin/autosk:ro` (cross-arch escape hatch).   |
| `runArgs`     | `[]`                                      | Extra `docker run` args (e.g. `--network`, `--cpus`, `--memory`, `--user`).                   |
| `env`         | `{}`                                      | Extra container env baked at `docker run` (inherited by every `docker exec`).                 |
| `home`        | `process.env.HOME`                        | Forwarded to the default inner `worktreeIsolation` (tests inject a temp home).               |
| `gitBin`      | `"git"`                                   | Forwarded to the default inner `worktreeIsolation`.                                          |

### Getting a runnable `autosk` into the container

The in-container `autosk` must match the **container** OS/arch. Two routes:

1. **Image ships `autosk`** (recommended; pairs with "image has pi"). The
   default — mount only the socket, `autosk` comes from the image.
2. **Bind-mount a compatible build** via `autoskBin` (e.g. a `linux/amd64`
   `autosk`) → `-v <bin>:/usr/local/bin/autosk:ro`. For same-OS hosts (Linux
   daemon, Linux container) the host binary works directly.

### Permissions

The bind-mounted worktree must be writable by the container user. Set the uid/gid
via `runArgs: ["--user", "1000:1000"]` (or build the image with a matching user).

## Caveats

- **`docker exec` orphan on abort.** Killing the *host* `docker exec` client on
  abort does not reliably kill the in-container process. The backstop is the
  lifecycle: an abort parks the task → `release` → `docker stop`, which tears down
  any orphan. (For the same reason the seam's TIMEOUT path SIGKILLs the client
  while the ABORT path SIGTERMs it cooperatively.)
- **Failure wrapping.** Like the worktree provider, `acquire`/`release`/`reap`
  throw plain descriptive messages; the engine wraps them
  (`isolation_acquire_failed:` / `isolation_reap_failed:`) and parks the task to
  `human`.

## Exports

- `dockerIsolation(options)` → `IsolationProvider`
- `containerName(projectRoot, taskId)`, `dockerExecArgv(...)`, `runArgsFor(...)`,
  `resolveSocketPath(...)` — the deterministic derivation / argv helpers
  (exported for tooling / tests).
- `DOCKER_TAG` — the provider tag (`"docker"`) rendered by `workflow.get`.
