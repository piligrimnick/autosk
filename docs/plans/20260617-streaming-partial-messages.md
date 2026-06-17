# Streaming partial agent messages to the front ends

**Status:** proposed
**Date:** 2026-06-17
**Scope (this iteration):** daemon + SDK + pi-agent + Tauri GUI. The Go wire
types are mirrored for conformance; the `autosk lazy` TUI live-rendering is a
follow-up phase (it must only *tolerate* the new frame for now).

## 1. Goal

When an agent (today `pi-agent`) streams a model turn, render the partial
assistant message in the GUI **as it arrives**, instead of only after the whole
step is committed to the session transcript. Target: minimal added latency,
trivial/robust clients, zero new disk writes, no new IPC `listen`/`invoke`
sites.

## 2. Why nothing streams today

`pi --mode rpc` already emits incremental events during a turn —
`message_update` carrying a cumulative `partial: AssistantMessage`, plus
`text_delta` / `thinking_delta` / `toolcall_delta`. That is exactly what pi
renders its own live UI from.

The autosk pipeline drops them:

- **Driver ignores deltas.** `daemon/extensions/pi-agent/src/driver.ts:205-224`
  (`onLine`) handles only `agent_start`/`agent_end`, `message_start`/`message_end`,
  `tool_execution_start`. On `message_end` it calls `mirrorMessage` →
  `hooks.onMessage` → `ctx.log.message()`.
- **Transcript is durable + line-based.** `ctx.log.message` →
  `TranscriptWriter.message` (`daemon/core/src/engine/transcript.ts:46`) appends a
  line to `.jsonl` through a serial promise chain, then fires `onEntry` →
  `engine.emitSessionMessage` → `session-event` with `kind:"message"`.
- **Subscriptions tail committed lines only.** `SessionSubscription.pump()`
  (`daemon/core/src/rpc/connection.ts:240`) re-reads new `.jsonl` lines from a
  monotonic cursor and pushes them as `kind:"message"`. The GUI reducer
  (`session/transcriptAppended`) dedups by line number.

So the wire and the GUI are already fully event-driven (replay-then-tail, no
polling). The one missing primitive is an **ephemeral partial channel** that is
*not* written to disk and *does not* touch the line cursor.

## 3. Chosen design

Decisions taken (see §8 for rejected alternatives):

1. **Channel:** extend the existing `session-event` notification with a new
   `kind:"partial"`. No new notification name, no new Tauri event, no new GUI
   `listen` site. Partials ride the same per-session `session.subscribe`
   replay-then-tail subscription, preserving order relative to the eventual
   committed message.
2. **Payload:** the full **cumulative** assistant message snapshot per frame
   (`TranscriptMessage`, with `text`/`thinking`/`toolCall` blocks), **coalesced**
   on the producer side (~40 ms) so frame rate is bounded. The client just
   *replaces* its current partial — idempotent, loss-tolerant, and correct when a
   client joins mid-stream.
3. **Persistence:** partials are **never** appended to `.jsonl`. The final
   `message_end` produces the one durable line exactly as today; that committed
   line supersedes the partial.
4. **Ordering:** partial emission is funnelled through the **same
   `TranscriptWriter` serial chain** as durable appends, so a partial of message
   *N+1* can never overtake the commit of message *N* (the chain preserves the
   producer's event order across the async disk write).

### 3.1 Data flow (end to end)

```
pi  --message_update(cumulative partial)-->  driver.onLine
      └─ Coalescer (~40ms, flush on block/段 end + message_end)
           └─ hooks.onPartial(msg)
                └─ ctx.partial(msg)                         [SDK: ephemeral]
                     └─ TranscriptWriter.partial(msg)        [same serial chain, NO disk]
                          └─ onPartial → engine.emitSessionPartial
                               └─ EngineEvent {session-event, kind:"partial", message}
                                    └─ daemon.onEngineEvent → per-session subs only
                                         └─ SessionSubscription.onEvent(kind:"partial")
                                              └─ frame WITHOUT pump / WITHOUT cursor advance
                                                   └─ conn.send session-event(kind:"partial")
                                                        └─ Tauri re-emits verbatim
                                                             └─ store.tsx router case "partial"
                                                                  └─ reducer partialBySession[id] = msg
                                                                       └─ Transcript.tsx live tail bubble
pi  --message_end-->  (unchanged) durable .jsonl line --kind:"message"--> clears partial
```

## 4. Changes by package

### 4.1 pi-agent — `daemon/extensions/pi-agent/`

- `src/driver.ts`
  - Add `onPartial?(message: TranscriptMessage): void` to `PiDriverHooks`
    (near `onActivity`, `onMessage`).
  - In `onLine`, add `case "message_update":` → take `msg.message` (the
    cumulative `AssistantMessage`), validate it is an `assistant` role, feed it to
    a small **Coalescer** (leading + trailing edge, ~40 ms `min-interval`).
  - Flush the coalescer immediately on `message_end` and on `agent_end` (so the
    last partial never lingers past the committed line), and drop any pending
    partial once the committed message is mirrored.
  - The Coalescer is a tiny private helper (latest-snapshot + timer); no external
    dep.
- `src/index.ts`
  - Wire the hook where the driver is built (`createPiAgent.onRun`,
    `index.ts:106`): `onPartial: (m) => ctx.partial(m)`. Same wiring applies to
    the interactive chat loop (it builds the same driver).
- Tests: `driver`-level test that a sequence of `message_update` frames produces
  coalesced `onPartial` calls and that `message_end` both flushes and stops
  partial emission; assert no partial is emitted after the committed message.

### 4.2 SDK — `daemon/sdk/`

- `src/agent.ts`: add an **ephemeral** method to `AgentRunContext`:
  ```ts
  /**
   * Streams a partial (in-progress) message snapshot to live subscribers.
   * EPHEMERAL: never written to the transcript, best-effort, superseded by the
   * next committed `log.message`. Cumulative — send the full current snapshot.
   */
  partial(message: TranscriptMessage): void;
  ```
  Keep it on `ctx` (sibling of `log`), not inside `TranscriptAPI`, to make the
  "not durable" contract explicit.
- `src/proto.ts`: extend `SessionEventParams`:
  ```ts
  kind: "message" | "status" | "done" | "error" | "partial";
  /** Present on `partial`: a non-persisted, cumulative message snapshot. */
  partial?: TranscriptMessage;
  ```
  Update the doc-comment; `RPC_NOTIFICATIONS` is unchanged (no new notification).
  The compile-time conformance block stays green.

### 4.3 Daemon core — `daemon/core/`

- `src/engine/transcript.ts`: add
  ```ts
  partial(message: TranscriptMessage): void
  ```
  which enqueues an **emit-only** step on the existing `this.chain` (no
  `appendEntry`, no id, no cursor) that calls a new constructor callback
  `onPartial?(message)`. Routing it through the same chain is what guarantees
  ordering vs. the durable commit.
- `src/engine/session.ts`
  - `buildContext`: add `partial: (m) => this.transcript.partial(m)` to `base`.
  - Construct `TranscriptWriter` with the new `onPartial` →
    `this.host.emitSessionPartial(this.project, this.id, m)`.
- `src/engine/types.ts`
  - `EngineEvent` `session-event` `kind` union: add `"partial"`; add an optional
    `message?: TranscriptMessage` field for the snapshot.
  - `SessionHost`: add `emitSessionPartial(project, sessionId, message)`.
- `src/engine/engine.ts`: add `emitSessionPartial` →
  `this.emit({ type:"session-event", kind:"partial", root, session_id, message })`.
- `src/rpc/connection.ts`
  - `SessionEventInput`: add `kind:"partial"` + `partial?: TranscriptMessage`.
  - `SessionSubscription.onEvent`: for `kind:"partial"` **return after forwarding
    the partial frame directly** — do **not** call `pump()` and do **not** touch
    `this.cursor`. Add a `partialFrame(message)` builder that sends
    `{ kind:"partial", partial }` (no `line`, no `session`).
- `src/rpc/daemon.ts` `onEngineEvent`
  - Forward `partial` to per-session subscribers (the existing loop) — pass
    `partial` through.
  - Exclude `partial` from the project-scope `session-changed` broadcast: change
    the guard from `kind !== "message"` to only `status|done|error` (partials are
    high-frequency and carry no meta).
- Tests (`test/rpc.subscriptions.test.ts`, transcript test):
  - a `partial` frame is delivered to a per-session subscriber, **carries no
    line**, and **does not advance the cursor** (a following committed message
    still arrives at the expected line);
  - a `partial` emitted between commit *N* and commit *N+1* never reorders ahead
    of commit *N* (ordering via the writer chain);
  - `partial` is **not** broadcast on `session-changed`.

### 4.4 Go mirror — `internal/daemon/api/types.go`

- `SessionEventParams`: add `Partial *TranscriptMessage \`json:"partial,omitempty"\``
  and extend the doc comment kinds to `message | status | done | error | partial`.
- The lazy TUI session reducer must **tolerate** `kind:"partial"` (ignore it for
  now). Add a decode test asserting an unknown-to-render `partial` frame does not
  break tailing. (Live rendering in `autosk lazy` = Phase 2.)

### 4.5 Tauri GUI — `gui/`

- `src/types.ts`: mirror the proto change (`kind` union + `partial?`).
- `src/state/types.ts`: add `partialBySession: Record<string, TranscriptMessage | null>`.
- `src/state/store.tsx` router (`subscribeSessionEvents`, ~line 489): add
  `case "partial": dispatch({ type:"session/partial", sessionId: evt.session_id,
  message: evt.partial })`.
- `src/state/reducer.ts` (`transcriptSlice`):
  - `session/partial` → set `partialBySession[id] = message`.
  - In `session/transcriptAppended`, when the appended entry is a committed
    `assistant` message → clear `partialBySession[id] = null` (the durable line
    supersedes the live bubble).
  - Clear on `session/upsert` terminal (`done`/`failed`/`aborted`) and on
    `session/subscribed` reset / `session/transcriptReset`.
- `src/features/center/components/Transcript.tsx`: after the committed rows,
  render `partialBySession[sessionId]` (if non-null) as a trailing **live** row
  reusing the existing assistant-message renderer, with a typing/cursor
  indicator. `useStickToBottom` already follows the tail.
- **IPC discipline preserved:** partials reuse the `session-event` Tauri event —
  `events.ts` (sole `listen`) and `ipc.ts` (sole `invoke`) are untouched.
  `npm run typecheck` (`check-ipc-discipline.mjs`) stays green.
- Tests: reducer unit tests for set-on-partial / clear-on-commit / clear-on-terminal.

## 5. Ordering & correctness notes

- **Commit-vs-partial race** is the only real hazard: `ctx.log.message` defers
  its engine emit until *after* the async disk append, while `ctx.partial` would
  emit immediately. Funnelling both through the single `TranscriptWriter.chain`
  (commit appends, partial emits) keeps engine-emission order == producer event
  order, so the subscription (also serial) sees `…partial(N), commit(N),
  partial(N+1), commit(N+1)…`.
- **Mid-stream join:** a client that subscribes while a turn streams replays
  committed lines, then receives the next cumulative snapshot — instantly correct
  (no delta backlog to reconstruct).
- **Loss tolerance:** dropping a coalesced snapshot only costs one frame of
  freshness; the next snapshot is whole.
- **Remote mode / multiple clients:** unchanged — the daemon fans `partial` to
  every active per-session subscriber over the same connection notification path.

## 6. Performance

- Cumulative snapshots are O(message) bytes per frame; coalescing at ~40 ms caps
  this at ~25 frames/s over a local UDS / Tauri IPC — negligible for typical
  message sizes. (If a pathologically long single message ever matters, a later
  optimization can switch the producer to text-delta frames behind the same
  `kind:"partial"` envelope without touching the consumers.)
- No new file I/O. The `.jsonl` and the P5 cursor model are unaffected.

## 7. Phasing

1. **Phase 1 (this plan):** SDK `ctx.partial` + proto `kind:"partial"`; daemon
   writer/engine/subscription/daemon fan-out; pi-agent `message_update` +
   coalescer; Go type mirror (tolerate); GUI live tail bubble; tests; docs +
   changelog.
2. **Phase 2 (follow-up):** render the live partial in `autosk lazy` (gocui
   session pane) using the already-mirrored Go `Partial` field.

## 8. Rejected alternatives

- **New `session-partial` notification.** Cleaner separation but needs a new
  notification name, a new Tauri event, a new `events.ts` registration (second
  `listen` site → fights IPC discipline), and a separate fan-out path. Reusing
  `session-event` is strictly less code and keeps partial/commit ordering on one
  channel.
- **Incremental deltas (`text_delta` …).** Lower bandwidth, but forces
  per-`contentIndex` accumulation in three runtimes (Go, Tauri, React), is
  fragile to drops, and complicates mid-stream join. Cumulative snapshot +
  coalescing wins on robustness for a marginal byte cost locally.
- **Persisting partials.** Breaks the append-only line cursor, bloats `.jsonl`,
  and contradicts "the agent commits one durable message per step." Partials stay
  ephemeral.

## 9. Docs & changelog

- `docs/daemon.md`: document the `session-event` `kind:"partial"` frame
  (ephemeral, not persisted, cumulative).
- `docs/workflows.md`: document the SDK `ctx.partial(message)` ephemeral channel.
- `CHANGELOG.md` (`## [Unreleased]` → `### Added`): "GUI renders streaming
  partial agent messages live as a turn is generated."

## 10. Acceptance

- In the GUI, an active pi-agent turn shows assistant text/thinking/tool-call
  blocks growing live; when the step commits, the live bubble is replaced by the
  identical durable message with no flicker or duplication.
- No new file written under `.autosk/` for partials; `.jsonl` line numbering is
  byte-for-byte what it is today.
- `bun run typecheck` + `bun test` (daemon), `go test ./...` (mirror tolerance),
  and `npm run typecheck` + `npm run test` (GUI, incl. IPC-discipline guard) pass.
