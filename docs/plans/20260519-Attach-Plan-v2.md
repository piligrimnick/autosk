# autosk attach — v2 plan (Go TUI client)

**Date:** 2026-05-19 (pivot recorded during the `docs` step of task `as-a018`).
**Status:** Implemented and merged. This document captures the
shape that landed; the original v1 plan is archived at
[`20260519-Attach-Plan.md`](20260519-Attach-Plan.md) for the paper trail.
**Predecessors:** [`20260518-Daemon-UDS-Plan.md`](20260518-Daemon-UDS-Plan.md),
[`20260518-Agent-Packages.md`](20260518-Agent-Packages.md),
[`20260519-Attach-Plan.md`](20260519-Attach-Plan.md) (v1, superseded).

> The server-side half of v1 (`pirunners.Registry`, `Attachments`,
> `server/attach.go`, `POST /input` / `POST /abort`, the SSE
> `?attach=true` parameter, the executor's pause-on-attach behaviour)
> shipped unchanged. **Only the client surface is different.**

---

## 1. Why we pivoted

The v1 client was Design 2 from the design discussion: a locally
launched `pi --no-session --no-builtin-tools -e <ext>` that ran a
new TypeScript extension (`extension-attach/`, package
`pi-autosk-attach`). The extension intercepted `pi.on("input", …)`,
POSTed input back to the daemon, registered a custom
`autosk-remote` renderer, and exposed `/abort` / `!abort` as a typed
command on the prompt.

That client got built and was working. While polishing it for
review the following showed up:

- **Implicit pi dependency in the attach UX.** The pi peer-dependency
  range (`>=0.74.0 <0.75.0`) had to be tracked manually because the
  renderer hooked into named theme slots; a pi minor release that
  renamed a slot silently broke the UX. The extension grew a
  startup theme-probe just to surface that.
- **No single writer to the terminal.** The `pi` instance owned the
  terminal but rendered events the daemon told it about — every
  visual decision involved either tweaking pi (out of scope) or
  fighting it (the custom renderer was at ~85% fidelity by design).
- **Two-process distribution.** Resolving the extension entry across
  development / installed / `~/.autosk/packages/node_modules` layouts
  required a 5-step search, plus a `--ext` flag, plus an
  `AUTOSK_ATTACH_EXTENSION` env var. The cumulative complexity wasn't
  carrying its weight.
- **The TS extension was a thin SSE consumer.** Strip out the
  pi-hosting concerns and the actual logic is: open SSE, decode
  frames, render blocks, POST input on a hotkey. That's a Bubble
  Tea program.

The retired v1 still lives at `extension-attach/` until cleanup task
`as-a3c2` lands; nothing in the Go build references it.

## 2. Locked decisions

| Topic | Decision |
|---|---|
| Client surface | **In-process Bubble Tea TUI** at `internal/attach/tui`, driven by a typed UDS HTTP+SSE client at `internal/daemon/client`. `cmd/autosk/attach.go` runs `tui.Run(ctx, c, jobID)` in the foreground; no `syscall.Exec`, no subprocess. |
| Mirror is **read/write** | Operator can type prompts and abort. No takeover semantics in v2. Unchanged from v1. |
| Correction-loop while attached | **Paused** (W1). No corrective prompts sent; idle-timeout suspended at turn boundaries. Unchanged from v1. |
| Multi-client model | Symmetric. Every connection can read SSE and POST input/abort. Unchanged from v1. |
| Attach lifecycle | An attach **is** an open SSE stream opened with `?attach=true`. Connection close (detected via `r.Context().Done()`) is detach. No separate attach/heartbeat/detach endpoints. Unchanged from v1. |
| Replay | One SSE call: `GET /v1/jobs/{id}/stream?full=true&attach=true`. The historical transcript is replayed first, then the live tail starts. No separate `GET /messages?full=true`. |
| Streaming indicator | Daemon authoritative. The status SSE frame carries `streaming bool`, sampled from `*pi.Runner.IsStreaming()` and re-emitted on every transition. TUI never infers streaming from transcript event kinds. |
| Status SSE frames carry no `id:` line | So the message-event monotonic id sequence is preserved for `Last-Event-ID` resume. Status/done frames are stateless from the resume perspective. |
| Key bindings | Enter → newline (textarea default). `Ctrl-D` send; `Ctrl-F` send as `follow_up`; `Ctrl-A` abort; `Ctrl-C` / `Ctrl-Q` quit; `Ctrl-K`/`Ctrl-J` scroll; `PgUp`/`PgDn` page. `Ctrl-Enter` deliberately omitted (most terminals don't differentiate it from Enter). No typed `/abort` command. |
| Status bar fields | `attach <jobID>` · run status · `streaming|idle` · `attached: N` · `corrections: K/M` · terminal note (right-coloured) · flash messages (right-aligned, ~2 s). |
| Terminal-at-connect | Special-cased: if the first status snapshot is already terminal, the TUI stays open after stream close for read-only inspection (exit on `Ctrl-C`). If the run goes terminal **during** the session, the TUI auto-quits. |
| Mid-stream errors | A non-fatal flash and stay open; no auto-reconnect in v2. |
| Storage | None on disk. Attach state is an in-memory counter inside the daemon, reset to zero on daemon start. Unchanged from v1. |

## 3. Architecture

```
                          ~/.autosk/daemon.sock
                                  │
   ┌──────────── HTTP/UDS ────────┴────────────┐
   │                                            │
GET /v1/jobs/{id}/stream?attach=true&full=true   ────► single SSE call:
                                                        replay + live tail.
POST /v1/jobs/{id}/input
POST /v1/jobs/{id}/abort
   │                                            │
   ▼                                            ▼
daemon (Go)                              client: `autosk attach`
   ├─ pirunners.Registry   (jobID → RunnerHandle)
   ├─ pirunners.Attachments (jobID → counter)        cmd/autosk/attach.go
   ├─ executor.Run (honours Attachments.Attached)        └─ tui.Run(ctx, c, jobID)
   └─ server.handle{Stream,Input,Abort}                       ├─ internal/attach/tui (Bubble Tea)
        │                                                     └─ internal/daemon/client (typed UDS)
        ▼
   *pi.Runner (single writer to session.jsonl)
```

The daemon's `pi --mode rpc` process remains the single writer to
`session.jsonl`. The client writes nothing on disk; it owns a
terminal and a UDS connection.

## 4. Daemon-side changes vs v1 (Go)

The v1 server contract is mostly preserved. Three pivot-driven
adjustments:

1. **`?full=true` on the SSE handler.** The replay-then-tail path is
   already what the SSE handler did; `?full=true` is what the
   `/messages?full=true` call used to deliver. We made `full=true`
   first-class on the stream endpoint so the client can drop the
   second HTTP call. `Last-Event-ID` resume continues to work.
2. **`api.JobResponse.Streaming bool`.** New field, populated by
   `FromRunWithAttach(run, attachCount, streaming)`. The SSE tail
   loop samples `*pi.Runner.IsStreaming()` via the registry on the
   initial snapshot and on every tick, tracks `prevStreaming`
   alongside `prevStatus`/`prevPID`/`prevCorrections`, and re-emits
   a status frame on every transition. `GET /v1/jobs/{id}` returns
   the same field for symmetry.
3. **Status / done SSE frames carry no `id:` line.** `writeSSEEvent`
   already gates the `id:` line on `id > 0`; the tail loop now
   passes `0` for status and done. Last-Event-ID resume semantics
   are unaffected (it only ever consulted message ids).

The executor, registry, and attachments package are unchanged.

## 5. Client — `internal/attach/tui` (new package, Go)

### 5.1 Files

```
internal/attach/tui/
├── model.go         # Bubble Tea root model: keymap, state, View()
├── update.go        # Update() reducer + tea.Cmd factories for input/abort/SSE
├── render.go        # styles + per-event-kind renderers + status bar
├── run.go           # public Run() entry; SSE pump goroutine; exit summary
├── model_test.go    # reducer + keymap unit tests
├── render_test.go   # rendering snapshot tests
├── run_test.go      # printExitSummary / summariseTerminal table tests
└── update_test.go   # SSE event handling + RPC fakes against httptest
```

### 5.2 Bubble Tea model shape

```go
type Model struct {
    client   *client.Client
    jobID    string
    viewport viewport.Model
    textarea textarea.Model
    keys     keyMap
    styles   styles
    width    int
    height   int

    job        *api.JobResponse
    streaming  bool
    events     []renderedEvent
    eventCount int

    flash      string
    flashUntil time.Time

    terminalAtConnect bool
    seenStatus        bool
    quitting          bool
}
```

The model owns:

- a `viewport.Model` for the transcript (rebuilt on every event and
  on resize — see [Limitations](../attach.md#viewport-rebuilds-are-on-per-event)),
- a multi-line `textarea.Model` whose Enter binding is the default
  (newline) so multi-line input "just works",
- a fixed key map (`defaultKeyMap`), and
- a `styles` struct of lipgloss adaptive-colour tokens.

### 5.3 SSE pump

`run.go` opens the SSE stream once with `Attach: true, Full: true`,
constructs the Bubble Tea program, and launches `pumpStreamToProgram`
on a goroutine that ranges over `handle.Events` and `prog.Send`s
one `streamEventMsg` per decoded frame. When the channel closes the
pump sends a final `streamClosedMsg`.

This avoids the tea.Cmd back-pressure pattern: messages are pushed
to the program from outside, so a slow `Update()` can't stall the
producer (and a stuck producer can't keep tea.Cmd running).

### 5.4 RPC commands

`sendInputCmd` and `sendAbortCmd` are plain `tea.Cmd` factories that
return `inputResultMsg` / `abortResultMsg`. Both use a fresh
`context.WithTimeout` (30 s for input, 10 s for abort) on
`context.Background` — they intentionally don't piggy-back on the
SSE context so a daemon hang on `/input` doesn't bring the stream
down.

### 5.5 Replay via `?full=true&attach=true`

The historical transcript and the live tail come through the same
SSE stream. `client.Stream(ctx, jobID, StreamOptions{Attach: true,
Full: true})` returns a `StreamHandle` whose `Events` channel
delivers `MessageEvent`s in chronological order, followed by the
first `Status` snapshot, followed by live frames. The TUI does not
distinguish replay from live frames — both flow through the same
reducer.

## 6. CLI — `autosk attach` (Go)

```bash
autosk attach <job-id> [--sock PATH] [--cwd PATH]
```

`cmd/autosk/attach.go` resolves cwd / sock / X-Autosk-DB through
`resolveDBOverride()` (shared with `daemonClient.dbOverride`), opens
a `client.Client`, and calls `tui.Run(ctx, c, jobID)` in the
foreground. No `syscall.Exec`, no subprocess. On a non-tty stdin the
client still runs but Bubble Tea will not paint usefully — calling
`autosk attach` from a script is supported for the side-effect of
the SSE stream being open (e.g. holding the attach counter > 0 from
a hands-off shell).

The previous `--pi-bin` and `--ext` flags, the
`AUTOSK_ATTACH_EXTENSION` env var, and the 5-step extension
resolution from v1 are all gone.

## 7. Tests

| Layer | Test |
|---|---|
| `pirunners.Registry` + `Attachments` | Unchanged from v1. |
| `api.FromRunWithAttach` | Includes the new `Streaming` field; covered by existing `runstore` tests. |
| `server.handleStream` | `TestSSE_StatusFrameCarriesStreamingFromRunner` pins the daemon-authoritative streaming signal (flip-on + flip-off). `TestSSE_StatusFrameOmitsEventID` pins the contract that status/done frames have no `id:` line. |
| `server.handleGet` | Symmetric `streaming` field on `GET /v1/jobs/{id}`. |
| Executor | Attached run skips corrections + suspends idle-timeout. Unchanged from v1. |
| `internal/attach/tui` reducer | `TestUpdate_StatusEvent_StreamingFromDaemon` (flip-on / flip-off), `TestUpdate_StreamEvent_AssistantTextDoesNotFlipStreaming` (regression — transcript event kinds must not affect `m.streaming`). |
| `internal/attach/tui` rendering | `TestRender*` snapshot the styled blocks for each kind. |
| `internal/attach/tui` exit summary | `TestPrintExitSummary_*` (four branches) + `TestSummariseTerminal`. |
| `internal/attach/tui` end-to-end | `update_test.go` drives a fake daemon via `httptest` (POST `/input` and `/abort`, GET `/stream`). |

## 8. Phasing

The pivot landed as a single autosk task (`as-a018`) with the
following children:

- `as-de4f` — `internal/daemon/client` typed UDS HTTP+SSE client.
- `as-45e9` — `internal/attach/tui` Bubble Tea client.
- `as-704c` — server: surface `attach_count` + `streaming` in status SSE + `JobResponse`.
- `as-8c84` — `cmd/autosk/attach.go` rewired to the in-process TUI.
- `as-ee84` — e2e attach TUI against `fakepi`.
- `as-a3c2` — cleanup: `rm -rf extension-attach/` + v1 plan redirect (this file is the redirect target).

## 9. Open items / future work

- **Append-only viewport.** Rebuilds are currently O(N) per event;
  fine for typical sessions, but a long run can stutter during
  replay. Fast path: a `viewportContent strings.Builder` that
  appends new blocks, falling back to full rebuild only on resize.
- **Live cancellation of an idle timer.** Today the executor samples
  `Attachments.Attached(jobID)` at turn boundaries. A `false → true`
  transition mid-turn could cancel `turnCtx` so the timer stops
  immediately; needs `Attachments` to expose a notify primitive.
- **Reconnect on stream drop.** v2 surfaces a flash and stays open
  on an alive-run stream drop; no automatic reconnect. A small
  backoff retry is a reasonable v2.1.
- **Custom JS-runner agents.** Still unsupported for `/input` and
  `/abort`. Tracked under the same follow-up as in v1.
- **Remote daemon access.** All paths are local UDS today. A
  network-mode iteration will need TLS + auth; explicitly deferred.

---
