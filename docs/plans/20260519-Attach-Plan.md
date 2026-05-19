# autosk attach — read/write mirror of a running agent session

> **⚠️ Superseded by [v2](20260519-Attach-Plan-v2.md).**
>
> This is the original v1 plan. It locks in Design 2: a locally-
> launched `pi --no-session --no-builtin-tools -e <ext>` that runs a
> new TypeScript extension (`extension-attach/`, package name
> `pi-autosk-attach`). That client was built and worked, but during
> review we pivoted to an in-process Go Bubble Tea TUI for the
> reasons listed in [§1 of v2](20260519-Attach-Plan-v2.md#1-why-we-pivoted).
>
> The **server-side decisions** in §3–§4 of this document
> (`pirunners.Registry`, `Attachments`, the SSE `?attach=true` flag,
> `POST /v1/jobs/{id}/input` and `/abort`, the executor's pause-on-
> attach behaviour) shipped as-described. Only the **client surface**
> (§5 Extension and §6 CLI of this document) was replaced.
>
> The retired `extension-attach/` directory still exists in the tree
> pending cleanup task `as-a3c2`. Treat anything in §2, §5 and §6
> below as historical.

---

**Date:** 2026-05-19
**Status:** Superseded — see [v2](20260519-Attach-Plan-v2.md).
**Predecessors:** [`20260518-Daemon-UDS-Plan.md`](20260518-Daemon-UDS-Plan.md),
[`20260518-Agent-Packages.md`](20260518-Agent-Packages.md).
**Out of scope (future iterations):** handover / takeover to a local
interactive pi (Design 1's "pi-as-frontend via custom provider"),
multi-host attach, web UI client.

---

## 1. Purpose

Let a human operator open a window that looks and behaves like a
normal `pi` session while an agent is being driven by the daemon
inside that very session — **without** ever touching the live
`session.jsonl` and without stopping the agent.

User contract:

```bash
autosk attach <job-id>
# … the operator sees the running pi session, can type prompts,
#    abort, etc. Close the window to detach. Workflow resumes
#    when no clients are attached.
```

While at least one client is attached:

- The daemon's correction-loop **does not** send corrective prompts.
- The daemon's per-turn idle-timeout is suspended.
- All other workflow plumbing (the executor turn loop, `step_signals`
  consumption, `MarkDone` on transition) continues unchanged.

The implementation is **Design 2** from the design discussion: the
client is a `pi` instance running our extension that renders a live
projection of the daemon's existing SSE stream and forwards user
input back over the existing unix socket. No second pi-process owns
the session file. No changes inside the `pi` binary are required.

---

## 2. Locked decisions

| Topic | Decision |
|---|---|
| Mirror is **read/write** | Operator can type prompts and abort. No takeover semantics in v1. |
| Correction-loop while attached | **Paused** (W1). No corrective prompts sent; idle-timeout disabled. |
| Multi-client model | Symmetric. Every connection can read SSE and POST input/abort. Concurrent writers are serialised by the existing `runner.encMu`; pi RPC's `prompt`/`steer`/`follow_up` semantics already queue them between turns. No reader/writer distinction, no token. |
| Attach lifecycle | An attach **is** an open SSE stream opened with `?attach=true`. Connection close (detected via `r.Context().Done()`) is detach. No separate attach/heartbeat/detach endpoints. |
| Distribution of the extension | Sibling npm package next to `extension/` (the existing `pi-autosk` tool). New dir `extension-attach/`, package name `pi-autosk-attach`. Bundled and resolved at attach-time. |
| Client renderer | Custom — `pi.sendMessage` + `pi.registerMessageRenderer("autosk-remote", ...)`. Visual fidelity target ≈ 85% of native pi rendering. No tool execution happens locally. |
| Input boundary | Extension intercepts `pi.on("input", …)` and returns `{action: "handled"}`. User text never reaches the local pi's agent loop. |
| Session lifetime in the client | `pi --no-session --no-tools` — ephemeral. Local pi never writes a session file. |
| Storage | None on disk. Attach state is an in-memory counter inside the daemon (`pirunners.Registry` or a small sibling), reset to zero on daemon start (consistent with the existing `daemon_restart` recovery that rewrites stale `running` rows). |

---

## 3. Architecture

```
                          ~/.autosk/daemon.sock
                                  │
   ┌──────────── HTTP/UDS ────────┴────────────┐
   │                                            │
GET /v1/jobs/{id}/stream  ──── SSE ─────────►  client (autosk attach):
POST /v1/jobs/{id}/input  ◄── prompt/steer ──   pi --no-session --no-tools \
POST /v1/jobs/{id}/abort  ◄── abort ────────    -e <ext> --autosk-job <id> \
POST /v1/jobs/{id}/attach ◄── enter / exit ──   --autosk-sock <sock>
                                            │
   ┌────────────────────────────────────────┘
   │
   ▼
daemon (Go)
   ├─ projectmgr.Project.Runs (runstore)  ← attach_count, attach_expires_at
   ├─ runners.Registry (new)              ← jobID → *pi.Runner
   ├─ executor.Run                        ← honours attach flag
   └─ server.handle{Input,Abort,Attach,Heartbeat,Detach}
        │  (look up runner from registry, proxy to pi stdin)
        ▼
   *pi.Runner (already exists)
        │
        ▼
   pi --mode rpc (the actual agent, owned by daemon)
   pi writes session.jsonl exclusively — no race.
```

The daemon's `pi --mode rpc` process is the **single writer** to the
session file at all times. The attached pi has `--no-session`, so it
writes nothing to disk.

---

## 4. Daemon-side changes (Go)

### 4.1 Attach state — in-memory only

No schema migration. Attach state lives next to the runner registry:

```go
// Attachments is a per-daemon, in-memory counter keyed by jobID.
type Attachments struct { ... }
func (a *Attachments) Acquire(jobID string) (release func())
func (a *Attachments) Attached(jobID string) bool
```

`Acquire` is called by the SSE handler when the connection carries
`?attach=true`. The returned `release` is invoked via `defer` so the
counter always decrements on disconnect, including panics.

Daemon-restart semantics are already covered: the existing recovery
path rewrites stale `running` rows to `failed` with
`error='daemon_restart'`, and live pi processes don't survive the
restart anyway, so the counter starting at zero is correct.

### 4.2 `internal/daemon/pirunners` — runner registry (new package)

Holds the `*pi.Runner` (or `PiRunner` interface) for every currently
running job in the daemon, keyed by `jobID`.

```go
package pirunners
type Registry struct { ... }
func New() *Registry
func (r *Registry) Register(jobID string, runner pi.RunnerHandle)
func (r *Registry) Unregister(jobID string)
func (r *Registry) Get(jobID string) (pi.RunnerHandle, bool)
```

`pi.RunnerHandle` is the narrowed interface that the HTTP handlers need
(currently a subset of `executor.PiRunner`: `SendCommand`,
`Abort`). The executor registers on spawn and unregisters in `cleanup`.

The registry is injected into `executor.Deps` and into
`server.Deps`. Two consumers, one source of truth. The sibling
`Attachments` (§4.1) lives in the same package and is wired the same way.

### 4.3 HTTP routes — `internal/daemon/server`

Two new routes plus one extension to the existing SSE route:

| Method | Path | Body | Effect |
|---|---|---|---|
| `GET`  | `/v1/jobs/{id}/stream?attach=true` | — | Existing SSE stream. With `attach=true` the handler calls `Attachments.Acquire(jobID)` and defers the release on disconnect. Without it, behaves exactly like today (read-only, no pause). |
| `POST` | `/v1/jobs/{id}/input`  | `{message, images?, streamingBehavior?}` | Forward to pi as `prompt` when pi is idle; as `steer` (default) or `follow_up` (explicit) when streaming. Daemon picks the command automatically — clients don't need to track pi's state. |
| `POST` | `/v1/jobs/{id}/abort`  | — | `runner.Abort(ctx)`. |

Notes:
- `POST /input` returns `409` if `runner` not found (terminal run) or
  `runner.SendCommand` reports rejection (e.g. malformed JSON).
  Concurrent writers are not rejected — they queue via pi's existing
  `steer`/`follow_up` mechanisms and appear in the SSE stream of
  every reader in the order pi delivers them to the LLM.
- All routes require the existing `X-Autosk-Cwd` /
  `X-Autosk-DB` headers, like the rest of the API.

### 4.4 Executor — `internal/daemon/executor`

The turn loop in `Executor.Run` learns three things:

1. **Register runner.** Right after spawning the `runner` and before
   the `SendPrompt(initialMsg)`, call
   `e.deps.Runners.Register(jobID, runner)`. In `cleanup`,
   `Unregister(jobID)`.

2. **Skip corrections while attached.** Inside the
   `for { WaitForAgentEnd; ...check signals... }` loop, immediately
   before the corrective `SendPrompt`, check
   `e.deps.Attachments.Attached(jobID)`. If true: do nothing —
   continue the loop (the next `WaitForAgentEnd` will catch the
   next user-typed turn). Do **not** increment `corrections_used`.

3. **Suspend idle-timeout while attached.** Build `turnCtx` from
   `ctx` (no deadline) when `Attachments.Attached(jobID)`, else
   `context.WithTimeout(ctx, e.cfg.IdleTimeout)` as today.

The existing `step_signals` path is unchanged: if an attached user
types a prompt that ends in the agent emitting `autosk step next`,
the executor sees the signal, breaks the loop, advances the task,
exits — exactly like a non-attached run.

### 4.5 Cancellation interaction

`DELETE /v1/jobs/{id}` (cancel) still works while attached. The
scheduler kills the runner; SSE clients see `done`; the attach record
is dropped by the runner's `cleanup`.

---

## 5. Extension — `extension-attach/` (new package, TypeScript)

### 5.1 Files

```
extension-attach/
├── package.json          # name: "pi-autosk-attach"
├── tsconfig.json
├── README.md
└── src/
    ├── index.ts          # factory, registers flag + hooks
    ├── daemon-client.ts  # UDS HTTP + SSE client
    ├── renderer.ts       # registerMessageRenderer for "autosk-remote"
    └── types.ts          # daemon SSE event types
```

### 5.2 Wire-up sketch

```ts
export default async function attach(pi: ExtensionAPI) {
  pi.registerFlag("autosk-job",  { type: "string", description: "..." });
  pi.registerFlag("autosk-sock", { type: "string", description: "..." });

  pi.registerMessageRenderer("autosk-remote", renderRemoteEvent);

  let client: DaemonClient | undefined;

  pi.on("session_start", async (_, ctx) => {
    const job  = pi.getFlag("autosk-job")  as string;
    const sock = pi.getFlag("autosk-sock") as string;
    if (!job || !sock) {
      ctx.ui.notify("autosk-attach: missing --autosk-job/--autosk-sock", "error");
      ctx.shutdown();
      return;
    }
    client = new DaemonClient(sock, job, ctx);
    // Replay history once, then keep the SSE stream open with attach=true.
    // The stream connection IS the attach: closing it auto-detaches.
    await client.replayHistory(event => render(event));
    client.streamWithAttach(event => render(event));
  });

  pi.on("input", async (event, ctx) => {
    if (event.source !== "interactive") return { action: "continue" };
    if (!client) return { action: "handled" };
    await client.sendInput(event.text, event.images);  // POST /input
    return { action: "handled" };
  });

  pi.on("session_shutdown", async () => {
    await client?.close();  // closes SSE → daemon-side defer releases attach
  });

  function render(event: DaemonEvent) {
    pi.sendMessage({
      customType: "autosk-remote",
      content: summarise(event),  // one-line fallback text
      details: event,
      display: true,
    });
  }
}
```

### 5.3 What the renderer must cover

Per pi's event types from the daemon SSE (`internal/daemon/transcript`):

| Daemon event | Rendered as |
|---|---|
| `user_text`           | user bubble with the original timestamp |
| `assistant_text`      | assistant text block |
| `assistant_thinking`  | thinking block (collapsed by default) |
| `tool_call`           | tool-call tile, name + args summary |
| `tool_result`         | tool-result tile under its call, OK / ERR badge |
| status changes (`status` SSE event) | dim status line in the footer |

Streaming (`message_update`) is rendered by **appending** to the
last `autosk-remote` message of the same `customType + cursor_id`.
Cursor id comes from the daemon SSE event-id.

### 5.4 Replay on attach

Before opening the SSE stream the client `GET`s
`/v1/jobs/{id}/messages?full=true` once to seed local rendering with
the conversation so far. After that it switches to SSE for the live
delta. Both requests target the same daemon socket; the SSE call
adds `?attach=true` so the open connection itself marks the run
as attached for as long as the client is around.

### 5.5 Reconnect

There is no heartbeat. On SSE disconnect the extension tries to
re-open the stream up to N times with backoff. If the run has gone
terminal (`done` event observed, or a fresh `GET /v1/jobs/{id}`
returns terminal status), the extension calls `ctx.shutdown()`.

---

## 6. CLI — `autosk attach`

New subcommand under `cmd/autosk/attach.go`:

```bash
autosk attach <job-id> [--sock PATH] [--pi-bin PATH]
```

Behaviour:

1. Resolve sock path (default `defaultSockPath()`).
2. Resolve the bundled extension path:
   - During development: `<repo>/extension-attach/src/index.ts`.
   - Installed: `<binary-dir>/../share/autosk/extension-attach/src/index.ts`,
     and as a final fallback the npm-package resolution under
     `~/.autosk/packages/node_modules/pi-autosk-attach/src/index.ts`.
3. `syscall.Exec` (replacing the current process) into:

   ```bash
   pi --no-session --no-builtin-tools \
      -e <ext-path> \
      --autosk-job <id> \
      --autosk-sock <sock>
   ```

Replacing the process keeps the terminal attached cleanly — no
intermediate Go process holding the tty. Falls back to `os/exec`
on Windows.

---

## 7. Tests

| Layer | Test |
|---|---|
| pirunners.Registry + Attachments | Register/Get/Unregister and Acquire/release concurrency; counter stays consistent under parallel acquires. |
| executor | Attached run **does not** consume a correction or send a corrective prompt; idle-timeout suspended. After all clients detach, correction-loop and idle-timeout resume on the next turn boundary. |
| server | `?attach=true` SSE acquires/releases via defer (use `httptest` cancellation to simulate disconnect). `POST /input` selects `prompt` when idle, `steer` when streaming, and `follow_up` when explicitly requested. `POST /abort` proxies through. 404 for unknown job; 409 when no runner (terminal run). Concurrent `POST /input` from two clients both succeed and end up in the runner's stdin order. |
| e2e | Existing `daemon_uds_e2e_test.go` style: drive a fake pi runner, two concurrent attached clients sending input, assert messages, attach counter, and that correction-loop didn't fire while attached. |
| extension | `tsc --noEmit` on the new package; no runtime test in v1. |

---

## 8. Phasing — autosk task IDs

The plan is decomposed as a small DAG. Parent task gathers the
unblock relationships:

Tracked as a single autosk task (`as-1007`). Internal phases:

```
1. internal/daemon/pirunners: runner Registry + in-memory Attachments,
   inject into executor.Deps and server.Deps.
2. server: SSE handler honours ?attach=true (Acquire/release via defer);
   new POST /v1/jobs/{id}/input that auto-picks prompt|steer|follow_up;
   new POST /v1/jobs/{id}/abort.
3. executor: skip corrections + suspend idle-timeout when
   Attachments.Attached(jobID) is true.
4. extension-attach/ TS package: replay via /messages?full=true,
   then SSE with ?attach=true, custom renderer, input interceptor.
5. cmd/autosk/attach.go: resolve sock + ext path, exec pi with
   --no-session --no-builtin-tools -e <ext> --autosk-job --autosk-sock.
6. docs/attach.md + README pointer.
7. e2e smoke: fake-pi backend, two concurrent attached clients.
```

No blocking edges to model in autosk — the work fits in one focused
branch; we will revisit if more than one agent ends up working on
it in parallel.

---

## 9. Open items / future work

- **Tool-call rendering fidelity.** The custom renderer is best-
  effort; if it drifts visibly from native pi, we may revisit
  Design 1 (custom-provider proxy + tool stubs).
- **Remote daemon access.** All paths are local UDS today. A
  network-mode iteration will need TLS + auth; explicitly deferred.
- **Replay buffer size.** `GET /messages?full=true` is fine for
  the typical workflow run but may need pagination for long runs.
- **Stale TCP detection.** UDS conn close is normally instant, but
  a wedged peer could keep an attach "live" indefinitely. If this
  becomes a real problem, add a server-side keepalive write on the
  SSE stream and treat write errors as detach.

---
