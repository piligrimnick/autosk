# Interactive Sessions — Implementation Plan

Status: draft (design agreed via Q&A 2026-06-17)

Add a first-class **interactive chat session**: the user opens a session from the
UI, picks an installed agent, and talks to the model turn-by-turn. The session is
**not** tied to a task and **not** tied to a workflow — no synthetic task/workflow
is created to host it.

## 1. Agreed decisions

These were locked in before planning (do not re-litigate without revisiting):

1. **Agent selection** → introduce a **first-class agent registry**.
   `registerAgent(...)` in `AutoskAPI` + a `registry.agent.list` RPC; `pi-agent`
   registers itself as a named agent; the picker lists every registered agent.
2. **Session data model** → **reuse the `Session` entity**. Make
   `task_id`/`workflow`/`step` optional and add a `kind: "task" | "interactive"`
   discriminator. Interactive sessions live in the same `sessionStore`. No fake
   task, no fake workflow.
3. **Run contract** → **interactive mode without transit**. Reuse
   `AgentDefinition.onRun`; in interactive mode the session stays alive accepting
   user turns until the user explicitly ends it (or aborts). `ctx.transit()` is
   not required (and not available) in interactive mode.
4. **autosk-as-tools** → **chat first**. v1 ships a working chat with no autosk
   tools. The autosk tool surface (create/list/show task, comments, enroll/trigger
   workflow) is a separate follow-up.
5. **cwd / isolation** → **project root, no isolation**. `ctx.cwd === projectRoot`.
6. **Agent params** → **name only** for v1. `registerAgent` registers a named
   agent with its default options; the create modal is just an agent dropdown.
   Model/profile selection is a follow-up.
7. **End semantics** → **graceful end → `done`**. A distinct end action winds the
   agent down and seals the session `done` (not `aborted`). In the GUI, the
   interactive session shows an **End** button where a workflow session shows
   **Abort**.
8. **First message** → **empty session**. The modal only picks the agent; the
   session opens idle and the first turn comes from the composer.
9. **Front-end scope (v1)** → **Tauri GUI only**. TUI/CLI are out of scope for v1
   (the daemon surface is added so they can follow later).

## 2. Current architecture (what we build on)

- A `Session` today is always `{ task_id, workflow, step, agent }`, created by the
  scheduler when a `status=work` task hits an agent step
  (`daemon/core/src/engine/engine.ts` `dispatch`). `SessionMeta` —
  `daemon/sdk/src/types.ts`.
- An "agent" is **not** a standalone entity: it is the inline `AgentDefinition`
  value of a workflow step (`daemon/sdk/src/agent.ts`). The only registration
  hook is `AutoskAPI.registerWorkflow` (`daemon/sdk/src/api.ts`). `pi-agent`'s
  `piAgent(opts)` is a factory a workflow author calls.
- The agent contract requires `ctx.transit()` exactly once; otherwise
  `SessionRuntime.onRunSettled` fails the session (`agent_did_not_transit`) and
  **parks the task to `human`** (`daemon/core/src/engine/session.ts`).
- Multi-turn chat already works inside one `onRun`: `session.input
  {kind:"steer"|"followup"}` → `engine.sessionInput` → `SessionRuntime.input` →
  agent `onSteer`/`onFollowup` → `pi-agent` forwards into the live `pi` via
  `PiDriver.input` (`daemon/extensions/pi-agent/src/{index,driver}.ts`). When `pi`
  is idle, a followup starts a fresh turn.
- Transcript + subscriptions are session-id based and already kind-agnostic:
  `session.transcript`, `session.subscribe` (per-session tail), and
  `session.subscribeProject` (lifecycle pushes). The GUI already renders a session
  transcript live (`gui/src/features/center/views/SessionView.tsx`,
  `components/Transcript.tsx`).

The only deep coupling to "task/workflow" lives in:
`SessionRuntime` (transit/park/`buildContext`), `engine.dispatch` (scheduler
claim), and `recoverProject` (parks the task on crash). Everything else
(transcript, store, subscriptions, GUI rendering) is already session-centric.

## 3. SDK changes (`daemon/sdk/src`)

### 3.1 `SessionMeta` + transcript header (`types.ts`, `transcript.ts`)

- Add `kind: "task" | "interactive"` to `SessionMeta`.
- Relax `task_id`/`workflow`/`step` to be **empty strings** for interactive
  sessions (keep them non-optional `string` on the wire to avoid churn in the Go
  mirror; `""` is the "unset" sentinel, mirroring how `WorkflowInfo` already uses
  empty/`null`). `agent` stays the registered agent name.
  - Decision point to confirm during impl: `"" ` sentinel vs `string | null`.
    Recommendation: `""` sentinel (smallest blast radius on Go/Tauri mirrors;
    `task_id` is already a plain `string`).
- `SessionHeader` (transcript line 1) gains the same `kind` field; `task_id` etc.
  may be `""`.
- `CreateSessionInput` (`daemon/core/src/store/sessionStore.ts`) gains `kind` and
  tolerates empty `task_id`/`workflow`/`step`.

### 3.2 Agent registry types (`types.ts`)

```ts
/** A registered agent, rendered for registry.agent.* (parallels WorkflowInfo). */
export interface AgentInfo {
  name: string;
  description?: string;
}
```

### 3.3 `AutoskAPI.registerAgent` (`api.ts`, `agent.ts`)

```ts
export interface AgentRegistration {
  name: string;
  description?: string;
  /** The agent definition used for interactive sessions (default options). */
  agent: AgentDefinition;
}

export interface AutoskAPI {
  registerWorkflow(workflow: WorkflowDefinition): void;
  registerAgent(registration: AgentRegistration): void; // NEW
}
```

- `AgentRunContext` gains an interactive discriminator so an agent can branch its
  `onRun` between the transit loop and the chat loop:

```ts
export interface AgentRunContext {
  // ...existing...
  /** "task" = workflow step (must transit). "interactive" = chat (never transits). */
  mode: "task" | "interactive";
}
```

  - In interactive mode, `ctx.transit` is present but rejects (throws
    `"transit is not available in an interactive session"`), and `ctx.tasks` /
    `ctx.workflows` are stub views (see §4.3). An interactive agent must not call
    them; pi-agent's chat loop does not.

### 3.4 Proto-v2 wire surface (`proto.ts`)

Add params/results, the method-map entries, and the `RPC_METHODS` array members
(the file's compile-time `Equal` assertions force all three to stay in sync):

```ts
// registry
"registry.agent.list": { params: ProjectSelector; result: AgentInfo[] };

// session
export interface SessionCreateParams extends ProjectSelector {
  agent: string;           // a registered agent name
}
export interface SessionEndParams extends ProjectSelector { id: string }

"session.create": { params: SessionCreateParams; result: SessionMeta };
"session.end":    { params: SessionEndParams;    result: OkResult };
```

- `SessionMeta` gaining `kind` flows through `session.list/get/transcript`,
  `session-event`, and `session-changed` automatically.

## 4. Daemon core changes (`daemon/core/src`)

### 4.1 Extension registry (`extensions/registry.ts`, `extensions/loader.ts`)

- Add an agent map to `ExtensionRegistry`: `addAgent(source, registration)`,
  `resolveAgent(name): AgentDefinition | undefined`, `getAgentInfo(name)`,
  `listAgents(): AgentInfo[]`. Mirror the existing workflow-registration
  validation (duplicate name → load error surfaced via `project.diagnostics`,
  never crashes the daemon).
- `loader.ts`: extend the `AutoskAPI` it hands extensions with
  `registerAgent: (reg) => registry.addAgent(entry.source, reg)`.

### 4.2 New engine entry points (`engine/engine.ts`)

```ts
/** Creates + dispatches an interactive (taskless) session for a named agent. */
async createInteractiveSession(root: string, agentName: string): Promise<SessionMeta>;

/** Gracefully ends a live interactive session → status "done" (no park). */
async sessionEnd(root: string, sessionId: string): Promise<{ handled: boolean }>;
```

- `createInteractiveSession`:
  - resolve the project + the registered agent (`registry.resolveAgent`); unknown
    agent → `EngineError.invalidParams`.
  - `sessions.create({ id, kind:"interactive", task_id:"", workflow:"", step:"",
    agent: agentName, cwd: project.root, timestamp })`.
  - build an **interactive** `SessionRuntime` (see §4.3), register it in
    `this.running`, push to the queue, `emitSession(..., "status")`, `pump()`.
  - The scheduler (`scanProject`) is **not** involved — interactive sessions are
    dispatched directly here, never claimed by a task scan.
- `sessionEnd` routes to `SessionRuntime.end()` (graceful done). `session.input`
  and `session.abort` already route through `this.running`, so they work
  unchanged for interactive runtimes.

### 4.3 SessionRuntime: interactive mode (`engine/session.ts`)

Two viable shapes — **recommendation: a thin mode flag on `SessionRuntime` with
guarded task-coupled branches**, because the chat path reuses ~90% of the runtime
(AbortController, TranscriptWriter, `settleOnce`, `spawn`, steer/followup routing,
queued→running claim). A fully separate class would duplicate all of that.

Changes:

- `SessionRuntimeInit` becomes a union: the existing task/workflow fields OR
  `{ kind:"interactive", agent, agentName }` with no `workflow`/`taskId`/`step`.
  Internally store `kind`; `wf` becomes nullable.
- `run()`:
  - No isolation (interactive has no `wf.isolation`) → `cwd = projectRoot`.
  - Same atomic `queued→running` claim via `patchMetaIf`.
  - `buildContext()` sets `mode:"interactive"`, `transit` → throws, `tasks` →
    a stub `TasksAPI` whose methods reject (`"no task in an interactive
    session"`), `workflows` → a stub `WorkflowsAPI`
    (`current = { workflow:"", step: agentName, targets: [] }`, `get/list` proxy
    the registry). These stubs keep the `AgentRunContext` shape intact without a
    real task/workflow.
  - Call `agent.onRun(ctx)`.
- `onRunSettled` (interactive): returning from `onRun` is **normal** (the agent's
  chat loop exits when the signal fires from `end()`/`abort()`). So:
  - if `aborted` → `finalizeAborted()` **without parking** (no task).
  - else (ended or natural return) → new `finalizeDone()`: seal `done`, write
    `autosk:session_end "done"`, **no park, no transit, no isolation**.
- `end()`: graceful close. Set an `ending` flag, fire `controller.abort()` so the
  agent's `await signal` unblocks and `pi` winds down, let `onRun` return, then
  `finalizeDone()`. (We reuse the abort signal as the "stop now" mechanism but
  pick the terminal status by whether `end()` vs `abort()` was called.)
- `finalizeAborted` / `finalizeFailed`: guard the `host.park(...)` call on
  `kind === "task"` (interactive has no task to park). For interactive, an abort
  seals `aborted`, a crash/throw seals `failed` — both without park.

### 4.4 Crash recovery + idle shutdown (`engine/engine.ts`, `rpc/daemon.ts`)

- `recoverProject`: for an interrupted **interactive** session, seal
  `failed: daemon_restart` but **skip** the `park(task)` step (there is no task).
  (v1: interactive sessions are not auto-resumed after a daemon restart; see §8.)
- `hasPendingWork` already counts `queued`/`running` sessions, so a live
  interactive session keeps the daemon from idle-shutting-down mid-chat — correct.
  Document that an idle (waiting-for-user) interactive session also holds the
  daemon open; acceptable (it is an open session). No change needed.

### 4.5 RPC handlers (`rpc/daemon.ts`)

- `registry.agent.list` → `handle.extensions.listAgents()`.
- `session.create` → `engine.createInteractiveSession(root, reqString(o,"agent"))`;
  map unknown-agent to `INVALID_PARAMS`/`NOT_FOUND`.
- `session.end` → `engine.sessionEnd(root, id)`; `{ ok: handled }`.
- `session.input` already exists; no change (a followup on an idle interactive
  session starts a fresh `pi` turn via the driver).

## 5. pi-agent: interactive mode (`daemon/extensions/pi-agent/src/index.ts`)

- The default extension factory (currently a no-op) registers a named agent:

```ts
export default function piAgentExtension(autosk: AutoskAPI): void {
  autosk.registerAgent({
    name: "pi",
    description: "Interactive chat backed by `pi --mode rpc`.",
    agent: piAgent(), // default options (model from pi's own defaults)
  });
}
```

- `piAgent().onRun` branches on `ctx.mode`:
  - `"task"` → existing `runTurns(...)` (transit loop). Unchanged.
  - `"interactive"` → new `runChat(ctx, driver)`:
    - spawn `pi --mode rpc` (no `autosk_transit` extension needed; or keep it
      harmlessly — but since transit throws, prefer **not** injecting it in chat
      mode to avoid offering a dead tool). Add a `buildPiCommand` flag to skip the
      transit extension when interactive.
    - **Do not** send an initial prompt (empty session). Register the driver in
      `liveSessions` (already done before the first await), then `await` a promise
      that resolves when `ctx.signal` fires. Each composer message arrives via
      `onFollowup` → `driver.input("followup", msg)`; idle → fresh `pi` turn,
      streaming → `follow_up`. Transcript mirroring is unchanged.
    - On signal: return (the runtime seals `done` for an `end`, `aborted` for an
      `abort`). `onAbort` already calls `driver.shutdown()`.

## 6. Go mirror (`internal/daemon/api`) — minimal, no feature work

Front-end scope is GUI-only, but `proto.ts` is the single source of truth the Go
view types mirror, and the daemon has a method-list conformance check. To avoid
drift:

- Add `Kind` to `api.SessionMeta` (additive; `task_id` already `string`).
- Mirror the new methods/types only as far as needed to keep the Go build + any
  RPC-method conformance test green (`internal/daemon/api`, `internal/daemon/
  rpcclient`). No TUI/CLI verbs or keybindings in v1.
- **Verify**: search for a Go-side list that must equal `RPC_METHODS` (e.g. a
  proto conformance test) and update it; otherwise this is purely additive.

## 7. Tauri GUI (`gui/src`) — the v1 user-facing feature

Mirror the `NewTaskModal` create flow and reuse the existing session rendering.

- **IPC** (`services/ipc.ts`): add `agentList(cwd)`, `sessionCreate(cwd, agent)`,
  `sessionEnd(cwd, id)`. (`daemonRequest` generic forwarder already exists; just
  add typed wrappers.)
- **Sessions panel** (`features/sessions/components/SessionsPanel.tsx`): add a
  `+` button in the panel header → opens `NewSessionModal`.
- **NewSessionModal** (new, modeled on `NewTaskModal.tsx`): one dropdown populated
  from `agentList(cwd)`; on confirm → `sessionCreate(cwd, agent)` → upsert into
  state → select the session (so `SessionView` opens and subscribes). No
  title/description, no first-message field (empty session).
- **Composer** (`features/center/components/ComposerInput.tsx` +
  `state/selectors.ts` `composerMode`): add a `"chat"` mode for a selected
  **interactive** session in `queued|running` status → submit sends
  `sessionInput(cwd, id, text, "followup")` (followup = new turn when idle). The
  existing `"steer"` mode for workflow sessions is unchanged.
- **SessionView** (`features/center/views/SessionView.tsx`): for `kind ===
  "interactive"`, render an **End** action (calls `sessionEnd`) where a workflow
  session renders **Abort**. Header omits `task_id`/`workflow:step` for
  interactive sessions (they are `""`); show the agent name instead.
- **State** (`state/types.ts`, `reducer.ts`, `selectors.ts`): `SessionMeta`
  already normalized by id; add `kind` to the mirrored type. `session/upsert` +
  `sessionOrderByProject` already handle a newly created session;
  `session-changed` (project subscription) delivers status flips. No new slice.

## 8. Forward-compat: "continue a completed workflow session as interactive"

Out of scope for v1, but the design stays compatible:

- Interactive sessions reuse the same `Session` entity + pi-format transcript, so
  a future "continue" can spawn an interactive session seeded from a prior
  session's transcript / `pi` session resume.
- Reserve (do not implement yet) an optional `origin_session_id?: string` on
  `SessionMeta`/`SessionCreateParams` so a continued chat can reference the
  workflow session it grew out of (and optionally its `task_id`). Adding it later
  is additive.
- pi-agent already supports resuming a turn loop; a future `runChat` can accept a
  `resumeFrom` (pi session id / transcript path). Keep `runChat` signature open to
  an options bag.

## 9. Edge cases / decisions to honor during impl

- **Empty `task_id` in the index**: `sessionStore.byTask` will bucket interactive
  sessions under `""`. No real task has id `""`, and the scheduler never queries
  `hasLiveSession("")`, so this is inert. `session.list` (no `task_id`) returns
  all metas (interactive included) — desired for the project panel.
- **`session.input` when idle**: returns `{handled:true}` only if `onFollowup`
  delivered. pi-agent registers the driver before the first await, so the very
  first composer message is delivered (starts a turn). Confirm the GUI shows a
  pending/streaming state.
- **End vs Abort**: `end()` → `done`; `abort()` → `aborted`. Both must not park
  (no task). Keep `session.abort` working for interactive (force-stop) even though
  the GUI primary action is End.
- **No `autosk_transit` tool in chat mode**: skip injecting it so the model is not
  offered a tool that throws.

## 10. Milestones

1. **SDK**: `SessionMeta.kind`, `AgentInfo`, `AgentRegistration`,
   `registerAgent`, `AgentRunContext.mode`, proto methods/types + array entries.
   `bun run typecheck` (the proto `Equal` asserts) is the gate.
2. **Core daemon**: registry agent map + loader; `createInteractiveSession`,
   `sessionEnd`; `SessionRuntime` interactive mode (`finalizeDone`, guarded
   park, stub context); recovery skip-park; RPC handlers. `bun test`.
3. **pi-agent**: register `"pi"`; `runChat`; `buildPiCommand` skip-transit flag.
   `bun test` (driver state-machine tests already exist — extend for idle-first).
4. **Go mirror**: additive `Kind` + method conformance. `make build` / `go test`.
5. **GUI**: ipc wrappers, `NewSessionModal`, `+` button, composer `"chat"` mode,
   `SessionView` End button + interactive header. `npm run typecheck && npm test`.
6. **Docs + changelog**: `docs/daemon.md` / `docs/workflows.md` (agent registry +
   interactive sessions), `gui/README.md`, and a `CHANGELOG.md` `[Unreleased]`
   entry (Added: interactive sessions, agent registry, `session.create` /
   `session.end` / `registry.agent.list`).

## 11. Testing

- **Core**: a fake registered agent whose `onRun` (interactive) waits on
  `ctx.signal`; assert `createInteractiveSession` → `running`, `session.input`
  delivers, `sessionEnd` → `done` (no park, no task touched), `abort` → `aborted`,
  crash recovery → `failed` without park. Assert `ctx.transit()` throws in
  interactive mode.
- **pi-agent**: extend the `PiDriver` tests for "idle first message starts a
  turn"; assert `runChat` sends no initial prompt and forwards followups.
- **proto**: the `bun run typecheck` `Equal` assertions cover method/notification
  no-drift; add a daemon test that `registeredMethods()` includes the new methods.
- **GUI**: reducer/selector tests for `composerMode` returning `"chat"` for an
  interactive session; `ipc` wrapper tests.

## 12. Out of scope (explicit follow-ups)

- autosk-as-tools surface for the model (create/list task, comment, enroll).
- Agent params/model selection in the create modal (param schema).
- TUI + CLI parity (`+` in the lazy Sessions panel; `autosk session new`).
- Resuming/continuing a completed workflow session as interactive (§8).
- Auto-resume of interactive sessions across a daemon restart.
