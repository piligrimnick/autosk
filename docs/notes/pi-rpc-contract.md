# pi `--mode rpc` contract (D0 reconnaissance)

**Probed pi version:** `0.74.1`
**Source-of-truth files** (read from the installed `dist/`):

- `@earendil-works/pi-coding-agent/dist/modes/rpc/rpc-types.d.ts`
- `@earendil-works/pi-coding-agent/dist/modes/rpc/rpc-mode.{js,d.ts}`
- `@earendil-works/pi-agent-core/dist/types.d.ts` (defines `AgentEvent`)
- `@earendil-works/pi-coding-agent/dist/core/agent-session.d.ts` (defines
  `AgentSessionEvent = AgentEvent | …`)

Reproduce with `scripts/pi-rpc-probe.sh`.

---

## Wire format

JSON Lines on both stdin and stdout. One object per `\n`-terminated line.
No JSON-RPC framing, just bare objects with a `type` discriminator.

```
stdin   →  RpcCommand
stdout  ←  RpcResponse | AgentSessionEvent | RpcExtensionUIRequest
stdin   →  RpcExtensionUIResponse  (only as replies to a request)
```

---

## Commands we send (subset the daemon uses)

| `type`               | Required fields                       | Used for |
|----------------------|---------------------------------------|----------|
| `prompt`             | `message`                             | Start a fresh user turn on an idle agent (initial run prompt + every kickback). |
| `follow_up`          | `message`                             | Queue an additional user turn while the agent is still streaming. Daemon does not use this in v0. |
| `abort`              | —                                     | Stop the current run. Used by cancel before SIGTERM. |
| `get_state`          | —                                     | Read `sessionFile`, `sessionId`, `messageCount`. Re-polled with backoff in the background after the first `prompt` until `sessionFile` populates, then recorded as `pi_session_id` / `session_path`. |
| `set_model`          | `provider`, `modelId`                 | Reserved; daemon prefers `--model` CLI flag. |
| `set_thinking_level` | `level` (`off|minimal|low|medium|high|xhigh`) | Reserved; daemon prefers `--thinking` CLI flag. |

Each command MAY carry an `id: string` for correlation; the response echoes it.

**Important: `prompt` response semantics.** `rpc-mode.js` emits the
`{type:"response", command:"prompt", success:true}` line as soon as
**preflight succeeds**, NOT when the agent is done. Completion is signalled
by the `agent_end` event (see below). The response only tells you the
command was accepted.

---

## Events we observe

### From `AgentEvent` (the core loop)

| `type`                    | Meaning |
|---------------------------|---------|
| `agent_start`             | Loop started for one prompt. |
| `agent_end`               | **Loop fully done. This is the daemon's end-of-turn marker.** Payload: `{ messages }`. |
| `turn_start`              | One assistant request inside the run is starting. |
| `turn_end`                | One assistant request finished; `{ message, toolResults }`. |
| `message_start`           | New user/assistant/toolResult message; `{ message }`. |
| `message_update`          | Streamed assistant delta; `{ message, assistantMessageEvent }`. |
| `message_end`             | Final form of a message; `{ message }`. |
| `tool_execution_start`    | `{ toolCallId, toolName, args }`. |
| `tool_execution_update`   | `{ toolCallId, toolName, args, partialResult }`. |
| `tool_execution_end`      | `{ toolCallId, toolName, result, isError }`. |

### From `AgentSessionEvent` extensions

`queue_update`, `compaction_start|end`, `session_info_changed`,
`thinking_level_changed`, `auto_retry_start|end`. Daemon projects these
to `kind:"other"` and only relays them on the `job-event` notification
stream (`job.subscribe`).

### Responses and requests

- `{ type:"response", command, success, data?, error? }` — async ack/error.
- `{ type:"extension_ui_request", id, method, … }` — pi asks the host for
  input. **The daemon replies with `cancelled:true`** for `select`, `input`,
  `confirm`, `editor` (so blocking dialogs don't hang headless runs);
  `notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text` are
  fire-and-forget — no reply needed.

---

## End-of-turn rule for the daemon

**End-of-turn = receipt of an `agent_end` event whose source can be matched
back to the most recent `agent_start`.** Concretely:

1. Send `{type:"prompt", message}` on stdin.
2. Stream stdout until you see `{type:"agent_end", ...}`.
3. That's one checkpoint. Run `verifyClosure` (plan §6.1.1).
4. If invalid and corrections remaining: send another
   `{type:"prompt", message: correctiveMessage}` and goto 2.
5. If valid or corrections exhausted: close stdin to request shutdown.

`agent_end` is awaited (RPC's listener completes settlement). After it
the agent is **idle** — sending another `prompt` is the correct way to
re-engage. `follow_up` is only meaningful while a run is still active.

---

## Clean shutdown

Closing stdin triggers `shutdown(0)` (see `process.stdin.on("end", …)` in
`rpc-mode.js`). SIGTERM and SIGINT also work, via the same `shutdown`
path. SIGHUP exits 129; otherwise the process exits 143.

The daemon's strategy:

1. Stop sending. Close stdin.
2. Wait `grace` (default 10 s) for clean exit.
3. SIGTERM. Wait the rest of the grace window.
4. SIGKILL.

---

## CLI flags the daemon spawns pi with

```
pi --mode rpc
   --model     <model>          # iff request.model != ""
   --thinking  <level>          # iff request.thinking != ""
   --session-dir <per-job dir>  # always; resolves session.jsonl
   --no-voice                   # avoid audio side-effects in headless runs
   --no-peon                    # ditto
   (request.extra_args appended)
```

We deliberately do NOT pass `-p` / `--print` — that disables RPC's
long-lived stdin handling.

---

## Pi session id & session file path

Captured via `{type:"get_state"}`. The response's `data` contains
`sessionId` and `sessionFile`. The daemon writes both to
`daemon_runs.pi_session_id` / `daemon_runs.session_path`.

**Why we poll, not query once.** pi 0.74-0.75 creates `session.jsonl`
lazily inside its session manager — the file path is stamped on the
first persist (after the first prompt is preflight-accepted), not at
spawn time. A `get_state` issued right after spawn therefore comes
back with `sessionFile == ""`. The executor kicks off a background
goroutine after a successful `prompt` send that re-issues `get_state`
with exponential backoff (100 ms → 5 s cap, ~30 s total budget) until
`sessionFile` populates, then writes through `SetPISession` exactly
once. The poll is gated on the runner type: it only runs for pi-based
agents, since custom Node runners have no pi session.

`session_path` is the file we tail for the `job.messages` RPC — the
daemon does NOT mirror events into its own table.

---

## Known unknowns (deferred past v0)

- Image inputs in `prompt` — not in the v0 API.
- `compact` lifecycle and how it interleaves with `agent_end` — we treat
  `compaction_start|end` as informational and don't re-check closure.
- `auto_retry_*` events — surfaced via the `job-event` stream; the daemon
  does not intervene.
- Pi version skew — if `dist/modes/rpc/rpc-types.d.ts` changes, only the
  projection layer + wire types in `crates/autosk-core/src/pi.rs` need
  updating.
