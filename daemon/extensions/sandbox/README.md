# @autosk/sandbox

The userspace **sandbox library** for autoskd v2. Isolation is no longer an
engine/SDK concept (the `IsolationProvider` abstraction was abolished): agents
own the isolation they need by wrapping their harness with a `Sandbox` from this
package, and teardown is a normal workflow step.

This package absorbs the retired `@autosk/worktree` and `@autosk/docker`
providers. The deterministic slug / branch / container-name derivations are
byte-identical, so an already-allocated worktree/branch/container resolves to the
same place.

## The `Sandbox` shape (structural)

`Sandbox` is **structural**, not a nominal contract: agents accept any object
with these methods, so an operator can hand-roll an exotic sandbox without
depending on this package's type.

```ts
type Sandbox = {
  workspace(id): Promise<{ cwd: string }>;          // the per-task dir the harness runs in (idempotent)
  wrap(cmd, { cwd, env, id }): string[];            // wrap the harness argv (docker run …) — identity for host
  endpointFor(port): string;                        // host endpoint an in-sandbox process reaches (127.0.0.1 | host.docker.internal)
  stop(id): Promise<void>;                          // best-effort stop (agent onAbort)
  cleanup(id, { force }): Promise<{ removed; dirty; detail? }>; // terminal teardown (cleanup step)
};
```

## Factories

```ts
import { worktreeSandbox, dockerSandbox, sandboxCleanupStep } from "@autosk/sandbox";

// per-task git worktree on the host (branch autosk/<task-id>)
const sandbox = worktreeSandbox();

// run the harness inside a per-task `docker run -i --rm` container
const sandbox = dockerSandbox({ image: "my-org/autosk-runtime:latest" });
```

`dockerSandbox` emits
`docker run -i --rm --name <det> --add-host=host.docker.internal:host-gateway -v ws:ws -w ws -e … <image> <cmd…>`,
and `endpointFor` rewrites the host MCP URL to `host.docker.internal` so the
in-container harness reaches the host. The image is **thin**: just the harness
(`claude`/`pi`, authenticated) plus the build/test toolchain — no `socat`, no
`autosk`/`autoskd`, no mounted daemon socket (the agent's tool surface is the
per-session host HTTP MCP server reached over `host.docker.internal`). Inject
credentials / caches via `mounts`, align ownership via `user`, and set the
container `home`.

## Cleanup as a workflow step

`done`/`cancel` are now a raw status flip with **no** engine teardown — so a
workflow that allocates a sandbox MUST route its terminals through a cleanup
step or it leaks the worktree on every task:

```ts
steps: {
  dev: claudeAgent({ sandbox, firstMessage }),
  // …
  accept: statusStep("human"),
  cleanup: sandboxCleanupStep(sandbox),
},
onTransit(ctx, to) { /* route accept → cleanup, cleanup → done */ },
```

`sandboxCleanupStep` removes the worktree dir (branch preserved) and any stray
container, comments the outcome, and transits to its target (default `done`). It
is idempotent on a missing env.
