# autosk attach — read/write mirror of a running agent session

`autosk attach <job-id>` opens an in-process **Bubble Tea TUI** that
mirrors a job already running inside the daemon, and lets the
operator drive the agent without ever touching the upstream
`session.jsonl` file.

The mirror is **read/write**:

- The TUI replays the historical transcript and then streams live
  events over SSE.
- Typed text is dispatched to the daemon as `prompt | steer |
  follow_up` (the daemon picks the shape from pi's current state).
- A dedicated hotkey aborts the in-flight pi turn.

There is no local `pi` process and no extension. The terminal is
owned by the autosk binary directly, which speaks UDS/HTTP to the
daemon and pumps SSE frames into the Bubble Tea model.

- v2 design plan: [`docs/plans/20260519-Attach-Plan-v2.md`](plans/20260519-Attach-Plan-v2.md).
- v1 (abandoned) plan: [`docs/plans/20260519-Attach-Plan.md`](plans/20260519-Attach-Plan.md).
- Daemon-side HTTP routes referenced below: [`docs/daemon.md`](daemon.md).

> **Status (v2)** — local UDS only, read/write attach, no takeover /
> handover, no multi-host, no web client, and no custom JS-runner
> agents. See §[Limitations](#limitations).

---

## Quickstart

```bash
# 0. Have the daemon running and a job in flight.
autosk daemon serve &
id=$(autosk create "do thing" --workflow feature-dev --json | jq -r .id)
job=$(autosk daemon list --json | jq -r '.jobs[0].job_id')

# 1. Attach. Opens the Bubble Tea TUI in the current terminal.
autosk attach "$job"

# 2. Inside the TUI:
#    - the transcript so far is replayed once (via SSE ?full=true),
#    - then live deltas stream in as the agent runs,
#    - Ctrl-D sends the textarea buffer to the daemon (prompt|steer
#      auto-picked), Ctrl-F sends it as follow_up, Ctrl-A aborts the
#      in-flight turn,
#    - Ctrl-C or Ctrl-Q quits and detaches.

# 3. Detach.
#    Ctrl-C / Ctrl-Q closes the SSE stream; the daemon's `defer`
#    drops the attach counter on connection close.
```

While at least one client is attached the daemon:

- pauses the **correction loop** (no corrective prompts, no
  `corrections_used` bump);
- **suspends the per-turn idle timeout** at the next turn boundary
  (mid-turn attaches do not cancel an already-armed idle timer — see
  [Mid-turn attach is sampled at turn boundaries](#mid-turn-attach-is-sampled-at-turn-boundaries)).

Everything else (the executor turn loop, `step_signals` consumption,
the `MarkDone` on transition, daemon cancellation via
`autosk daemon cancel <job>`) continues to work unchanged.

---

## Architecture

```
                           ~/.autosk/daemon.sock
                                  │
   ┌──────────── HTTP/UDS ────────┴────────────┐
   │                                            │
GET /v1/jobs/{id}/stream?attach=true&full=true  │ ← single SSE call: replay
                                                │   + live tail. Connection
                                                │   lifetime IS the attach
                                                │   lifetime.
POST /v1/jobs/{id}/input                        │ ← prompt|steer|follow_up
POST /v1/jobs/{id}/abort                        │ ← stop in-flight turn
   │                                            │
   ▼                                            ▼
daemon (Go)                              client: `autosk attach`
   ├─ pirunners.Registry   (jobID → RunnerHandle)     in-process Bubble Tea:
   ├─ pirunners.Attachments (jobID → counter)            internal/attach/tui
   ├─ executor.Run (honours Attachments.Attached)        + internal/daemon/client
   └─ server.handle{Stream,Input,Abort}                  (typed UDS HTTP+SSE)
        │
        ▼
   pi --mode rpc          ← single writer to session.jsonl. Never racing.
```

There is **no** second pi-process touching the session file. The
attach client never writes anywhere on disk; it owns a terminal and
a UDS connection, that's it.

---

## CLI — `autosk attach`

```
autosk attach <job-id>
  [--sock PATH]   unix socket path (default: $AUTOSK_SOCK or ~/.autosk/daemon.sock)
  [--cwd  PATH]   project root for X-Autosk-Cwd (default: current dir)
```

`autosk attach` opens an HTTP/UDS client against the daemon (the same
client the rest of the CLI uses) and runs `internal/attach/tui` in
the foreground. The Bubble Tea program owns the terminal until you
quit — there is no fork, no `syscall.Exec`, no subprocess.

### Environment

| Var | Effect |
|---|---|
| `AUTOSK_SOCK` | Default unix socket path; overridden by `--sock`. |
| `AUTOSK_DB`   | Forwarded as the `X-Autosk-DB` request header so the daemon can locate the right project DB without relying on cwd discovery. Same precedence as the rest of the CLI (see [`docs/daemon.md`](daemon.md)). |

There is no `AUTOSK_ATTACH_EXTENSION` or pi-bin flag anymore — both
belonged to the retired Design 2.

---

## TUI layout

The TUI uses [Bubble Tea](https://github.com/charmbracelet/bubbletea)
with three stacked regions:

```
┌───────────────────────────────────────────────────────────────────┐
│  transcript viewport                                              │
│  (one styled block per daemon SSE event:                          │
│     user_text · assistant_text · assistant_thinking ·             │
│     tool_call · tool_result · session/model/compaction metadata   │
│   wrapped to the current terminal width)                          │
│                                                                   │
│                                                                   │
├───────────────────────────────────────────────────────────────────┤
│ ▎ multi-line textarea                                             │
│   (Enter inserts a newline; explicit hotkeys submit)              │
├───────────────────────────────────────────────────────────────────┤
│ attach as-XXXX · running · streaming · attached: 1 · corrections:  │
│ 0/3                                            ← flash messages → │
└───────────────────────────────────────────────────────────────────┘
```

### Key bindings

The bindings are intentionally Ctrl-based because Bubble Tea and most
terminals do not deliver Alt/Meta combos reliably (macOS Terminal
swallows Alt by default).

| Key | Action |
|---|---|
| `Enter` | Insert a newline in the textarea (multi-line input). |
| `Ctrl-D` | **Send.** POSTs the textarea buffer to `/v1/jobs/{id}/input`. The daemon picks `prompt` (pi idle) or `steer` (pi streaming). |
| `Ctrl-F` | **Send as `follow_up`.** Same dispatch path but with `streamingBehavior: "follow_up"` — queued for delivery after the agent's current turn-chain. |
| `Ctrl-A` | **Abort.** POSTs `/v1/jobs/{id}/abort` to stop the in-flight pi turn. Run row stays non-terminal. |
| `Ctrl-C` / `Ctrl-Q` | Quit. Closes the SSE stream → daemon-side `defer` decrements the attach counter. |
| `Ctrl-K` / `Ctrl-J` | Scroll the transcript viewport up / down one line. |
| `PgUp` / `PgDn` | Half-page up / down in the viewport. |

> **There is no typed `/abort` command.** The v1 design routed abort
> through the input channel as a `/abort` (or `!abort`) prefix string.
> The v2 TUI uses a dedicated hotkey (`Ctrl-A`) for it, so any text
> you type is dispatched verbatim as agent input — including a literal
> "/abort", which the agent will see as a prompt.

`Ctrl-Enter` is **not** a binding in v2. The original spec listed it
alongside `Ctrl-D`, but most terminals (macOS Terminal, iTerm2 in
default config, the linux console) don't differentiate it from a
plain Enter — they emit the same `\r` byte. We dropped it to avoid a
chord that fires on some terminals and never fires on others.

### Status bar fields

The bottom line is composed by `renderStatusBar` (see
[`internal/attach/tui/render.go`](../internal/attach/tui/render.go))
and shows, left-to-right:

| Slot | Source | Notes |
|---|---|---|
| `attach <jobID>` | CLI arg | Stable for the lifetime of the TUI. |
| run status (e.g. `running`, `done`, `failed`) | `api.JobResponse.Status` | The terminal statuses get a coloured highlight on the right of the line. |
| `streaming` or `idle` | `api.JobResponse.Streaming` | **Authoritative** — the daemon samples `*pi.Runner.IsStreaming()` on every status frame. No event-kind heuristic. |
| `attached: N` | `api.JobResponse.AttachCount` | The number of clients currently attached, including the local one. |
| `corrections: K/M` | `api.JobResponse.CorrectionsUsed` / `MaxCorrections` | Surfaced so the operator can tell whether the corrective loop is about to kick in if they detach. |
| terminal note (right-coloured) | only when status is terminal | `done` / `failed` / `cancelled`. |
| flash message (right-aligned) | ephemeral | "dispatched: steer", "abort sent", "input rejected (HTTP 409): …", etc. Shown for ~2 seconds. |

Before the first status frame arrives the bar reads
`attach <jobID> · connecting…`.

### Exit behaviour

On clean exit the TUI restores the alt-screen and prints a one-line
summary to stderr so the operator's terminal doesn't snap back to
the prompt without context. Four shapes, picked by `printExitSummary`:

- `autosk attach <job>: detached (no status received)` — the stream
  closed before any status frame arrived (rare; usually a daemon
  disconnect during the very first HTTP burst).
- `autosk attach <job>: <status> (read-only inspection — N events)` —
  you attached to a job that was **already terminal** at connect time
  (see below) and exited via `Ctrl-C`. N counts the full transcript,
  including kinds that the TUI renders as empty.
- `autosk attach <job>: run <status> during session (<summary>)` — the
  run went terminal mid-session and the TUI auto-quit. Summary
  includes exit code, duration, and the error string if any.
- `autosk attach <job>: detached (run was <status>)` — non-terminal
  detach (you hit `Ctrl-C` while the run was still alive). The run
  is still running in the daemon; nothing was cancelled.

### Terminal-at-connect vs. mid-session terminal

The first status snapshot the TUI receives is special:

- **Already terminal at connect** (`terminalAtConnect = true`): the
  TUI treats the session as a historical read-only inspection. The
  stream closes immediately after replay but the TUI stays open so
  you can scroll. You exit on `Ctrl-C`.
- **Alive at connect, goes terminal mid-session**: the TUI auto-quits
  on stream close so an agent finishing its workflow doesn't strand
  the operator in a dead UI.
- **Stream drops mid-flight on an alive run** (network blip, daemon
  restart, etc.): a flash is shown ("stream closed"). v2 does not
  reconnect; quit and re-run `autosk attach` to resume.

### Input dispatch during the pre-status window

Between the SSE connection opening and the first status frame
arriving, `runIsTerminal()` returns `false` (the TUI has no
`JobResponse` yet). `Ctrl-D` / `Ctrl-F` / `Ctrl-A` in that window
dispatch through to the daemon, which will reply `409` for a job
that's already terminal. This is intentional — the window is one
HTTP response burst long and the daemon is the source of truth on
"can I accept input right now". Gating the keystroke locally would
add latency for no real safety win.

### Streaming indicator semantics

`m.streaming` is driven **only** by the `streaming` field on the
status SSE frame, which the daemon updates whenever
`*pi.Runner.IsStreaming()` flips. The TUI does not infer streaming
from transcript event kinds; the daemon's tail loop emits a fresh
status frame on every transition so the indicator stays trustworthy
between turns.

---

## Daemon HTTP routes (attach-relevant subset)

All three routes live on the existing UDS HTTP API and require the
usual `X-Autosk-Cwd` (and optional `X-Autosk-DB`) headers — see
[`docs/daemon.md`](daemon.md).

| Method | Path | Body | Effect |
|---|---|---|---|
| `GET`  | `/v1/jobs/{id}/stream?attach=true&full=true` | — | SSE stream. With `attach=true` the handler increments the in-memory attach counter and defers a release on disconnect. With `full=true` the replay of the historical transcript is included before the live tail starts (so the TUI can drop its separate `/messages` call). `Last-Event-ID:` is still honoured for resume. |
| `POST` | `/v1/jobs/{id}/input`                        | `{message, streamingBehavior?}` | Dispatch the operator's text into pi as `prompt`/`steer`/`follow_up`. |
| `POST` | `/v1/jobs/{id}/abort`                        | —                                | Ask pi to stop the in-flight turn. |

### SSE frame contract

Each frame is one of:

- `event: message` — a transcript event (`api.MessageEvent`). Carries
  a monotonic `id:` so a reconnecting client can resume with
  `Last-Event-ID:`.
- `event: status` — a fresh `api.JobResponse` snapshot. Emitted on
  status changes (`running` → `done`/`failed`/`cancelled`),
  `AttachCount` transitions, `CorrectionsUsed` changes, `PID`
  transitions, and `Streaming` flips. **Status frames do not carry an
  `id:` line** so the message-counter sequence is not perturbed.
- `event: done` — terminal-state final frame, also without `id:`.

The omitted `id:` on status/done is deliberate: a client that resumes
on `Last-Event-ID:` is replaying message frames only, and reusing the
message counter for status would either skip frames or replay them
spuriously.

### `POST /v1/jobs/{id}/input` — status codes

| Code | Meaning |
|---|---|
| `200` | Dispatched. Response: `{ "job_id", "dispatched": "prompt"\|"steer"\|"follow_up" }`. |
| `400` | Malformed body or `streamingBehavior` not one of `steer`/`follow_up`. |
| `404` | Job not found in this project. |
| `409` | Run is terminal **or** no live runner registered (e.g. job is queued, pi just exited, or the agent is a custom JS runner — see [Limitations](#limitations)). |
| `422` | pi rejected the command (rare; details echo pi's error and a `retry` flag — see [TOCTOU between IsStreaming and SendCommand](#toctou-between-isstreaming-and-sendcommand)). |
| `503` | Daemon was built without a runner registry. Should not happen in normal builds. |
| `504` | Client disconnected before pi acked. |

### `POST /v1/jobs/{id}/abort` — status codes

| Code | Meaning |
|---|---|
| `200` | Pi acked the abort. `{ "job_id", "ok": true }`. |
| `404` | Job not found. |
| `409` | Run is terminal **or** no live runner registered. |
| `500` | Pi returned an error from the abort RPC. |

### `GET /v1/jobs/{id}/stream?attach=true`

Acquires the in-memory attach counter only **after** the run is
confirmed to exist in this project. A 404 (unknown / wrong-project
jobID) does **not** bump the counter. The counter is released via
`defer` on connection close.

---

## Limitations

These are deliberate v2 constraints. The plan's §9 tracks them as
future iterations.

### Attach only supports standard pi-runner agents

Custom JS runners (npm packages that declare `autosk.agent.runner`
and are dispatched through `@autosk/agent-runtime`) do **not**
implement the pi runner-handle interface. The executor logs a single
INFO line on spawn when registration is skipped:

```
attach: skipping runner registration (runner does not implement RunnerHandle)
  job_id=... agent=... package_runner=path/to/runner.ts
```

For such jobs, `autosk attach <job-id>` will open the TUI
successfully, replay the transcript, and stream live events, but
every `/input` and `/abort` POST returns `409 runner not registered
for job`. Use `autosk daemon messages <job>` for read-only inspection
of those runs in v2.

### `?attach=true` on a terminal run is replay-only

The SSE handler still accepts `?attach=true` after a run has gone
terminal: you get the historical transcript + an immediate `done`
event, and the attach counter briefly transitions `0 → 1 → 0` over
the lifetime of the response. `/input` and `/abort` on the same job
return 409. This is technically asymmetric with the write endpoints
and may be tightened (Acquire gated on `!IsTerminal`, or 410 Gone)
in a future iteration; tracked in follow-up.

### Mid-turn attach is sampled at turn boundaries

The executor reads `Attachments.Attached(jobID)` at turn boundaries
only:

- **before** `WaitForAgentEnd` to decide whether to arm the per-turn
  idle timeout, and
- **after** `WaitForAgentEnd` to decide whether the corrective
  kickback should fire.

That means: if you attach mid-turn while the idle timer is already
armed (e.g. you attach 29 minutes into a turn with `--idle-timeout 30m`),
the timer is **not** cancelled — the turn will still fail with
`idle_timeout` if the agent stays silent. Attach at the next turn
boundary onwards suspends the timer correctly.

Live cancellation (cancel `turnCtx` on `false → true` transition of
the attached counter) is tracked as future work and would require
`Attachments` to expose a notify primitive.

### TOCTOU between `IsStreaming` and `SendCommand`

The daemon samples pi's streaming state once before dispatching a
command. Pi's reader goroutine can flip streaming between that read
and the actual write — `agent_start` or `agent_end` lands in the
middle — and pi may then reject the dispatch with a `success=false`
state-mismatch error.

The daemon detects this conservatively (pattern-matches a short
allow-list of error tokens, including `not streaming`, `already
streaming`, `idle`, `state mismatch`, …) and retries once with the
opposite shape. Non-state errors (e.g. `message too long`) are
returned one-shot as a 422 with `retry=false`. The token list is a
v2 stopgap and may match incidentally on a future pi error template
that happens to mention `idle` or `in_progress`; the long-term fix
is for pi to grow a structured `reason: "state_mismatch"` field on
its response. Tracked in follow-up.

### Viewport rebuilds are O(N) per event

The TUI rebuilds the viewport content from `m.events` on every event
arrival and on every terminal resize. This is fine for sessions with
a few hundred events but becomes quadratic during replay of very
long runs. The mitigation (append-only `strings.Builder` for the
common case, full rebuild only on resize) is a planned follow-up if
a real workflow ever shows visible stutter.

### No takeover / handover, no multi-host, no web UI

All explicitly out of scope for v2. The current model is "many
symmetric attachers, each running an in-process TUI that mirrors the
daemon's pi". Closing the connection = detach; the daemon resumes
its corrections / idle-timeout on the next turn boundary as soon as
the last attacher leaves.

---

## Related docs

- v2 plan with locked decisions: [`docs/plans/20260519-Attach-Plan-v2.md`](plans/20260519-Attach-Plan-v2.md)
- v1 (abandoned) plan: [`docs/plans/20260519-Attach-Plan.md`](plans/20260519-Attach-Plan.md)
- Daemon HTTP API and security model: [`docs/daemon.md`](daemon.md)
- pi RPC wire format: [`docs/notes/pi-rpc-contract.md`](notes/pi-rpc-contract.md)
