# autoskd v2 — Bun/TypeScript daemon, extension-driven workflows, file-based tasks

Status: draft / discussion
Date: 2026-06-12

## 1. Goals

1. **Fewer concepts.** Collapse the engine's internal vocabulary (runs,
   step_signals, step_transitions, kickbacks, synthetic workflows, agent
   packages, dolt audit, metadata.step_visits, …) into a minimal set:
   *Task, Comment, Session, Workflow, Agent, Extension, Project*.
2. **User-extensible workflows.** Workflows and agents become **code**
   registered by extensions, loaded the same way pi loads its extensions
   (project-local dirs, global dirs, npm packages). The engine stops
   interpreting workflow JSON; it only drives the task status machine and
   calls hooks.

Non-goals: migrating existing `.autosk/db` projects (clean break, v2),
keeping wire compatibility with `autosk-proto` v1.

## 2. Decisions taken (discussion summary)

| Decision | Choice |
|---|---|
| Daemon runtime | Bun + TypeScript app, replaces the Rust `autoskd` (and `autosk-core`, `autosk-proto`, doltlite) entirely |
| Storage | Files under `.autosk/` — no database |
| File ownership | **Hybrid**: the daemon is the writer for all RPC-driven mutations, but it picks up external (human/script) edits via startup scan + fs watcher |
| Task format | `tasks/<id>/task.json` + `tasks/<id>/comments.jsonl` |
| Agent execution | **In-process**: `onRun` is an async function inside the daemon. Core has **no** pi knowledge — `spawnPi` is gone from core; pi-based agents ship as an extension |
| Protocol | **New JSON-RPC v2**, designed for the simplified model; Go CLI / lazy / Tauri GUI are adapted to it |
| Kept features | worktree isolation (moved into workflow as a pluggable isolation module, sandcastle-style), task deps (blocks/blocked_by), comments as agent channel (editable/deletable later, not strictly append-only), multi-project daemon, steer/input into live sessions |
| Dropped from tasks | `priority`, `author`, `metadata` (incl. `step_visits`) |
| Dropped concepts | step_signals, step_transitions table, steps table, workflows table, agents table, daemon_runs table, dolt commits/audit, compactor/GC, kickback/corrections loop, `max_visits`, `single:<agent>` synthetic rows, agent npm-package registry (`autosk agent install`), per-step `agent.params`, prompt_rules |
| Crash recovery | Sessions interrupted by a daemon restart are marked `failed: daemon_restart`; the task is **parked to `human`** |
| Migration | Clean break: old projects stay on the old binary; no migrator |

## 3. Concept model (v2)

### 3.1 Task

`./.autosk/tasks/<id>/task.json`:

```jsonc
{
  "id": "ask-3f9b2c",
  "title": "Implement auth module",
  "description": "…",
  "status": "work",              // new | work | human | done | cancel  (unchanged enum)
  "workflow": "feature-dev",      // workflow name from the registry; null when status=new
  "step": "review",               // current step name; null unless enrolled
  "blocked_by": ["ask-aa11bb"],   // dependency edges (blocks is derived)
  "created_at": "2026-06-12T09:00:00Z",
  "updated_at": "2026-06-12T09:30:00Z"
}
```

Removed vs v1: `priority`, `author_id`, `metadata`. IDs and the
five-status enum survive unchanged. `blocked` remains a derived flag
(open blocker exists), never stored.

`comments.jsonl` — one JSON object per line:
`{ "id": "cm-…", "author": "…", "text": "…", "created_at": …, "updated_at": … }`.
Edit/delete = the daemon rewrites the file atomically (it is the sole
writer in the normal path, so no merge logic is needed). The format
stays a flat list, not an event log.

### 3.2 Session (replaces `daemon_runs` + transcripts + signals)

One invocation of an agent's `onRun` for one task step = one session.

- Meta: `./.autosk/sessions/<session-id>.json` —
  `{ id, task_id, workflow, step, agent, status: queued|running|done|failed|aborted, error?, started_at, ended_at }`
- Transcript: `./.autosk/sessions/<session-id>.jsonl` — the entry
  schema **deliberately mirrors pi's session format** (see
  `pi/packages/coding-agent/docs/session-format.md`, `SessionEntryBase`
  in `src/core/session-manager.ts`) so pi-based agents can pipe pi
  session entries through verbatim and existing pi tooling/renderers
  stay reusable:

  - **Line 1 — header** (pi's `SessionHeader`, autosk fields added):
    `{ "type": "session", "version": 1, "id": …, "task_id": …,
    "workflow": …, "step": …, "agent": …, "timestamp": …, "cwd": … }`
  - **Entries** extend pi's base `{ type, id (8-char hex), timestamp }`.
    We drop `parentId` — transcripts are linear, there is no
    branching/`/tree` here.
  - **`message` entries** reuse pi's message schema unchanged
    (`role`, `content[]` blocks incl. `toolCall`, `usage`/`cost`,
    `stopReason`) — written via `ctx.log.message(…)`.
  - **`custom` entries** reuse pi's `CustomEntry` shape
    (`{ type: "custom", customType, data }`) — written via
    `ctx.log.custom(customType, data)`. This is the generic agent
    logging channel.
  - **Engine structural entries** are autosk-specific custom types the
    engine emits itself: `{ type: "custom", customType:
    "autosk:transit" | "autosk:steer" | "autosk:error" |
    "autosk:session_end", data: … }` — so a transcript is
    self-contained without consulting the meta file.

This is the entire remaining "audit": no dolt commits, no signal rows.
Listing a task's sessions = filter session metas by `task_id` (in-memory
index in the daemon; files are the persistence).

### 3.3 Workflow (code, not data)

```ts
interface WorkflowDefinition {
  name: string;                       // unique within a project's registry
  description?: string;
  firstStep: string;
  steps: Record<string, StepDef>;     // step name → definition
  // Called by the engine for EVERY transition (enroll → firstStep,
  // step → step, step → done/cancel/human, resume --to).
  // Throw / return error to reject the transition.
  onTransit?(ctx: TransitContext, to: StepTarget): void | Promise<void>;
  isolation?: IsolationProvider;      // optional pluggable module, see 3.5
}

interface StepDef {
  agent?: string;                     // agent name from the registry
  human?: boolean;                    // human-owned step: engine parks, never schedules
}

type StepTarget =
  | { step: string }
  | { status: "done" | "cancel" | "human" };
```

The engine itself knows nothing about graphs, visit caps, or prompt
rules. If a workflow wants `max_visits` semantics or transition guards,
it implements them inside `onTransit` (counting in its own state or in
comments — its choice). Default `onTransit` (absent) = allow everything.

The old `single:<agent>` synthetic workflows become a **built-in
workflow factory** shipped with the daemon:
`singleStep(agentName)` → `{ name: "single:<agent>", steps: { do: { agent } }, … }`,
materialised on demand by `task.enroll {agent}` — no persisted rows, no
hidden registry entries.

### 3.4 Agent (code, not packages)

```ts
interface AgentDefinition {
  name: string;
  // Runs one full step. MUST call ctx.transit(...) exactly once before
  // returning; returning without a transit fails the session
  // (error="agent_did_not_transit") and parks the task to human.
  onRun(ctx: AgentRunContext): Promise<void>;
  onSteer?(ctx: AgentRunContext, message: string): Promise<void>;
  onFollowup?(ctx: AgentRunContext, message: string): Promise<void>;
  onAbort?(ctx: AgentRunContext): Promise<void>;
}

interface AgentRunContext {
  session: { id: string };
  cwd: string;                        // project root, or the isolation handle's path
  signal: AbortSignal;                // fired on abort / daemon shutdown

  tasks: TasksAPI;                    // live task access (not a frozen snapshot)
  workflows: WorkflowsAPI;            // live registry access + current position
  log: TranscriptAPI;                 // pi-format transcript writer (see 3.2)

  comment(text: string): Promise<void>;    // shorthand: comment on the current task
  transit(to: StepTarget): Promise<void>;  // validates via workflow.onTransit, then commits

  // One-shot child process.
  exec(cmd: string[], opts?: ExecOptions): Promise<ExecResult>;
  // Long-lived interactive child with stdio streaming — this is how the
  // pi-agent extension drives `pi --mode rpc` over JSON-lines stdio.
  spawn(cmd: string[], opts?: SpawnOptions): ChildHandle;
}

interface TasksAPI {
  currentId: string;                          // the task this session runs for
  current(): Promise<TaskView>;               // re-reads from the store
  get(id: string): Promise<TaskView>;
  list(filter?: TaskFilter): Promise<TaskView[]>;
  comments(id?: string): Promise<Comment[]>;  // default: current task
}

interface WorkflowsAPI {
  current: { workflow: string; step: string; targets: StepTarget[] };
  get(name: string): WorkflowInfo | undefined; // rendered registry view (sync: in-memory)
  list(): WorkflowInfo[];
}

interface TranscriptAPI {
  message(message: TranscriptMessage): void;     // pi message-schema entry
  custom(customType: string, data?: unknown): void;
}

interface ChildHandle {
  stdin: WritableStreamDefaultWriter<Uint8Array>;
  onStdout(cb: (line: string) => void): void;    // line-buffered
  onStderr(cb: (line: string) => void): void;
  kill(signal?: string): void;
  exited: Promise<{ code: number | null }>;
}
```

Notes:

- **No `spawnPi` in core.** The current "standard branch" (spawn
  `pi --mode rpc`, first_message, model/thinking, corrections/kickback
  loop) is reimplemented as a separate extension package
  (`@autosk/pi-agent`, lives in this repo) on top of `ctx.spawn` +
  `ctx.transit`: it drives pi over JSON-lines stdio via `ChildHandle`
  and mirrors pi's session entries into `ctx.log` 1:1 (the transcript
  format is pi's, see 3.2). The kickback loop becomes that extension's
  private logic — the engine no longer has the concept.
- **Transit channel for pi-based agents (decided):** the pi-agent
  extension registers a **pi extension tool** (`autosk_transit`) into
  the spawned pi; the tool call is observed on pi's RPC event stream
  and translated into `ctx.transit(...)` by the pi-agent code. No
  session-scoped daemon RPC is needed; core stays closed.
- `onSteer` / `onFollowup` are invoked when a client calls
  `session.input` on a live session; `onAbort` on `session.abort`.
  All optional; absent → the RPC returns `unsupported_by_agent`.
- `ctx.transit` semantics: resolve target → call `workflow.onTransit`
  (may throw → the error propagates to the agent, which may retry with
  a different target) → atomically update `task.json` + close the
  session on terminal/sibling transitions → fire isolation lifecycle
  hooks → emit notifications. A second `transit` in the same session
  throws.

### 3.5 Isolation modules (sandcastle pattern)

Worktree isolation leaves the engine core and becomes a pluggable
provider attached to a workflow, modelled after sandcastle's tagged
providers + lifecycle wrapper (`SandboxProvider.ts` / `SandboxLifecycle.ts`):

```ts
interface IsolationProvider {
  tag: string;                                        // "worktree", "none", future: "docker", …
  acquire(ctx: { projectRoot: string; taskId: string }): Promise<IsolationHandle>;
  // terminal=true on done/cancel, false on human-park / sibling step
  release(handle: IsolationHandle, opts: { terminal: boolean }): Promise<void>;
}
interface IsolationHandle { cwd: string; meta?: object }
```

The engine's only obligations: call `acquire` before scheduling a
session for an isolated workflow (pass `handle.cwd` as `ctx.cwd`), call
`release({terminal})` on terminal transitions, and park to `human` with
the provider's error on failure. The shipped `worktreeIsolation()`
provider ports today's behaviour (deterministic path under
`~/.autosk/worktrees/…`, branch `autosk/<task-id>`, branch preserved on
terminal, dir re-allocated when missing). All the v1 worktree special
cases (`worktree_stranded`, `--base-ref`, `workflow update --isolation`
safety matrix) collapse into provider-internal logic or disappear
(workflows are code now — "updating isolation" is editing the code).

### 3.6 Extension system (pi-style)

Discovery (priority order, all merged):

1. `./.autosk/extensions/` — project-local (`*.ts`/`*.js`, subdirs with
   `index.ts`, packages with `package.json#autosk.extensions`)
2. `~/.autosk/extensions/` — global
3. npm packages listed in `~/.autosk/settings.json` /
   `./.autosk/settings.json` under `"extensions"` (installed into
   `~/.autosk/packages/` like today's agent packages)

Entry point — default-export factory, mirroring pi:

```ts
import type { AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerAgent(piAgent({ model: "sonnet:high", firstMessageFile: "./prompts/dev.md" }));
  autosk.registerWorkflow({
    name: "feature-dev",
    firstStep: "dev",
    steps: {
      dev:    { agent: "@autosk/pi-agent/dev" },
      review: { agent: "@autosk/pi-agent/review" },
      accept: { human: true },
    },
    onTransit(ctx, to) {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= 5) {
        throw new Error("dev bounced back too many times — park it");
      }
    },
    isolation: worktreeIsolation(),
  });
}
```

**No trust model:** an installed/discovered extension is loaded,
period — no prompt-on-first-load gate (unlike pi). Putting code into
`.autosk/extensions/` or `settings.json` *is* the consent.

Loading: Bun imports TS natively (no jiti needed). Extensions run
**in-process**, per-project registries: each loaded project gets the
union of global + its local extensions; `registerWorkflow` /
`registerAgent` write into that project's registry. Name collision =
load error surfaced via `project.diagnostics`. A broken extension never
takes the daemon down — load errors are caught, recorded, and the rest
of the registry stays usable (pi's `ExtensionRunner.onError` model).

**Live-code hazard** (workflow code changes while tasks are mid-flight):
on project (re)load, every `work`/`human` task is validated against the
registry; unknown workflow or unknown step ⇒ park to `human` with
`error="workflow_missing: …"`. No frozen copies, no versioning — the
registry at daemon start is the truth.

### 3.7 Engine core (what's left)

The daemon shrinks to:

1. **Project manager** — registry (`~/.autosk/projects.json`), walk-up
   resolution by `{cwd}`, lazy open; per-project: file store + extension
   registry + scheduler.
2. **File store** — read/write `task.json` / `comments.jsonl` / session
   files; atomic writes (tmp + rename); in-memory cache keyed by mtime;
   fs watcher + startup scan reconcile **external** edits (hybrid
   ownership). Reconciliation rule: external edits to `title` /
   `description` / `blocked_by` / comments are accepted as-is; external
   edits to `status` / `step` / `workflow` of a task with a live session
   are rejected (file rewritten from engine state, warning logged) —
   the engine owns enrolled tasks.
3. **Scheduler** — the only loop: task with `status=work`, current step
   has an agent (not `human:true`), no live session ⇒ create session,
   run `agent.onRun` through the worker pool (`--workers`, default 4,
   global FIFO across projects, AbortSignal per session). Event-driven
   off transits + a slow safety rescan; no 2s SQL poll.
4. **Session manager** — transcript writer, steer/followup/abort
   routing, subscribe/replay for clients, crash recovery on startup
   (running → `failed: daemon_restart`, task → `human`). **No
   retention/GC logic in this version** — session files accumulate;
   cleanup is left to the human (`rm .autosk/sessions/…`).
5. **RPC server** — JSON-lines over UDS (+ opt-in TCP/token, same
   security model as v1), single-instance bind, auto-spawn-friendly,
   idle-shutdown (same three conditions as v1).

Explicitly deleted: poller SQL, step executor branches, signals,
compactor, dolt GC, migrations, pkg resolver/installer for agents,
`metadata` verb tree, `workflow create/delete/updateIsolation` RPCs
(workflows are code), `agent install/uninstall` (agents are code).

## 4. JSON-RPC v2 (sketch)

Same envelope as v1 (one JSON object per line; `{cwd}` selector on
project-scoped methods; RFC3339 UTC on the wire).

| Domain | Methods |
|---|---|
| meta | `version`, `auth`, `healthz`, `shutdown` |
| project | `list`, `add`, `remove`, `init`, `diagnostics` (extension load errors), `subscribe`/`unsubscribe` |
| task | `list`, `get`, `create`, `update` (title/description), `enroll {workflow | agent}`, `resume {to?}`, `done`, `cancel`, `reopen`, `block`/`unblock`, `comment.add/list/edit/delete`, `subscribe`/`unsubscribe` |
| registry | `workflow.list`, `workflow.get` (rendered from code: steps, targets, isolation tag), `agent.list` |
| session | `list {taskId?}`, `get`, `transcript {fromLine?, limit?}`, `subscribe`/`unsubscribe` (replay-then-tail), `input {message, kind: steer|followup}`, `abort` |

Gone: `sql.*`, `step.next`, `signal.*`, `job.*` (renamed to `session.*`),
`maint.compact`, `task.setPriority`, `workflow.create/delete/updateIsolation`,
`agent.install/uninstall`, `task.setStatus` (covered by the explicit verbs).

Notifications: `task-changed`, `project-changed`, `session-event`
(`message|status|done|error`) — same push model as v1, now fed by
engine events + the fs watcher (covers external file edits too).

## 5. Resolved questions & remaining open points

Resolved during review:

1. **SDK & pi-agent packaging** — both live in **this repo**:
   top-level `daemon/` Bun workspace (`daemon/core`, `daemon/sdk`,
   `daemon/extensions/pi-agent`), published as separate npm packages.
   Rust `crates/` removed once parity lands.
2. **pi-agent transit channel** — register a **pi extension tool**
   inside the spawned pi; observe the tool call over pi's RPC stream
   and translate to `ctx.transit` (see 3.4). Core stays closed.
3. **Session retention** — **not implemented in this version**; no
   knobs, manual cleanup only.
4. **Trust model** — **none**: installed ⇒ available (see 3.6).
5. **Transcript format** — pi's session entry schema reused (header
   line + typed entries + `custom` entries; see 3.2).

Still open:

1. **Comment edit conflicts** under hybrid ownership (human edits
   `comments.jsonl` while the daemon rewrites it) — last-write-wins via
   rename is probably fine; confirm.

## 6. Implementation plan

Phased; each phase lands green and is independently reviewable.

- **P1 — Scaffolding & SDK.** Bun workspace (`daemon/`), `@autosk/sdk`
  types (Task/Session/Workflow/Agent/Isolation/AutoskAPI), proto-v2
  type definitions (single source of truth for Go/Tauri mirrors),
  pi-format transcript entry types.
- **P2 — File store + project manager.** Task/comment/session file
  formats, atomic writes, mtime cache, startup scan + watcher
  reconciliation, projects.json, walk-up resolution. Golden tests for
  the on-disk formats (these files are now the public contract).
- **P3 — Extension loader + registries.** pi-style discovery
  (project/global/npm), factory invocation, error isolation,
  per-project registries, `singleStep` builtin, registry validation of
  in-flight tasks (live-code hazard).
- **P4 — Engine.** Scheduler + worker pool, session lifecycle,
  `ctx.transit` (onTransit validation, atomic commit, isolation
  acquire/release), transcript writer, steer/followup/abort routing,
  crash recovery (park to human), idle-shutdown.
- **P5 — RPC v2 server.** UDS + TCP/token, single-instance,
  subscriptions/replay, `project.diagnostics`. Conformance test suite
  (replaces autosk-proto golden tests).
- **P6 — Shipped extensions.** `worktreeIsolation()` provider (port v1
  behaviour incl. missing-dir recovery), `@autosk/pi-agent` (port the
  standard branch: model/thinking/first_message/extra_args, kickback
  loop as private logic; `autosk_transit` pi-tool + RPC-stream bridge;
  pi session entries mirrored into the transcript), a `feature-dev`
  reference workflow replacing `feature-dev-generic` bootstrap.
- **P7 — Go clients on proto v2.** Regenerate `internal/daemon/api`
  view types, rewire CLI verbs (drop `metadata`, `sql`, `agent
  install`, `workflow create`; add `session`/`registry` verbs), adapt
  lazy panes (sessions instead of jobs). `AUTOSKD_BIN` now points at
  the Bun daemon (compiled via `bun build --compile` for distribution).
- **P8 — Tauri GUI on proto v2.** Mirror wire types in
  `gui/src-tauri`, update ipc/event services and reducers.
- **P9 — Decommission & docs.** Remove `crates/`, doltlite fetch,
  Rust CI; rewrite `docs/daemon.md` / `docs/workflows.md`, add
  `docs/extensions.md`; changelog (`### Changed`/`### Removed` — this
  is maximally user-visible).

Rough dependency graph: P1 → P2 → P3 → P4 → P5 → {P6, P7} → P8 → P9
(P6 can start against P4's ctx API in parallel with P5).
