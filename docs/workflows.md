# Workflows & agents (v0.2)

autosk v0.2 turns the task tracker into a small workflow engine. Tasks
move through a directed graph of **steps**; each step is owned by an
**agent** (a human or a background pi instance). The daemon picks up
tasks in `in_workflow` status, spawns the step's agent, and advances the
task when the agent emits a structural transition signal.

If you haven't yet, read the design plan first:
[`docs/plans/20260517-Workflows-Plan.md`](plans/20260517-Workflows-Plan.md).

---

## Concepts

### Agent

A named actor that can own a task. Stored in the `agents` table with
the minimal shape `(id, name, is_human)`. `human` is seeded on first
migrate.

All other agents come from **npm packages** installed into the global
autosk packages prefix (`~/.autosk/packages/` by default; override with
`$AUTOSK_PACKAGES`). The agent's name in `agents.name` is the **full
npm package name** — there's no aliasing.

Install one and start using it:

```bash
autosk agent install @autosk/developer
autosk agent install @autosk/code-reviewer
autosk agent install @autosk/task-validator
```

Workflows reference agents by their full npm package name via the
per-step `agent` object: `{ "name": "@autosk/developer" }`. The
`human` name is the only one that doesn't need to come from a package.
Per-step overrides for the package's defaults live under
`agent.params` (see [Per-step agent overrides](#per-step-agent-overrides) below).

The agent's **executable configuration** (model, thinking depth, the
text prepended to the first user turn, extra pi args, custom runner)
lives inside the package's `package.json`:

```jsonc
{
  "name":    "@autosk/developer",
  "version": "0.3.1",
  "type":    "module",
  "autosk": {
    "agent": {
      "model":              "sonnet:high",
      "thinking":           "high",
      "first_message_file": "./prompts/first_message.md",
      "extra_args":         ["--no-tool", "web_fetch"],
      "pi_extensions":      ["./extensions/foo.ts"],
      "pi_skills":          ["./skills"]
    }
  }
}
```

> The `first_message`(_file) text is **not** a system-role prompt. pi
> has its own system prompt; this field is the leading block of the
> first user turn the executor sends. Earlier drafts called this
> `system_prompt_file` — the name was misleading and was renamed.

The daemon resolves this configuration at spawn time. The `human` agent
has no package — the workflow engine never tries to spawn it.

#### Two flavours of agent

A package can be one of:

- **Standard** — declares `model`, `thinking`, `first_message`(_file),
  `extra_args`, `pi_extensions`, `pi_skills`. The daemon spawns `pi
  --mode rpc` with these settings; the agent is just pi configured
  through the package.

- **Custom** — declares an `autosk.agent.runner` pointing at a TS/JS
  module. The daemon spawns the Node bootstrapper
  (`@autosk/agent-runtime`), which imports the runner module and awaits
  its default export. The runner is free to do anything (call any
  LLM, run a deterministic shell pipeline, delegate back into pi via
  `ctx.spawnPi(...)`); it just needs to call `ctx.stepNext(...)` once
  before returning.

  Minimal custom runner:

  ```ts
  // my-agent/src/agent.ts
  import type { RunAgent } from "@autosk/agent-sdk";

  const run: RunAgent = async (ctx) => {
    await ctx.cli(["comment", "add", ctx.task.id, "starting"]);
    // ... do work ...
    await ctx.stepNext("done");
  };
  export default run;
  ```

  Custom runners are **single-shot**: the bootstrapper's process runs
  once and the executor checks `step_signals` after it exits. There is
  no kickback loop for the custom branch — a missed signal fails the
  run with `agent_did_not_emit_transition` immediately.

  See [`@autosk/agent-sdk`](../extension/sdk/README.md) for the full
  contract and [`@autosk/agent-runtime`](../extension/runtime/README.md)
  for the bootstrapper.

#### Manage installed agents

```bash
autosk agent list                       # union of installed pkgs + DB rows
autosk agent show  @autosk/developer    # registry + DB view
autosk agent uninstall @autosk/developer
autosk agent runtime install            # eagerly install @autosk/agent-runtime
```

The `human` agent always shows up with `source=builtin`. Packages whose
row is in the project DB but no matching install exists locally are
flagged `source=db_only` so the diagnostic surface is obvious.

Plan: [`docs/plans/20260518-Agent-Packages.md`](plans/20260518-Agent-Packages.md).

> **Security:** the spawned agent inherits the daemon's environment.
> Never put API keys in `package.json`. pi reads credentials from env;
> custom runners should do the same.

### Workflow

A workflow is a small directed graph of steps. Define it as a JSON file
and import:

```bash
autosk workflow create --file docs/notes/workflow-example.json
```

The shipped example:

```jsonc
{
  "name": "feature-dev",
  "description": "Implement, review, validate, then ask the human.",
  "first_step": "dev",
  "steps": {
    "dev":       { "agent": { "name": "developer" },     "next_steps": [{"step":"review", "prompt_rule":"…"}] },
    "review":    { "agent": { "name": "code-reviewer" }, "next_steps": [
                     {"step":"validator", "prompt_rule":"…"},
                     {"step":"dev",       "prompt_rule":"…"}
                   ]},
    "validator": { "agent": { "name": "task-validator" },"next_steps": [
                     {"step":"dev",                    "prompt_rule":"…"},
                     {"task_status":"human_feedback",  "prompt_rule":"…"}
                   ]}
  }
}
```

The `agent` field is always an object. The bare-string form
(`"agent": "developer"`) used in earlier drafts is no longer
accepted; the parser rejects it with a hint.

A `next_steps[]` entry has either a `step` (sibling step name in the
same workflow) or a `task_status` (one of `done`, `cancelled`,
`human_feedback`), never both. `prompt_rule` is natural-language guidance
shown to the agent in its prompt — the agent uses it to decide which
transition to emit.

### Per-step agent overrides

A step's `agent.params` block overrides the agent package's defaults
for *this step only*. The whole block is optional; absent keys keep
the package's value.

```jsonc
{
  "name": "generic-wf",
  "first_step": "generic",
  "steps": {
    "generic": {
      "agent": {
        "name": "@autogent/generic",
        "params": {
          "model": "claude-sonnet-4-6",
          "thinking": "high",
          "first_message": "You are generic agent...",
          "extra_args": ["--no-tool", "web_fetch"],
          "pi_extensions": ["/abs/path/to/ext.ts"],
          "pi_skills":     ["/abs/path/to/skill"]
        }
      },
      "next_steps": [
        { "task_status": "human_feedback", "prompt_rule": "When a human should accept it." },
        { "task_status": "done",           "prompt_rule": "When the task is complete." }
      ]
    }
  }
}
```

Merge semantics:

- **Scalars** (`model`, `thinking`, `first_message`) replace the
  package default when the key is present (even when set to the empty
  string).
- **Arrays** (`extra_args`, `pi_extensions`, `pi_skills`) replace the
  package default wholesale when the key is present with a non-null
  slice. There is no append mode.
- **`first_message_file`** is accepted in `params` (paths are resolved
  relative to the workflow JSON file at parse time) and inlined into
  `first_message`. It is mutually exclusive with `first_message`.
- **`runner`** is intentionally NOT supported: overrides can only
  change standard-agent fields. Trying to override a custom-runner
  agent (a package that declares `autosk.agent.runner`) with any
  `params` block fails the run with `agent_config_invalid`.
- The `thinking` value, when set, must be one of
  `off|minimal|low|medium|high|xhigh` (same as the package field).
- Unknown keys inside `params` are rejected at parse time.

> **Edge case.** Because `params` is persisted as JSON with
> `omitempty`, an explicit empty array (e.g. `"extra_args": []`)
> collapses to "absent" after a round-trip through the DB. If you
> need to clear an array, edit the package instead.

Validation enforces:

- `name` is unique and does not start with the reserved prefix `single:`.
- `first_step` exists in `steps`.
- Every step has ≥1 transition.
- Every referenced agent exists in the `agents` table.
- Every sibling-step target is defined.

### Task

In v0.2 a task carries three new nullable FKs: `author_id`,
`workflow_id`, `current_step_id`. The status enum is exactly:

```
new | in_workflow | human_feedback | done | cancelled
```

(`claimed` is gone — replaced by `assign --agent`. `blocked` is still
**not** a stored status: it's derived from open blocker edges.)

A SQL CHECK ties `status = 'in_workflow'` to `current_step_id IS NOT
NULL`. The CLI verbs (`create`, `assign`, `resume`, `done`, `cancel`,
`reopen`) take care of keeping this invariant satisfied; you should not
need to think about it.

### `single:<agent>` synthetic workflows

`autosk create … --agent NAME` (and `autosk assign … --agent NAME`)
auto-create a hidden workflow named `single:NAME` with one step `do`
whose transitions go to `{done, cancelled, human_feedback}`. The
executor uses the same code path as a real workflow — there is no
second branch.

These rows have `is_synthetic = 1` and are hidden from
`autosk workflow list` by default. Use `--all` to reveal them.

---

## CLI walkthrough

This mirrors the W9 acceptance scenario (see plan §9), updated for the
npm-package agent model.

```bash
# 1. Bootstrap.
autosk init
autosk agent install @autosk/developer
autosk agent install @autosk/code-reviewer
autosk agent install @autosk/task-validator

# (each install reads the package's autosk.agent block from
#  ~/.autosk/packages/node_modules/<pkg>/package.json and registers it)

autosk workflow create --file docs/notes/workflow-example.json
# wf-XXXX

# 2. Create a task in the workflow.
id=$(autosk create "Implement auth module" --workflow feature-dev --json | jq -r .id)
autosk show "$id"
# status: in_workflow, current_step: dev, current_agent: developer

# 3. Run the daemon (in another shell).
autosk daemon serve --workers 1 &

# 4. The poller picks up the task; the developer agent spawns; at the
# end of its turn it calls (via the autosk pi-extension):
#
#     autosk step next $id --to review
#
# The daemon records the signal, advances the task to step=review,
# and the loop continues until validator → human_feedback.

# 5. Watch transitions.
while true; do
  autosk show "$id" --json | jq -r '"status=\(.status) step=\(.current_step // \"-\")"'
  sleep 2
done

# 6. Resume after human review.
autosk resume "$id"          # back to status=in_workflow at validator
# … the workflow continues; eventually validator → done (via `done`
# transition you added, or a human running `autosk done $id`).
```

The single-agent shorthand:

```bash
id=$(autosk create "Bump version to 0.2" --agent @autosk/developer --json | jq -r .id)
autosk show "$id"
# status: in_workflow, workflow: single:@autosk/developer, current_step: do

# The developer agent runs once, picks `--to done`, and the task closes.
```

---

## Agent contract: `autosk step next`

When an agent runs **inside a workflow step** (i.e. the daemon spawned
it), it MUST call exactly one of:

```bash
autosk step next <task-id> --to <sibling-step-name>
autosk step next <task-id> --to done
autosk step next <task-id> --to cancelled
autosk step next <task-id> --to human_feedback
```

before stopping its turn. The valid targets for the current step are
listed in the agent's initial prompt (one bullet per outgoing
transition). Calling `step next` twice in the same run is rejected.

If the agent ends its turn without emitting a signal, the daemon sends a
corrective message and retries up to `max_corrections` (default 3)
times. After that the run is marked `failed` with
`error="agent_did_not_emit_transition"`.

The pi-extension's `step_next` action is the agent's tool-level handle
into this CLI — same arguments, same semantics.

---

## What humans still do directly

For tasks in `human_feedback`, the agent has handed control back to a
human. Humans can:

- **Inspect**: `autosk show <id>`, `autosk comment list <id>`.
- **Reply**: `autosk comment add <id> "..."` — the comment is surfaced
  to the next step's prompt.
- **Resume**: `autosk resume <id> [--to STEP]`. By default returns to
  the same step (so the same agent sees the new comments); `--to STEP`
  jumps to a different step in the same workflow.
- **Close**: `autosk done <id>` / `autosk cancel <id>` — direct, never
  through `step next`. The poller only picks tasks whose current step
  has `is_human=0`, so these verbs never race with the engine.
- **Reopen**: `autosk reopen <id>` — done/cancelled → new. Clears
  `current_step_id`; preserves `workflow_id` for audit.

`autosk update --status` is rejected on `in_workflow` tasks: those
transitions are owned by the workflow engine.

---

## Wiring inside the daemon

```
┌───────────────────────────────────────────────────────┐
│            autosk daemon serve                        │
│                                                       │
│   ┌────────────┐   ┌────────────┐   ┌──────────────┐  │
│   │  poller    │──▶│ scheduler  │──▶│ step         │  │
│   │ (§5.2 SQL) │   │ N workers  │   │ executor     │  │
│   └────────────┘   └────────────┘   │  - load agent│  │
│                                     │    config    │  │
│                                     │  - spawn pi  │  │
│                                     │  - wait for  │  │
│                                     │    step_signals
│                                     │  - kickback  │  │
│                                     │  - advance   │  │
│                                     └──────────────┘  │
└───────────────────────────────────────────────────────┘
                          │
                          ▼
                pi --mode rpc (per spawn)
```

Per `--poll-interval` (default 2s), the poller scans:

```sql
SELECT t.id, t.current_step_id
  FROM tasks t
  JOIN steps s   ON t.current_step_id = s.id
  JOIN agents a  ON s.agent_id        = a.id
 WHERE t.status = 'in_workflow'
   AND a.is_human = 0
   AND NOT EXISTS (
     SELECT 1 FROM daemon_runs r
     WHERE r.task_id = t.id AND r.status IN ('queued','running')
   )
 ORDER BY t.priority ASC, t.created_at ASC;
```

Each row is materialised as a `daemon_runs` entry (status=queued) and
pushed onto the scheduler queue. A worker picks it up; the executor
spawns pi with the step's agent config and watches `step_signals` after
every end-of-turn.

---

## Glossary

| Term | Meaning |
|---|---|
| **Step** | One node in a workflow. Has a name (`dev`, `review`, …), an `agent_id`, and ≥1 outgoing transition. |
| **Transition** | One edge from a step. Carries either a sibling-step target or a `task_status` (`done`/`cancelled`/`human_feedback`), plus a natural-language `prompt_rule`. |
| **Signal** | A row in `step_signals`. Inserted by `autosk step next` from inside the agent; consumed by the executor after end-of-turn. PK on `run_id` enforces "exactly one signal per run". |
| **Kickback** | A corrective message sent to the agent when it ends a turn without emitting a signal. Bounded by `max_corrections`. |
| **Synthetic workflow** | `single:<agent>` workflow auto-created by `--agent NAME`. `is_synthetic = 1`; hidden from `workflow list` unless `--all`. |

---

## Pointers

- Design: [`docs/plans/20260517-Workflows-Plan.md`](plans/20260517-Workflows-Plan.md)
- Daemon details: [`docs/daemon.md`](daemon.md)
- pi-extension surface: [`extension/src/`](../extension/src/)
- Example workflow: [`docs/notes/workflow-example.json`](notes/workflow-example.json)
