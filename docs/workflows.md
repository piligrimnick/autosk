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
  visits(step: string): number;       // how many times the task has entered `step`
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
  cwd: string;                        // run dir: project root, or the isolation handle's path
  projectRoot: string;                // canonical project root (`.autosk/`), regardless of isolation
  signal: AbortSignal;                // fired on abort / daemon shutdown

  tasks: TasksAPI;                    // live task access (current/get/list/comments)
  workflows: WorkflowsAPI;            // live registry + current { workflow, step, targets }
  log: TranscriptAPI;                 // pi-format transcript writer (message / custom)

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

## Isolation (pluggable, per workflow)

Worktree isolation is a pluggable provider attached to a workflow (the
sandcastle pattern), not a hard-wired engine feature:

```ts
interface IsolationProvider {
  tag: string;                        // "worktree" | "none" | future: "docker", …
  acquire(ctx: { projectRoot: string; taskId: string }): Promise<IsolationHandle>;
  // Session-bound cleanup for a LIVE handle. `terminal: true` on done/cancel;
  // the engine passes `force: true` so it always reaps on a terminal transition.
  release(handle: IsolationHandle, opts: { terminal: boolean; force: boolean }): Promise<void>;
  // Session-FREE cleanup, keyed by (projectRoot, taskId) — used when a task
  // reaches a terminal status OUTSIDE the engine (a manual done/cancel after a
  // human-park, where no live handle exists). `force: false` leaves a dirty env
  // in place and reports `{ dirty: true }`; `force: true` removes it regardless.
  reap?(ctx: { projectRoot: string; taskId: string }, opts: { force: boolean }):
    Promise<{ removed: boolean; dirty: boolean; detail?: string }>;
}
interface IsolationHandle { cwd: string; meta?: Record<string, unknown> }
```

The engine `acquire`s before scheduling a session (the returned `cwd` becomes
`ctx.cwd`) and `release`s on every transition (`terminal: true` on done/cancel).
On a provider failure it parks the task to `human` with the provider's message.

A **manual** terminal (a `task.done`/`task.cancel` RPC issued while no session is
live — e.g. after a human-park) runs no `release`, so the daemon calls `reap`
instead to clean up an env a prior step left on disk. By default `reap` refuses
to discard uncommitted changes (the verb is rejected with `ENVIRONMENT_DIRTY`); pass
`--force` (`autosk done -f` / `cancel -f`, or the TUI/GUI force-confirm prompt) to
remove the env and discard them — the branch is always preserved.

The shipped [`worktreeIsolation()`](../daemon/extensions/worktree/README.md)
provider runs each task in its own git worktree at
`~/.autosk/worktrees/<slug>/<task-id>` on branch `autosk/<task-id>`, preserving
the branch on terminal release/reap (so the work survives for review/merge) and
keeping the checkout across sibling/human-park steps. Attach it with
`isolation: worktreeIsolation()`; a workflow without an `isolation` field runs
every step in the project root (`tag: "none"`).

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
  review/validation parks for a human instead of looping forever.

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
