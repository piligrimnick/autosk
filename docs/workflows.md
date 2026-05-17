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

A named actor that can own a task. Stored in the `agents` table with the
minimal shape `(id, name, is_human)`. `human` is seeded on first migrate.
Other agents are created either explicitly:

```bash
autosk agent create developer
autosk agent create code-reviewer
autosk agent create task-validator
```

…or lazily on first use via `$AUTOSK_AGENT` (the env var the CLI uses to
identify its caller).

The **executable configuration** of an agent (model, thinking depth,
system prompt, extra pi flags) lives outside the DB in
`.autosk/agents/<name>.toml`:

```toml
# .autosk/agents/developer.toml
model         = "sonnet:high"
thinking      = "high"
system_prompt = """
You are the developer agent. Implement the task in this codebase
and run the tests before handing off to the reviewer.
"""
extra_args    = ["--no-tool", "web_fetch"]
```

The daemon reads this file at spawn time. The `human` agent does not
need a config file — the workflow engine never tries to spawn it.

> **Security:** never put API keys or other secrets in these files.
> The daemon inherits the environment from the user who started it;
> pi picks up its own credentials from env.

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
    "dev":       { "agent": "developer",     "next_steps": [{"step":"review", "prompt_rule":"…"}] },
    "review":    { "agent": "code-reviewer", "next_steps": [
                     {"step":"validator", "prompt_rule":"…"},
                     {"step":"dev",       "prompt_rule":"…"}
                   ]},
    "validator": { "agent": "task-validator","next_steps": [
                     {"step":"dev",                    "prompt_rule":"…"},
                     {"task_status":"human_feedback",  "prompt_rule":"…"}
                   ]}
  }
}
```

A `next_steps[]` entry has either a `step` (sibling step name in the
same workflow) or a `task_status` (one of `done`, `cancelled`,
`human_feedback`), never both. `prompt_rule` is natural-language guidance
shown to the agent in its prompt — the agent uses it to decide which
transition to emit.

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

This mirrors the W9 acceptance scenario (see plan §9).

```bash
# 1. Bootstrap.
autosk init
autosk agent create developer
autosk agent create code-reviewer
autosk agent create task-validator

cat > .autosk/agents/developer.toml <<EOF
model         = "sonnet:high"
thinking      = "high"
system_prompt = "You are the developer agent."
EOF
# … repeat for the other two agents …

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
id=$(autosk create "Bump version to 0.2" --agent developer --json | jq -r .id)
autosk show "$id"
# status: in_workflow, workflow: single:developer, current_step: do

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
