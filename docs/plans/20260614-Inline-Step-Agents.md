# Inline step agents + `statusStep` — workflow registration redesign

**Status:** plan (not yet started).
**Date:** 2026-06-14.
**Owners:** autosk core.
**Predecessor:** [`20260612-Bun-Daemon-Extensions.md`](20260612-Bun-Daemon-Extensions.md).
**Related code:**
`daemon/sdk/src/{api,workflow,agent,types,proto,index,singleStep}.ts`,
`daemon/core/src/extensions/{registry,loader}.ts`,
`daemon/core/src/engine/{engine,transition}.ts`,
`daemon/core/src/rpc/daemon.ts`,
`daemon/extensions/pi-agent/src/{index,prompt}.ts`,
`daemon/extensions/feature-dev/src/index.ts`,
`daemon/sdk/examples/sample-extension.ts`,
`internal/daemon/api/types.go`,
`internal/daemon/rpcclient/{types,*}.go`,
`cmd/autosk/{agent,enroll,create}.go`,
`internal/lazy/{datasource,tui}/*.go`,
`gui/src/**`,
`docs/{workflows,extensions}.md`, `CHANGELOG.md`.

---

## 1. Motivation

Today a workflow references its agents **by name** through a string indirection,
and agents are registered **separately** via a second API method:

```ts
export default function (autosk: AutoskAPI) {
  autosk.registerAgent(piAgent({ name: "@me/dev", firstMessageFile: "…" }));
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      dev:    { agent: "@me/dev" },   // string indirection
      accept: { human: true },
    },
  });
}
```

This has three smells:

- **Two registration calls** for one logical unit. An agent only ever exists to
  serve a step, but you declare it elsewhere and wire it by a stringly-typed
  name that the compiler cannot check.
- **A global agent namespace** (`registry.agent.*`, `autosk agent list/show`,
  the `single:<agent>` synthetic-workflow machinery) that exists mostly to
  support `enroll --agent`, an ad-hoc one-off-agent enroll path.
- **`human: true`** is the only terminal/park marker a step can carry; there is
  no way to declare a step that closes (`done`) or abandons (`cancel`) a task.

The target shape collapses the agent into the step it serves and drops the
separate agent registry entirely:

```ts
import { statusStep } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { worktreeIsolation } from "@autosk/worktree";

autosk.registerWorkflow({
  name: "feature-dev",
  description: "…",
  firstStep: "dev",
  steps: {
    dev:       piAgent({ firstMessageFile: ".../dev.md" }),   // agent name == "dev"
    review:    piAgent({ thinking: "xhigh", firstMessageFile: ".../review.md" }),
    docs:      piAgent({ firstMessageFile: ".../docs.md" }),
    validator: piAgent({ firstMessageFile: ".../validator.md" }),
    accept:    statusStep("human"),                            // terminal/park step
  },
  onTransit(ctx, to) { … },
  isolation: worktreeIsolation(),
});
```

Registering the workflow registers its agents — there is no second call.

---

## 2. Locked decisions

These are the contract — do not relitigate when implementing.

| Topic | Choice |
|---|---|
| `AutoskAPI` surface | A single method: `registerWorkflow(workflow)`. `registerAgent` is **removed**. |
| Step value | `StepDef = AgentDefinition \| StatusStep`. Discriminated structurally: an `AgentDefinition` has `onRun`; a `StatusStep` has `status`. |
| Agent name | The **step key** is the agent name. `AgentDefinition` no longer has a `name` field, and `PiAgentOptions` no longer accepts `name`. No override. |
| Agent identity in pi-agent | The pi-agent driver takes its display name from `ctx.workflows.current.step`, not from any option. |
| Status step | `statusStep("done" \| "cancel" \| "human")`, a new SDK helper, returns `{ status }`. Entering such a step does not schedule an agent; the engine moves the task to that status (`human` parks + is resumable; `done`/`cancel` close). |
| `enroll --agent` / `singleStep` | **Removed entirely** — SDK (`singleStep.ts`), the `single:<agent>` synthetic-workflow machinery, the proto `{ agent }` enroll arm, the `task.enroll {agent}` daemon path, the CLI `--agent` flags on `enroll`/`create`, the Go `EnrollAgent` client, and the TUI `single:<agent>` filtering. Enroll is `{ workflow }` only. |
| Agent-registry surface | **Removed entirely** — `registry.agent.list` RPC verb, `AgentInfo` (TS + Go), `autosk agent list/show` (`cmd/autosk/agent.go`), and the registry's `addAgent`/`getAgent`/`hasAgent`/`listAgents`/`agentNames`. Agents are pure step internals. |
| `WorkflowStepInfo` wire shape | Replace `agent: string\|null` + `human: boolean` with a single `status: "done"\|"cancel"\|"human"\|null`. An agent step renders `status: null`; the step's `name` is the agent name. |
| `SessionMeta.agent` | Kept on the wire; the engine sets it to the **step key** (== agent name). No Go session-rendering change. |
| `firstStep` | Validation unchanged (`firstStep in steps`). A `statusStep` firstStep is permitted (a task that parks/closes on enroll); not special-cased. |
| Reserved namespaces | The `single:` reserved-prefix rule in `addWorkflow` is removed (no more synthetic workflows). |

---

## 3. Design at a glance

```
        ┌───────────────────────── @autosk/sdk ─────────────────────────┐
        │ AgentDefinition  = { onRun, onSteer?, onFollowup?, onAbort? }  │
        │ StatusStep       = { status: "done"|"cancel"|"human" }         │
        │ StepDef          = AgentDefinition | StatusStep                │
        │ isAgentStep / isStatusStep  (type guards)                      │
        │ statusStep(status)          (helper)                           │
        │ AutoskAPI        = { registerWorkflow }                        │
        └───────────────────────────────────────────────────────────────┘
                                   │ registerWorkflow(def)
                                   ▼
        ┌──────────────── ExtensionRegistry (per project) ──────────────┐
        │ addWorkflow: validate each step is agent|statusStep;          │
        │              store the WorkflowDefinition as-is (no rewrite).  │
        │ resolveWorkflow(name) → WorkflowDefinition | undefined        │
        │ renderWorkflowInfo: per step → { name, status, targets }      │
        │ (no agent map, no single: handling, no agent.list)            │
        └───────────────────────────────────────────────────────────────┘
                                   │
        ┌──────────────────────── Engine ───────────────────────────────┐
        │ dispatch: step = wf.steps[row.step];                          │
        │           isStatusStep(step) → skip (already in status);      │
        │           else run step.onRun, SessionMeta.agent = row.step.  │
        │ positionFor: { step } → status = isStatusStep ? step.status   │
        │                                : "work".                      │
        └───────────────────────────────────────────────────────────────┘
```

The registry stores the `WorkflowDefinition` **exactly as the extension wrote
it** — there is no internal normalisation/harvest into a separate agent map,
because nothing addresses agents independently any more. The engine and the
renderer discriminate steps at the point of use via `isAgentStep` /
`isStatusStep`.

---

## 4. Per-layer plan

### 4.1 SDK (`daemon/sdk/src/`)

- **`agent.ts`** — drop `name` from `AgentDefinition`. The run-context types are
  unchanged.
- **`workflow.ts`** — replace `StepDef`:
  ```ts
  export interface StatusStep { status: "done" | "cancel" | "human"; }
  export type StepDef = AgentDefinition | StatusStep;
  export function isStatusStep(s: StepDef): s is StatusStep {
    return typeof (s as StatusStep).status === "string";
  }
  export function isAgentStep(s: StepDef): s is AgentDefinition {
    return typeof (s as AgentDefinition).onRun === "function";
  }
  ```
  `WorkflowDefinition.steps` becomes `Record<string, StepDef>` (same type name,
  new union).
- **`statusStep.ts`** — new file (replaces `singleStep.ts`):
  ```ts
  export function statusStep(status: "done" | "cancel" | "human"): StatusStep {
    return { status };
  }
  ```
  Delete `singleStep.ts` and its `single:`/`singleStep` exports.
- **`types.ts`** — delete `AgentInfo`. Rewrite `WorkflowStepInfo`:
  ```ts
  export interface WorkflowStepInfo {
    name: string;
    /** Terminal/park status for a statusStep; null for an agent step. */
    status: "done" | "cancel" | "human" | null;
    targets: StepTarget[];
  }
  ```
- **`api.ts`** — `AutoskAPI = { registerWorkflow }`. Update the doc comment.
- **`proto.ts`** — remove `registry.agent.list` from the method map + the method
  name list; remove the `AgentInfo` import. Change `TaskEnrollParams` to
  `ProjectSelector & { id: string; workflow: string }` (no `{ agent }` arm).
- **`index.ts`** — export `statusStep` (drop `singleStep`).
- **`sdk/test/`** — delete `singleStep.test.ts`; add `statusStep.test.ts`; fix
  `proto.test.ts` (method list).

### 4.2 core (`daemon/core/src/`)

- **`extensions/registry.ts`**:
  - Delete: `SINGLE_STEP_PREFIX`, `parseSingleStepName`, `addAgent`, `getAgent`,
    `hasAgent`, `singleStepFor`, `listAgents`, `agentNames`, the agents map, and
    the `single:`-resolution branch of `resolveWorkflow`.
  - `resolveWorkflow(name)` = `this.workflows.get(name)`.
  - `addWorkflow`: keep the name/firstStep/steps validation; additionally
    validate each step is an `AgentDefinition` (has `onRun`) **or** a valid
    `StatusStep` (has a `status` in `{done,cancel,human}`) — else record a
    diagnostic and skip. Store the definition unchanged. Drop the reserved
    `single:` check.
  - `renderWorkflowInfo`: per step → `{ name, status: isStatusStep(def) ?
    def.status : null, targets }`. The conservative `targets` superset is
    unchanged.
- **`extensions/loader.ts`** — the per-extension `AutoskAPI` handle is
  `{ registerWorkflow: (wf) => registry.addWorkflow(entry.source, wf) }`.
- **`engine/engine.ts`**:
  - `EnrollTarget = { workflow: string }`. Delete the `{ agent }` arm and the
    `agent`-branch in `resolveEnrollTarget` (which used `hasAgent` /
    `singleStepFor`).
  - `dispatch`: `const step = wf.steps[row.step]`; `if (isStatusStep(step))
    return;` (replaces `stepDef.human || !stepDef.agent`); otherwise the
    `AgentDefinition` is `step` itself (no `getAgent`); `SessionMeta.agent =
    row.step`; pass `step` + `row.step` as `agent`/`agentName` to
    `SessionRuntime`.
- **`engine/transition.ts`** — `positionFor` `{ step }` branch uses
  `isStatusStep(wf.steps[to.step])` to pick the status (`human`/`done`/`cancel`
  vs `work`). `validateTarget` unchanged.
- **`rpc/daemon.ts`** — delete the `registry.agent.list` handler; in
  `task.enroll` drop the `agent` arm + the "exactly one of workflow/agent"
  validation (now just require `workflow`).

### 4.3 Bundled extensions

- **`pi-agent/src/index.ts`** — drop `name` from `PiAgentOptions` and the
  `name`-required throw. In `onRun`, derive the agent display name from
  `ctx.workflows.current.step` and pass it to `renderInitialPrompt({ agentName,
  … })`. The `liveSessions` map stays keyed by session id (unaffected).
- **`feature-dev/src/index.ts`** — inline the four roles into `steps`, replace
  `accept: { human: true }` with `accept: statusStep("human")`, delete
  `featureDevAgents()` and the `registerAgent` loop; the factory is just
  `autosk.registerWorkflow(featureDevWorkflow())`.
- **`sdk/examples/sample-extension.ts`** — rewrite to the inline shape; drop the
  `single:<agent>` note.

### 4.4 Go (`cmd/autosk`, `internal/`)

- **`internal/daemon/api/types.go`** — delete `AgentInfo`; rewrite
  `WorkflowStepInfo` to `{ Name string; Status *string; Targets []StepTarget }`
  (pointer/`omitempty` to mirror `status: … | null`). Update the
  `registry.agent.list` comment.
- **`internal/daemon/rpcclient/`** — delete the `Agent` type alias and the
  `registry.agent.list` call (`types.go`, the lister); delete `EnrollAgent`
  (`writes.go`).
- **`cmd/autosk/agent.go` (+ `agent_test.go`)** — delete the `autosk agent`
  command tree and register it nowhere.
- **`cmd/autosk/enroll.go`, `create.go`** — remove the `--agent` flag, the
  mutual-exclusion check, and the `EnrollAgent` call path; `--workflow` becomes
  the only enroll target.
- **`internal/lazy/datasource/`** — drop `EnrollAgent` from the interface + RPC +
  wrappers; drop any `single:<agent>` filtering.
- **`internal/lazy/tui/`** — drop the `single:<agent>` filtering in the enroll
  popup / workflow list and the `EnrollAgent` keybinding; render the new
  `Status` field in the workflow/step view instead of `Agent`/`Human`.

### 4.5 GUI (`gui/`)

- Mirror the `WorkflowStepInfo` change (`agent`/`human` → `status`) in the TS
  types + any step rendering; remove any agent-list view if present. Run
  `npm run typecheck` + `cargo check`.

### 4.6 Docs + changelog

- **`docs/workflows.md`** — rewrite the `WorkflowDefinition` / `StepDef` /
  agent-definition sections to the inline shape; document `statusStep`; delete
  the "Agent definitions" registry framing, the `singleStep` builtin section,
  and the `enroll --agent` / `autosk agent` CLI rows.
- **`docs/extensions.md`** — `AutoskAPI` now has one method; rewrite the example;
  delete the `singleStep` builtin section and the `enroll --agent` example.
- **READMEs** — `pi-agent`, `feature-dev` (drop `name`, show inline steps).
- **`CHANGELOG.md`** — `### Removed`: `autosk agent list/show`, `enroll --agent`
  / `create --agent`, the `single:<agent>` synthetic workflow. `### Changed`:
  workflow extensions register agents inline via step values; `statusStep`
  replaces `{ human: true }` and supports `done`/`cancel`.

---

## 5. Risks & edge cases

- **Big cross-cutting surface.** SDK → core → proto → Go CLI/TUI → GUI → docs →
  tests. Work top-down by layer; run `bun run typecheck` + `bun test` in
  `daemon/`, then `make test`, then `gui` checks, after each layer.
- **Session-meta name shift.** feature-dev agent steps change `SessionMeta.agent`
  from `@autosk/pi-agent/dev` to `dev`. In-flight tasks survive a restart (the
  live-code hazard guard validates the **step**, not the agent), but historical
  transcripts keep the old names — cosmetic only.
- **Third-party extensions break.** Any global/project extension calling
  `registerAgent` or registering string-`agent` steps fails to load and shows up
  in `project.diagnostics` (the factory throws on an undefined method / the
  step-shape validation rejects it). Acceptable for pre-1.0 v2; call it out in
  the changelog.
- **`StatusStep` vs `StepTarget` structural overlap.** Both are `{ status }`.
  They never mix (one is a step value, the other a transition target), but keep
  the `isStatusStep` guard narrow (`typeof s.status === "string"`) so an
  `AgentDefinition` is never misread.
- **`done`/`cancel` step semantics.** `positionFor` for a `{ step }` into a
  status step keeps `workflow` + sets `step` to that step's name; for `done`/
  `cancel` the task closes showing it ended on that step. Consistent with the
  existing `{ status }`-target behaviour (which keeps the *leaving* step).

---

## 6. Test plan

**Bun (`daemon/`):**
- `sdk`: `statusStep.test.ts` (helper shape); `proto.test.ts` method-list fix.
- `core/extensions.registry.test.ts`: step-shape validation (agent vs
  statusStep vs invalid), `renderWorkflowInfo.status`, removal of agent/single
  assertions.
- `core/extensions.loader.test.ts`: drop `registerAgent` fixtures; assert the
  handle exposes only `registerWorkflow`.
- `core/engine.transitions.test.ts`: enroll `{ workflow }` only; transit into a
  `statusStep("human")` parks, `statusStep("done")` closes; delete the
  `{ agent }` / `single:<agent>` cases.
- `core/rpc.*`: replace `enroll {agent}` with workflow enroll; delete
  `registry.agent.list` conformance; assert `task.enroll {agent}` is rejected
  (unknown param) — or simply gone from the method set.
- `core/extensions.bundled.test.ts`: assert feature-dev steps + `statusStep`
  accept; drop `hasAgent(role)` checks.

**Go:**
- Delete `cmd/autosk/agent_test.go`; fix `verbs_test.go` (`--agent` rejection +
  `enroll --agent` cases removed); fix `lazy_e2e_test.go` / datasource fakes
  (drop `EnrollAgent`).
- Workflow rendering tests: `Status` field instead of `Agent`/`Human`.

**Acceptance:** `make test` green; `daemon/` `bun run typecheck` + `bun test`
green; `gui/` `npm run typecheck` + `cargo check` green; `autosk workflow show
feature-dev` renders the four agent steps + the `accept` human status step; a
`feature-dev` enroll drives `dev → … → accept` and parks at `accept`.
