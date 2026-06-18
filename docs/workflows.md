# Workflows & agents

A **workflow** is a directed graph of **steps**; each step is owned by an
**agent** (or by a human). The daemon (`autoskd`) drives a task through its
workflow: it schedules the current step's agent, runs it, and follows the
transition the agent (or a human) chooses.

In v2, **workflows and agents are code** — TypeScript registered by
[extensions](extensions.md), not JSON loaded into a database and not npm-package
"agents" you install. The engine knows nothing about graphs, visit caps, or
prompts; it only drives the task status machine and calls the hooks your code
defines. This page is the reference for the contracts your extension registers
(from [`@autosk/sdk`](../daemon/sdk/src)) and the CLI verbs that drive them.

> **Clean break from v1.** There is no in-place workflow editor and no
> agent-package installer — editing a workflow means editing its extension code,
> and agents are code too (each step's agent is declared **inline** in the
> workflow). `autosk workflow` is now a **read-only** view of what the project's
> extensions registered.

## The task status machine

A task has one of five statuses (unchanged from v1):

| Status | Meaning |
| --- | --- |
| `new` | Open work, not enrolled in a workflow. |
| `work` | Enrolled and owned by the engine — an agent is (or will be) on it. |
| `human` | Parked, waiting for a person (a `human` step, a park, or a failure). |
| `done` | Completed. |
| `cancel` | Abandoned. |

`autosk ready` returns the *ready set*: `new` tasks with no open blocker. That's
what humans and agents pull from. Enrolling a task moves it to `work`; the engine
takes over from there.

## Workflow definitions

A workflow is a [`WorkflowDefinition`](../daemon/sdk/src/workflow.ts):

```ts
interface WorkflowDefinition {
  name: string;                       // unique within a project's registry
  description?: string;
  firstStep: string;                  // the step a freshly-enrolled task enters
  steps: Record<string, StepDef>;     // step name → definition
  onTransit?(ctx: TransitContext, to: StepTarget): void | Promise<void>;
  isolation?: IsolationProvider;      // optional, pluggable (see "Isolation")
}

// A step is EITHER an inline agent OR a terminal/park status step.
// Discriminated structurally: an AgentDefinition has `onRun`; a StatusStep
// has `status`. The step key is the agent name.
type StepDef = AgentDefinition | StatusStep;

interface StatusStep {
  status: "done" | "cancel" | "human";   // build one with statusStep(...)
}

type StepTarget =
  | { step: string }                              // a sibling step (self-loop allowed)
  | { status: "done" | "cancel" | "human" };      // a terminal / park status
```

- **`firstStep`** is where `task.enroll {workflow}` lands the task.
- An **agent step** is an inline [`AgentDefinition`](#agent-definitions) (it has
  an `onRun`): the daemon schedules it and runs that agent's `onRun`. The step
  key is the agent's name.
- A **status step** (`statusStep(...)`) is one the engine never schedules:
  entering it moves the task to that status. `statusStep("human")` parks the task
  and waits for a person to `resume` it; `statusStep("done")` /
  `statusStep("cancel")` close the task (recording the step it ended on).

A `statusStep` is built with the SDK helper:

```ts
import { statusStep } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";

steps: {
  dev:     piAgent({ firstMessageFile: ".../dev.md" }),  // agent step (name "dev")
  accept:  statusStep("human"),                          // park for a human
  shipped: statusStep("done"),                           // close as done
}
```

### `onTransit` — the only graph authority

`onTransit` is called by the engine for **every** transition — enroll →
`firstStep`, step → step, step → terminal/park, and `resume --to`. Throw (or
return a rejected promise) to **reject** a transition; an absent `onTransit`
allows everything. This is where you put graph shape, guards, and visit caps —
the engine has no opinion of its own.

```ts
interface TransitContext {
  taskId: string;
  workflow: string;                   // the workflow this transition belongs to
  step: string;                       // the step the task is leaving
  visits(step: string): number;       // entries into `step` so far (from metadata.step_visits)
  tasks: TasksAPI;                    // live task access (re-reads the store)
  comment(text: string): Promise<void>;
}
```

`visits(step)` is the convenience for the common `max_visits` pattern. For
example, to cap how many times a task can bounce back to `dev`:

```ts
onTransit(ctx, to) {
  if ("step" in to && to.step === "dev" && ctx.visits("dev") >= 5) {
    throw new Error("dev bounced back too many times — park it");
  }
}
```

**How the count is kept.** `visits(step)` reads a **persistent** counter the
engine maintains in the task's [`metadata.step_visits`](daemon.md#task-metadata)
map — it no longer counts session files. The engine increments
`metadata.step_visits[step]` by one on **every transition INTO a named step**:
enroll → `firstStep`, a step→step `transit`, and a `resume` into a step
(including a *bare* `resume`, which re-enters the parked step). A `{ status }`
target (a `done` / `cancel` / `human` flip) and the administrative `reopen` /
park do **not** count. The bump commits atomically with the position write.

The count carries **prior-entries** semantics: `onTransit` runs *before* the
bump, so inside the hook `ctx.visits(target)` is the number of times the task
entered `target` **before** this transition (the cap above fires on the 6th
bounce when the threshold is `5`). Because the counter lives in human-editable
metadata, it is **resettable**: `autosk metadata unset <id> step_visits` (or
`metadata set <id> step_visits.dev 0`) lets a capped task proceed again — the
escape hatch for a task that legitimately needs more passes.

When `onTransit` throws on an agent-chosen target, the error propagates back to
the agent, which may retry with a different target (the `@autosk/pi-agent`
extension turns this into a corrective "kickback" message). If the agent never
produces an accepted transition, the task is parked to `human`.

## Agent definitions

An agent is an [`AgentDefinition`](../daemon/sdk/src/agent.ts) — an inline step
value. There is no `name` field and no separate agent registry: the **step key
is the agent name**, and registering the workflow registers its agents.

```ts
interface AgentDefinition {
  onRun(ctx: AgentRunContext): Promise<void>;
  onSteer?(ctx: AgentRunContext, message: string): Promise<void>;
  onFollowup?(ctx: AgentRunContext, message: string): Promise<void>;
  onAbort?(ctx: AgentRunContext): Promise<void>;
}
```

`onRun` executes **one full step** in-process and **MUST call `ctx.transit(...)`
exactly once** before returning. Returning without a transit fails the session
(`error="agent_did_not_transit"`) and parks the task to `human`. The engine has
no pi knowledge — pi-based agents are an extension on top of `ctx.spawn` +
`ctx.transit` (see [`@autosk/pi-agent`](../daemon/extensions/pi-agent/README.md)).

### The run context

`onRun` receives an [`AgentRunContext`](../daemon/sdk/src/agent.ts):

```ts
interface AgentRunContext {
  session: { id: string };
  mode: "task" | "interactive";       // "task" = workflow step (must transit); "interactive" = chat (never transits)
  cwd: string;                        // run dir: project root, or the isolation handle's path
  projectRoot: string;                // canonical project root (`.autosk/`), regardless of isolation
  signal: AbortSignal;                // fired on abort / daemon shutdown

  tasks: TasksAPI;                    // live task access (current/get/list/comments)
  workflows: WorkflowsAPI;            // live registry + current { workflow, step, targets }
  log: TranscriptAPI;                 // pi-format transcript writer (message / custom)
  partial(message: TranscriptMessage): void;  // ephemeral live snapshot (NOT persisted)

  comment(text: string): Promise<void>;    // shorthand: comment on the current task
  transit(to: StepTarget): Promise<void>;  // validate via onTransit, then commit (once)

  exec(cmd: string[], opts?): Promise<ExecResult>;  // one-shot child process
  spawn(cmd: string[], opts?): ChildHandle;         // long-lived interactive child
}
```

- **`transit`** resolves the target → calls `workflow.onTransit` → atomically
  updates `task.json` → fires the isolation lifecycle → emits notifications. A
  second `transit` in the same session throws.
- **`log`** writes the pi-format transcript: `log.message(...)` for a pi message
  entry, `log.custom(type, data)` for the generic logging channel.
- **`partial`** streams an in-progress assistant message snapshot to live
  subscribers. It is **ephemeral**: never written to the transcript, carries no
  line, never advances the line cursor, and is superseded by the next committed
  `log.message`. Send the full **cumulative** snapshot each time — the client
  just replaces its current partial. It rides the same per-session subscription
  as committed lines; see [docs/daemon.md → Streaming partial
  messages](daemon.md#streaming-partial-messages) for the wire frame and the
  ordering/persistence guarantees. (`@autosk/pi-agent` drives this from pi's
  `message_update` events.)
- **`exec`** / **`spawn`** run child processes; `spawn` is how the pi-agent
  extension drives `pi --mode rpc` over JSON-lines stdio.
- **`cwd` vs `projectRoot`:** `cwd` is where the agent runs — under worktree
  isolation a throwaway worktree with no `.autosk/`. `projectRoot` always points
  at the original project. `@autosk/pi-agent` passes it to the spawned pi as
  `AUTOSK_CWD`, which the `autosk` CLI honors as its project selector, so any
  `autosk` call the agent makes (e.g. the `@autosk/pi-tools` `autosk_task` /
  `autosk_comment` tools) targets the task's own project rather than walking up
  from the worktree.
- **`onSteer`** / **`onFollowup`** receive a `session.input` message on a live
  session; **`onAbort`** runs on `session.abort`. All are optional.

### Sessions

One `onRun` for one task step = one **session** (`./.autosk/sessions/<id>.json`
meta + `<id>.jsonl` transcript). Sessions replace v1's jobs; see
[docs/daemon.md → Sessions & transcripts](daemon.md#sessions--transcripts) for
the on-disk format and the steer/abort surface.

## Named agents & interactive sessions

A workflow registers its agents **inline** (the step key is the agent name). An
extension can also publish a **named, standalone agent** so a user can chat with
it directly, outside any workflow:

```ts
import { type AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";

export default function (autosk: AutoskAPI) {
  autosk.registerAgent({
    name: "pi",
    description: "Interactive chat backed by `pi --mode rpc`.",
    agent: piAgent(),          // an AgentDefinition, run with its default options
  });
}
```

A registered agent backs an **interactive (taskless) session** — a chat the user
opens directly (no task, no workflow). The engine runs the same
`AgentDefinition.onRun`, but with `ctx.mode === "interactive"`:

- `ctx.transit(...)` is **unavailable** (it throws) and `ctx.tasks` /
  `ctx.workflows` are stub views — an interactive agent must not touch them.
- The agent runs a chat loop and **returns when `ctx.signal` fires** (rather than
  transiting). Returning is normal: a graceful end seals the session `done`, an
  abort seals it `aborted`, a crash seals it `failed` — **none park a task**
  (there is none). The `agent_did_not_transit` failure does not apply.

`@autosk/pi-agent` registers the `"pi"` agent and branches `onRun` on `ctx.mode`:
`"task"` runs the workflow transit loop; `"interactive"` runs a chat loop that
spawns `pi --mode rpc` **without** the `autosk_transit` tool (transit is not
offered in chat) and forwards each composer message as a follow-up.

See [docs/daemon.md → Interactive sessions](daemon.md#interactive-taskless-sessions)
for the session lifecycle and the `registry.agent.list` / `session.create` /
`session.end` RPC surface.

## Isolation (pluggable, per workflow)

Worktree isolation is a pluggable provider attached to a workflow (the
sandcastle pattern), not a hard-wired engine feature. Isolation is scoped to a
task's **active run** — the contiguous time it spends in `work` — and driven by
the task's status machine, **not** by step-session boundaries:

```ts
interface IsolationProvider {
  tag: string;                        // "worktree" | "none" | future: "docker", …
  // ensure-ready (create | start | reuse). Mandatory; idempotent + recovery-safe.
  acquire(ctx: { projectRoot: string; taskId: string }): Promise<IsolationHandle>;
  // quiesce-on-exit: stop a LIVE env but keep it cheaply resumable. No
  // destruction here (that is `reap`), so NO `terminal`/`force`. Optional — a
  // provider with nothing to stop (e.g. worktree) omits it entirely.
  release?(handle: IsolationHandle): Promise<void>;
  // destroy-on-terminal, keyed by (projectRoot, taskId) so it needs no live
  // handle. `force: false` leaves a dirty env in place and reports
  // `{ dirty: true }`; `force: true` removes it regardless (branches preserved).
  // Optional: a provider with no out-of-band identity omits it (caller skips reaping).
  reap?(ctx: { projectRoot: string; taskId: string }, opts: { force: boolean }):
    Promise<{ removed: boolean; dirty: boolean; detail?: string }>;
}
interface IsolationHandle { cwd: string; meta?: Record<string, unknown> }
```

The environment moves through a small state machine, advanced by the task's
status transitions:

```text
 ABSENT ──acquire(create)──▶ RUNNING ──release(stop)──▶ DORMANT
                               ▲   │                        │
                               │   └──acquire(reuse)         │
                               └────acquire(resume/start)────┘
                            (any) ──reap(force)──▶ GONE
```

The three methods map to three distinct roles:

- **`acquire` = ensure-ready** (create | start | reuse). Mandatory. Called on
  entering `work` (enroll / resume) and re-entered **per step**; MUST be
  idempotent and recovery-safe (create when ABSENT, resume when DORMANT, re-use
  when RUNNING). The returned `cwd` becomes the session's `ctx.cwd`.
- **`release` = quiesce-on-exit** (optional). Called **only when the task LEAVES
  `work`** — a `human` park, or a `done`/`cancel` terminal. It stops a live env
  but keeps it cheaply resumable; it carries no `terminal`/`force` and performs
  no destruction. It **NEVER fires on step→step**. A provider with nothing to
  stop (keeping the dir IS the absence of teardown) omits it entirely.
- **`reap` = destroy-on-terminal** (optional). Called **only on a TERMINAL
  transition** (`done`/`cancel`), keyed by `(projectRoot, taskId)` so it works
  with no live handle. `force: false` refuses to discard uncommitted changes and
  reports `{ dirty: true }`; `force: true` removes the env regardless (branches
  the env created are always preserved).

The engine `acquire`s before scheduling each session (per step). On a step→step
transition it does **nothing** — the env stays RUNNING. On a `human` park it
calls `release` only; on `done`/`cancel` it calls `release` then `reap({force:
true})`. A provider failure parks the task to `human` with the provider's
message.

A **manual** terminal (a `task.done`/`task.cancel` RPC issued while no session is
live — e.g. after a human-park) has no live handle to `release`, so the daemon
calls `reap` only to clean up an env a prior step left on disk. By default `reap`
refuses to discard uncommitted changes (the verb is rejected with
`ENVIRONMENT_DIRTY`); pass `--force` (`autosk done -f` / `cancel -f`, or the
TUI/GUI force-confirm prompt) to remove the env and discard them — the branch is
always preserved.

The shipped [`worktreeIsolation()`](../daemon/extensions/worktree/README.md)
provider runs each task in its own git worktree at
`~/.autosk/worktrees/<slug>/<task-id>` on branch `autosk/<task-id>`. It **omits
`release`** (keeping the checkout on disk across sibling/human-park steps is
exactly the absence of teardown) and implements `reap` to remove the worktree on
a terminal transition while **preserving the branch** (so the work survives for
review/merge). Attach it with `isolation: worktreeIsolation()`; a workflow
without an `isolation` field runs every step in the project root (`tag:
"none"`).

## Driving a workflow from the CLI

```bash
# Create, optionally enrolling at the same time.
autosk create "Wire up the auth flow"
autosk create "Fix the flaky test" --workflow feature-dev   # create + enroll

# Enroll an existing task.
autosk enroll <id> --workflow feature-dev   # at the workflow's firstStep

# Resume a parked (human) task back into its workflow.
autosk resume <id>                 # at the current step
autosk resume <id> --to review     # at a chosen step (or done|cancel|human)

# Human status overrides (administrative — do NOT run onTransit).
autosk done <id>
autosk cancel <id>
autosk reopen <id>
```

`enroll` (and `create --workflow`) takes a `--workflow` target only — a task is
always enrolled into a named workflow at its `firstStep`.

### Read-only registry views

Workflows come from code, so the CLI only *shows* them:

```bash
autosk workflow list          # workflows registered by this project's extensions
autosk workflow show <name>   # steps (KIND: agent | done|cancel|human), targets, isolation
```

`workflow show` renders each step with a `KIND` column: `agent` for an inline
agent step (its `name` is the agent name), or the terminal/park status
(`done` / `cancel` / `human`) for a `statusStep`.

If a workflow you expect is missing, check `autosk project diagnostics` — a
broken extension (or an invalid step shape) records a load error there instead
of crashing the daemon (see [docs/extensions.md](extensions.md#error-isolation--projectdiagnostics)).

## The reference workflow: `feature-dev`

[`@autosk/feature-dev`](../daemon/extensions/feature-dev/README.md) is an npm
package the daemon installs on first run (see
[docs/extensions.md → First-run bootstrap](extensions.md#first-run-bootstrap)),
so every project can enroll into it with no per-project files:

```text
dev ──▶ review ──▶ docs ──▶ validator ──▶ accept (human)
 ▲        │                    │
 └────────┴────────────────────┘   (review→dev and validator→dev bounce-backs)
```

| Step | Kind | Notes |
| --- | --- | --- |
| `dev` | agent (`piAgent`) | implements the task (first step) |
| `review` | agent (`piAgent`) | thorough review; can bounce back to `dev` |
| `docs` | agent (`piAgent`) | documentation pass |
| `validator` | agent (`piAgent`) | independent verification; can bounce back to `dev` |
| `accept` | `statusStep("human")` | the engine parks here for final acceptance |

- **Isolation:** `worktreeIsolation()` — each task runs in its own git worktree,
  so the project root must be a git repo.
- **Visit cap:** `onTransit` rejects a bounce-back into `dev` once the task has
  entered `dev` 5 times (via `ctx.visits("dev")`), so a task that keeps failing
  review/validation parks for a human instead of looping forever. The count is
  the persistent, human-resettable [`metadata.step_visits.dev`](daemon.md#task-metadata)
  — clear it with `autosk metadata unset <id> step_visits` to let a parked task
  resume bouncing through `dev`.

```bash
id=$(autosk create "Fix the flaky test" --workflow feature-dev --json | jq -r .id)
# the daemon (auto-spawned) picks it up, runs dev → review → …, and either
# parks it at `accept` for you or bounces it back to `dev`.
```

### Customising it

Copy the extension into `~/.autosk/extensions/` (or your project's
`.autosk/extensions/`) and edit the `piAgent({...})` / `featureDevWorkflow({...})`
calls (model, thinking, the visit cap, the step graph, the prompts under
`prompts/`). Because a project/global extension overrides an npm one by name,
your `feature-dev` then replaces the provisioned one. See
[docs/extensions.md → Writing your own](extensions.md#writing-your-own) for the
discovery/override rules.

## Make your own workflow

A minimal two-step flow with a human gate:

```ts
// ~/.autosk/extensions/my-flow.ts
import { statusStep, type AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { worktreeIsolation } from "@autosk/worktree";

export default function (autosk: AutoskAPI) {
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key is the agent name; registering the workflow registers
      // its inline agents — there is no separate registerAgent call.
      dev: piAgent({
        model: "sonnet:high",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      accept: statusStep("human"),
    },
    onTransit(ctx, to) {
      // gate, count, or comment here; throw to reject a transition.
    },
    isolation: worktreeIsolation(),
  });
}
```

```bash
autosk workflow list                 # my-flow should appear
autosk enroll <task-id> --workflow my-flow
```

For the full extension lifecycle (discovery order, overrides, diagnostics, the
no-trust loading model), see [docs/extensions.md](extensions.md).
