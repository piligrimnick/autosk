# Workflows & agents (v0.2)

autosk v0.2 turns the task tracker into a small workflow engine. Tasks
move through a directed graph of **steps**; each step is owned by an
**agent** (a human or a background pi instance). The daemon picks up
tasks in `work` status, spawns the step's agent, and advances the
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
    "dev":       { "agent": { "name": "developer" },     "max_visits": 5, "next_steps": [{"step":"review", "prompt_rule":"…"}] },
    "review":    { "agent": { "name": "code-reviewer" }, "max_visits": 5, "next_steps": [
                     {"step":"validator", "prompt_rule":"…"},
                     {"step":"dev",       "prompt_rule":"…"}
                   ]},
    "validator": { "agent": { "name": "task-validator" },"next_steps": [
                     {"step":"dev",                  "prompt_rule":"…"},
                     {"task_status":"human",         "prompt_rule":"…"}
                   ]}
  }
}
```

The `agent` field is always an object. The bare-string form
(`"agent": "developer"`) used in earlier drafts is no longer
accepted; the parser rejects it with a hint.

A `next_steps[]` entry has either a `step` (sibling step name in the
same workflow) or a `task_status` (one of `done`, `cancel`,
`human`), never both. `prompt_rule` is natural-language guidance
shown to the agent in its prompt — the agent uses it to decide which
transition to emit.

### Visit limits (`max_visits`)

A workflow that loops (`dev → review → dev → …`) can churn indefinitely
if its agents keep bouncing the task back. Each step block accepts an
optional `max_visits: N` cap so the engine can fail loudly and escalate
to a human instead of burning tokens forever.

```jsonc
"review": {
  "agent":      { "name": "@autosk/code-reviewer" },
  "max_visits": 5,
  "next_steps": [ /* … */ ]
}
```

Semantics:

- **What counts as a visit.** Every transition INTO the step bumps the
  counter — including the very first entry when a task is created
  (`autosk create … --workflow X`) or enrolled (`autosk enroll …
  --workflow X`) at this step. The cap is checked *before* the
  counter is bumped, so the Nth visit is allowed and the (N+1)th
  fails.
- **`autosk resume <id>` without `--to` does NOT count.** It just flips
  `human → work` and the same step keeps its current
  counter — there was no transition. `autosk resume <id> --to STEP`
  DOES count, even when `STEP` is the step the task currently sits on:
  that's a deliberate re-entry and is treated as one.
- **`max_visits: 0` or absent → unlimited.** The counter still ticks,
  but the engine never compares it to anything. Synthetic
  `single:<agent>` workflows always have `max_visits = 0`.
- **What happens when the cap fires.** The current run is failed with
  `daemon_runs.status = 'failed'` and
  `daemon_runs.error = 'step_max_visits_exceeded: step "…" reached
  visits=N max=N'`. The task is parked to `status = 'human'`
  via the same `parkTaskOnFailure` path used by any other terminal
  failure. **`current_step_id` moves to the TARGET step** — the one
  the run was trying to enter when the cap fired (e.g. on a
  `dev → review` advance that breaches `review.max_visits`, the task
  parks on `review`, not on `dev`). After resetting the counter,
  `autosk resume <id>` (no `--to`) lands on the same target step so
  the loop can continue from there. This rule is specific to
  `advanceTask` failures — every other failure mode (pi crash,
  runner timeout, SendPrompt error, shutdown error,
  `agent_did_not_emit_transition`) leaves `current_step_id`
  untouched on the source step, because the run died before the
  target was known.
- **Counters live in `tasks.metadata.step_visits`** as a JSON object
  keyed by `step_id` (the engine-managed schema; see
  [Task metadata](#task-metadata) below).
- **Resets are humans-only.** Nothing auto-clears counters — not
  `reopen`, not `enroll`, not the daemon. Use
  `autosk metadata reset-visits <id>` (optionally with
  `--step NAME` or `--step-id ID`) as the escape hatch.
- **Validation.** Negative `max_visits` is rejected at workflow import
  time with `step %q: max_visits must be >= 0`.

Typical cap-fire recovery:

```bash
# Task is parked because review hit max_visits = 5.
autosk show as-bea9
# status: human
# current_step: review   <-- TARGET of the failed advance (the
#                            cap fired on the dev→review transition,
#                            so the task parks on review and `resume`
#                            without --to resumes there once the
#                            counter is cleared)
# visits: dev 4/5, review 5/5*

autosk daemon messages <job-id>
# … error: step_max_visits_exceeded: step "review" reached visits=5 max=5

# Inspect, decide, reset the counter you want to clear:
autosk metadata reset-visits as-bea9 --step review
# … and continue the loop:
autosk resume as-bea9
```

### Worktree isolation

By default every step of every task on a project runs in the project
root. Two tasks of the same workflow running concurrently will race on
the filesystem. To give each task its own sandbox, mark the workflow as
isolated:

```jsonc
{
  "name":       "feature-dev",
  "first_step": "dev",
  "isolation": "worktree",        // NEW; optional, default "none"
  "steps":     { ... }
}
```

Accepted values: `"none"` (the default — preserves previous behaviour)
and `"worktree"`. Anything else is rejected at `workflow create` time
with `unknown isolation mode: %q (want none|worktree)`. Synthetic
`single:<agent>` workflows (created implicitly by `--agent NAME`) are
pinned to `"none"` by the engine and cannot be flipped.

When a task targets a workflow with `isolation: "worktree"`:

- `autosk create` and `autosk enroll` allocate a per-task git worktree
  **before** the task enters the workflow. Path:
  `~/.autosk/worktrees/<basename>-<8hex(sha256(canonRoot))>/<task-id>`.
  Branch: `autosk/<task-id>`. Both are **derived deterministically**
  from the canonical project root + task id — no row in any table
  stores a copy.
- The daemon spawns every step run with `cwd =` the worktree and
  `AUTOSK_DB =` the canonical project DB. The agent's writes against
  `.autosk/db` therefore still land in the project's single source of
  truth, while file edits stay inside the per-task worktree.
- On `done` / `cancel` (via `autosk step next …`, `autosk done`,
  `autosk cancel`, or any other terminal transition the engine
  records), the worktree **directory** is removed automatically and
  the **branch is preserved** for human review / merge.
- On `human` (parked) and sibling-step transitions the
  worktree is kept on disk — the human (or the next step's agent)
  needs it to inspect or continue the work.
- On a parked-failure (`parkTaskOnFailure`) the worktree is *also*
  preserved: the human needs the on-disk state to debug. Cleanup
  happens only at a real terminal status.

```bash
autosk create "Wire up auth" --workflow feature-dev
# task created; worktree allocated at ~/.autosk/worktrees/myproj-7a3b9c2e/as-bea9
# branch autosk/as-bea9 created off HEAD

autosk show as-bea9
# …
# worktree:      ~/.autosk/worktrees/myproj-7a3b9c2e/as-bea9 (exists)
# branch:        autosk/as-bea9

autosk show as-bea9 --json | jq .worktree
# {
#   "path":   "/Users/me/.autosk/worktrees/myproj-7a3b9c2e/as-bea9",
#   "branch": "autosk/as-bea9",
#   "exists": true
# }
```

The `worktree` block in `autosk show` (text + JSON) is **omitted
entirely** for non-isolated workflows and for tasks with no workflow,
so existing JSON consumers don't see a new always-present key.

`autosk workflow show <name> [--json]` carries the isolation mode
verbatim. The `--json` shape always includes an `isolation` field
(`"none"` or `"worktree"`); the text form only prints an
`isolation:` line when the mode is non-default.

#### `enroll --base-ref`

When enrolling an existing task into an isolated workflow you can pick
the base ref the worktree's branch is forked from:

```bash
autosk enroll as-bea9 --workflow feature-dev --base-ref origin/main
```

`--base-ref` defaults to `HEAD`. It is **only** meaningful for
isolated workflows on a fresh allocation — passing it with `--agent`
or against a non-isolated workflow fails with `--base-ref requires the
target workflow to use isolation=worktree`. When the branch
`autosk/<task-id>` already exists (typical `reopen` → `enroll` cycle
after the worktree dir was reaped), `--base-ref` is ignored with a
stderr warning and the worktree is re-allocated on the existing
branch.

#### `autosk worktree`

Diagnostic / manual-recovery verbs. None of them mutate task rows.

| Verb | Behaviour |
|---|---|
| `autosk worktree list [--json]` | List every task in this project whose workflow has `isolation: "worktree"`, with task id, status, workflow, branch, on-disk presence, and path. Closed tasks whose dir survived a failed cleanup show `dir=missing` (`exists=false` in JSON). |
| `autosk worktree path <id> [--json]` | Print the derived worktree path. Pure helper; does not stat the path. Useful in shell aliases and scripts. |
| `autosk worktree rm <id> [--json]` | Force-remove the worktree directory; the branch is left intact. Refuses **only** `work` tasks (the daemon may be executing inside the dir right now). `new` / `human` / `done` / `cancel` are all accepted, so the `worktree_stranded` recovery flow below works without first closing the task. |

No `--all-projects` flag in v0.3: the CLI is single-process and has no
daemon round-trip for cross-project enumeration. Use it from each
project root individually.

#### Uncommitted work is discarded on terminal status

`git worktree remove --force` discards any uncommitted edits inside
the directory. The contract is **the agent commits its work before
signalling `done`**; everything else is intentionally lost. This is
why the kickback messages for `done` transitions in isolated
workflows remind the agent to commit first, and why the branch is
preserved (so a human can still inspect or merge what *was*
committed).

#### Missing worktree directory → auto-recovered

If the daemon picks up an isolated task and discovers its worktree
directory is gone, the executor transparently re-allocates it on the
existing `autosk/<task-id>` branch (via the same `worktree.Ensure`
the CLI uses on `enroll`) and the run proceeds. No park, no human
action required. The daemon emits an `executor: re-allocated missing
worktree` log line at `info` level so the recovery is auditable.

This covers the common cases where the dir went missing between
runs: terminal cleanup reaped it and the task was then
resumed/reopened, the operator manually `rm -rf`-ed
`~/.autosk/worktrees/...`, or a failed cleanup left only the branch.
The branch is the load-bearing piece (it survives terminal cleanup
by design), so re-creating the dir from the existing branch is
always safe.

If re-allocation itself fails (git not on PATH, project root no
longer a git repo, the slot is occupied by something that isn't a
registered worktree, etc.), the run fails with
`worktree_missing: re-allocate failed: ...` and the task is parked
to `human` for human triage.

#### Recovering from `worktree_stranded`

If the worktree directory is present but its `.git` no longer
resolves to the project's gitdir (the project was moved or
re-initialised), the run fails with `error=worktree_stranded` and the
task is parked to `human`. The worktree is **not**
auto-cleaned in this failure path — you may want to keep whatever is
on disk for debugging.

To recover, either give up:

```bash
autosk cancel as-bea9             # branch preserved; dir reaped if any remained
```

or re-allocate the worktree and retry:

```bash
autosk worktree rm as-bea9        # clean up the stranded dir
autosk cancel  as-bea9            # human → cancel (‘reopen’ doesn’t
                                  # take human as a source state)
autosk reopen  as-bea9            # cancel → new; current_step_id cleared
autosk enroll  as-bea9 --workflow feature-dev   # Ensure re-allocates dir + branch
# the daemon picks it up automatically; the branch (autosk/as-bea9)
# survived the round-trip so the worktree reuses it.
```

A shorter recovery via plain `autosk resume` is **not** possible:
`resume` flips status back to `work` at the same step without
cleaning the stranded dir, so the next run would `Verify`-fail and
park again.

#### Reopening a closed isolated task

`autosk reopen` itself never touches git. The branch survives the
reopen unmolested. A subsequent `enroll <id> --workflow <iso>`
re-allocates a fresh worktree directory on the existing branch (so
whatever was committed before is still in `git log`), and the engine
resumes the workflow from the entry step. `--base-ref` is ignored on
this path with a warning.

#### Limitations

- **Per-task, not per-step.** Every step of a given task shares the
  same worktree. Per-step isolation is out of scope for v0.3.
- **Project moves are unsupported.** The worktree path is derived
  from the canonical project root; moving the project directory
  invalidates every existing worktree. Recovery is `Verify`-fail →
  `human` → manual cleanup (`autosk worktree rm`) →
  re-enroll.
- **Network filesystems are unsupported.** Same constraint as
  `.autosk/db` itself.
- **No `--all-projects` on `worktree list`** in v0.3 (see above).
- **No orphan-branch GC.** `autosk worktree list` surfaces leaked
  directories from broken runs, but branches accumulate — prune
  them with normal git workflow once the task is well and truly
  done.

Design: [`docs/plans/20260521-Worktree-Isolation.md`](plans/20260521-Worktree-Isolation.md).
Example: [`docs/notes/workflow-isolated-example.json`](notes/workflow-isolated-example.json).

### Task metadata

Every task carries a free-form JSON `metadata` blob (the
`tasks.metadata` column, nullable). The engine reserves the top-level
key `step_visits` for the per-step visit counters used by
`max_visits`; every other key is user-defined and untouched by the
engine.

The `autosk metadata` verb tree is the human-facing surface:

```bash
autosk metadata show <id> [--visits-pretty]
autosk metadata set   <id> --key K  [--value V | --json-value FILE]
autosk metadata unset <id> --key K
autosk metadata reset-visits <id> [--step NAME | --step-id ID]
```

A few notes:

- `--key` is a dotted JSON object path (e.g. `tags`, `notes.tmp`,
  `step_visits.st-abcd`). Array indexing is not supported.
- `--value V` parses V as a JSON literal when V starts with one of
  `[`, `{`, `"`, `t`, `f`, `n`, `-`, or `0..9`; otherwise V is treated
  as a plain string. `--json-value FILE` (or `-` for stdin) reads a
  raw JSON value. Exactly one of `--value` / `--json-value` is
  required.
- Writes under `step_visits` are validated against the engine's typed
  shape (object of `step_id → non-negative integer`); attempts to set
  it to anything else fail before any DB write lands. This guards the
  engine from human-driven corruption.
- `metadata reset-visits` with no flag clears the entire
  `step_visits` map; with `--step NAME` it resolves the name against
  the task's current workflow and removes only that entry; with
  `--step-id ID` it removes the entry for that step id directly
  (handy for orphaned counters left behind when a workflow was
  recreated).
- No-op writes are detected — `metadata unset --key missing` or
  `reset-visits` on a task with no counters does not bump `updated_at`
  and does not produce a dolt commit.
- `autosk show <id>` (human renderer) surfaces a one-line
  `visits: dev 3/5, review 5/5*, validator 1` summary inline when the
  task has `step_visits`. The `*` marker flags steps at or over their
  cap; bare `N` (no denominator) means the step is uncapped.
  `autosk show --json` includes the raw `metadata` object verbatim.

See [`docs/notes/workflow-example.json`](notes/workflow-example.json)
for a working example with caps.

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
        { "task_status": "human", "prompt_rule": "When a human should accept it." },
        { "task_status": "done",  "prompt_rule": "When the task is complete." }
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
new | work | human | done | cancel
```

Every spelling is ≤ 7 chars so the persisted columns stay tight and
the TUI doesn't need to truncate. (`claimed` is gone — replaced by
`assign --agent`. `blocked` is still **not** a stored status: it's
derived from open blocker edges.) The legacy spellings
(`in_workflow`, `human_feedback`, `cancelled`) are not accepted on
any input surface (CLI flags, YAML/JSON workflow definitions, the
autosk_task / autosk_step JSON-RPC SDK). They were rewritten
in-place by a one-shot migration; see
[`docs/plans/20260521-Short-Statuses.md`](plans/20260521-Short-Statuses.md).

A SQL CHECK ties `status = 'work'` to `current_step_id IS NOT
NULL`. The CLI verbs (`create`, `assign`, `resume`, `done`, `cancel`,
`reopen`) take care of keeping this invariant satisfied; you should not
need to think about it.

### `single:<agent>` synthetic workflows

`autosk create … --agent NAME` (and `autosk assign … --agent NAME`)
auto-create a hidden workflow named `single:NAME` with one step `do`
whose transitions go to `{done, cancel, human}`. The
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
# status: work, current_step: dev, current_agent: developer

# 3. Run the daemon (in another shell).
autosk daemon serve --workers 1 &

# 4. The poller picks up the task; the developer agent spawns; at the
# end of its turn it calls (via the autosk pi-extension):
#
#     autosk step next $id --to review
#
# The daemon records the signal, advances the task to step=review,
# and the loop continues until validator → human.

# 5. Watch transitions.
while true; do
  autosk show "$id" --json | jq -r '"status=\(.status) step=\(.current_step // \"-\")"'
  sleep 2
done

# 6. Resume after human review.
autosk resume "$id"          # back to status=work at validator
# … the workflow continues; eventually validator → done (via `done`
# transition you added, or a human running `autosk done $id`).
```

The single-agent shorthand:

```bash
id=$(autosk create "Bump version to 0.2" --agent @autosk/developer --json | jq -r .id)
autosk show "$id"
# status: work, workflow: single:@autosk/developer, current_step: do

# The developer agent runs once, picks `--to done`, and the task closes.
```

### Enrolling an existing task

If a task already exists (typically `status='new'`, created without
`--workflow` / `--agent`), use `enroll` instead of recreating it:

```bash
autosk enroll as-bea9 --workflow feature-dev
autosk enroll as-bea9 --agent    @autosk/developer   # single:@autosk/developer
```

`enroll` is the post-creation mirror of `create --workflow` /
`create --agent`: it sets `workflow_id`, `current_step_id =
workflow.first_step` and flips status to `work`. Exactly one of
`--workflow` / `--agent` is required.

Only `status='new'` is accepted; other states get pointed at the right
verb instead:

| Current status   | Error hint                                                        |
|------------------|-------------------------------------------------------------------|
| `work`           | the daemon will advance this task — to switch workflows, `cancel + reopen + enroll`. |
| `human`          | use `autosk resume <id> [--to STEP]` to put it back into the workflow. |
| `done` / `cancel` | use `autosk reopen <id>` first.                                  |

`enroll --json` emits the same shape as `autosk show --json`
(including the derived `blocked` / `blocked_by` / `blocks` fields).

---

## Agent contract: `autosk step next`

When an agent runs **inside a workflow step** (i.e. the daemon spawned
it), it MUST call exactly one of:

```bash
autosk step next <task-id> --to <sibling-step-name>
autosk step next <task-id> --to done
autosk step next <task-id> --to cancel
autosk step next <task-id> --to human
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

For tasks in `human`, the agent has handed control back to a
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
- **Reopen**: `autosk reopen <id>` — `done`/`cancel` → `new`. Clears
  `current_step_id`; preserves `workflow_id` for audit.
- **Inspect / edit metadata**: `autosk metadata show <id>` (add
  `--visits-pretty` for a labelled visit-counter view),
  `autosk metadata set/unset <id> --key K …`, and
  `autosk metadata reset-visits <id> [--step NAME | --step-id ID]`
  — the escape hatch after a `step_max_visits_exceeded` park. See
  [Visit limits](#visit-limits-max_visits) above.

`autosk update --status` is rejected on `work` tasks: those
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
 WHERE t.status = 'work'
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
| **Transition** | One edge from a step. Carries either a sibling-step target or a `task_status` (`done`/`cancel`/`human`), plus a natural-language `prompt_rule`. |
| **Signal** | A row in `step_signals`. Inserted by `autosk step next` from inside the agent; consumed by the executor after end-of-turn. PK on `run_id` enforces "exactly one signal per run". |
| **Kickback** | A corrective message sent to the agent when it ends a turn without emitting a signal. Bounded by `max_corrections`. |
| **Synthetic workflow** | `single:<agent>` workflow auto-created by `--agent NAME`. `is_synthetic = 1`; hidden from `workflow list` unless `--all`. |

---

## Pointers

- Design: [`docs/plans/20260517-Workflows-Plan.md`](plans/20260517-Workflows-Plan.md)
- Worktree isolation: [`docs/plans/20260521-Worktree-Isolation.md`](plans/20260521-Worktree-Isolation.md)
- Daemon details: [`docs/daemon.md`](daemon.md)
- pi-extension surface: [`extension/src/`](../extension/src/)
- Example workflow: [`docs/notes/workflow-example.json`](notes/workflow-example.json)
- Isolated example: [`docs/notes/workflow-isolated-example.json`](notes/workflow-isolated-example.json)
