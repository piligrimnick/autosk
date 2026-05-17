# autosk — workflows & agents plan

**Date:** 2026-05-17
**Status:** Spec drafted from ask_user round 2026-05-17 and the maintainer's
markdown review of the first draft. **Proposed** sub-decisions are marked
`(P)` and need a final yes/no from the maintainer before phase D0 starts.
**Predecessors:** [`20260513-Init-Plan.md`](20260513-Init-Plan.md),
[`20260513-Impl-Plan.md`](20260513-Impl-Plan.md),
[`20260517-Daemon-Plan.md`](20260517-Daemon-Plan.md).
**Compat:** none. The v0.1 schema is dropped. No migration of existing data.
Existing DBs need a wipe & re-init.

---

## 1. Purpose

Turn autosk from "todo list + dumb worker" into "todo list + a directed
graph of agents that work the task end-to-end".

We introduce two first-class concepts:

- **agents** — named actors that can own a task. `human` is one of them.
- **workflows** — a small graph of `steps`; each step has an agent and a
  list of `transitions` annotated with `prompt_rule` text. The agent reads
  the rules and emits the next transition explicitly.

The CLI shape we want to ship:

```bash
autosk workflow create --file docs/notes/workflow-example.json
autosk create "Implement auth module with jwt..." --workflow feature-dev
# → status=in_workflow, current_step=<first step of feature-dev>
# → daemon picks it up, spawns the step's agent

autosk create "Implement auth module" --agent developer
# → status=in_workflow, workflow=single:developer, current_step=do
# → daemon picks it up; the single-step workflow lets the agent emit
#   step next --to {done|cancelled|human_feedback} as the only transitions
```

Notably, "the agent currently working on a task" is **always derived from
`tasks.current_step_id → steps.agent_id`**. There is no second pointer.

---

## 2. Decisions

### 2.1 Locked (from ask_user 2026-05-17)

| Topic | Decision |
|---|---|
| **Router** | Agent emits an **explicit structural signal** at end-of-turn. No separate LLM "judge". The agent calls the autosk pi-extension command `autosk step next <task-id> --to <name>`, where `<name>` is either a sibling step name or a `task_status` value listed in the step's transitions. `prompt_rule` text is **shown to the agent in its initial prompt** as guidance (not consumed by the daemon). |
| **Agents** | DB stores **only** `id, name, is_human`. Execution config (model, thinking, system prompt, extra pi args, allowed tools) lives in `.autosk/agents/<name>.{toml,json}` next to the project. Tracked in git. The daemon reads the file at spawn time. |
| **Workflow storage** | **Normalized** tables `workflows`, `steps`, `step_transitions`. `workflow create --file foo.json` parses the JSON and inserts the three sets in one transaction. |
| **human_feedback resume** | **Explicit command**: `autosk resume <task-id> [--to <step>]`. Comments alone do not resume. (Comments are still surfaced to the next step's prompt, see §5.7.) |
| **`--agent` without `--workflow`** | Sugar over a real workflow. autosk lazily creates a hidden workflow named `single:<agent>` with one step `do` whose three transitions are `task_status ∈ {done, cancelled, human_feedback}`. The executor has **one** code path — workflows are universal. |
| **No `assigned_agent_id` on `tasks`** | The current agent is derived: `tasks.current_step_id → steps.agent_id`. For `status ∈ {done, cancelled, new}` there is no current agent. |
| **No `git_branch` on `tasks`** | Deferred until worktree-isolated runs land. Not in v0.2 schema. |

### 2.2 Proposed (need confirmation)

| ID | Topic | Proposal |
|---|---|---|
| **P1** | Status set | Drop `claimed`. New status set is **exactly** `{new, in_workflow, human_feedback, done, cancelled}`. `autosk claim` becomes `autosk assign <id> --agent <name>` (sugar: `--agent human` for the old "I'm working on it"). |
| **P3** | Author of a task | `tasks.author_id` is auto-set to the agent the CLI is running as. `autosk` resolves the caller via `$AUTOSK_AGENT` env var (default `human`). If the agent doesn't exist in the DB, autosk inserts it lazily with `is_human=0` (or `=1` for the literal name `human`). |
| **P5** | Daemon model | The existing `autosk daemon serve` becomes the workflow engine. **One `daemon_runs` row per step execution**, not per task. The engine picks up `in_workflow` tasks whose current step's agent is non-human and not currently being run, spawns a run for the current step, observes a transition signal, advances the task, and immediately enqueues the next run. |
| **P6** | Kickback loop fate | Keep the kickback mechanism, but **closure becomes the transition signal**. A step run is "valid" iff the agent emitted exactly one `autosk step next` call. Invalid → corrective message → up to `max_corrections` then `failed`. The old `done|cancelled|new-blocker` closure rules are **gone** — `single:<agent>` covers what `--agent` used to cover. |
| **P7** | Comments surface | Comments are appended to a step's prompt **in chronological order** at spawn time, prefixed `[<agent>@<ts>]: <text>`. Comments are not surfaced mid-turn. |
| **P8** | `pi-rpc-contract.md` | The pi-extension `step next` tool is a **new** tool exposed by the autosk pi-extension. v1 just records the call; pi exits the turn normally afterwards. No new RPC frame types. |
| **P9** | `autosk done` / `cancel` for humans | Kept. Behave as a direct shortcut for "set status to done|cancelled" — they do **not** go through `step_signals`. The daemon never picks up tasks whose current step's agent is `human`, so this can't race with a running step. |
| **P10** | Keep `reopen` | `autosk reopen` survives: `done|cancelled → new`, clears `workflow_id` and `current_step_id`. Cheap, useful. |

If you push back on any `(P)` item, only that row needs to change; the rest
of the plan is stable.

---

## 3. High-level shape

```
                          ┌─────────────────────────────────────┐
  autosk CLI ──────┐      │           autosk daemon serve       │
                   │      │                                     │
  workflow create  │      │   ┌────────────┐   ┌────────────┐   │
  agent create     ├────▶ DB │  poller     │──▶│ scheduler  │   │
  create  -w/-a    │      │   │ in_workflow│   │  N workers │   │
  step next        │      │   │ non-human  │   └─────┬──────┘   │
  resume / assign  │      │   │  current   │         │          │
  comment add      │      │   │   step     │         ▼          │
  done / cancel    │      │   └────────────┘ ┌────────────────┐ │
                   │      │                  │ step executor  │ │
  human user ──────┘      │                  │ - render prompt│ │
                          │                  │ - spawn pi rpc │ │
                          │                  │ - watch signal │ │
                          │                  │ - kickback     │ │
                          │                  │ - advance task │ │
                          │                  └────────────────┘ │
                          └─────────────────────────────────────┘
                                            │
                                            ▼
                              pi --mode rpc (agent config)
                                  │
                                  ▼
                       autosk pi-extension tools:
                       - autosk show/create/comment/...
                       - autosk step next <id> --to <name>   (NEW)
```

Single binary, single process, single DB. The poller and the scheduler are
inside the daemon. The agent's `pi` child is the only external process.

---

## 4. Data model

### 4.1 Schema (replaces v0.1)

We **rewrite** `001_init.sql` rather than stacking migrations. `002_daemon_runs.sql`
is also rewritten because the run row's semantics change. There is no
migration from v0.1 → v0.2 — users wipe `.autosk/db` and re-init.

```sql
-- 001_init.sql (v0.2)

CREATE TABLE agents (
  id         TEXT PRIMARY KEY,          -- "ag-XXXX"
  name       TEXT NOT NULL UNIQUE,      -- "human", "developer", "code-reviewer", ...
  is_human   INTEGER NOT NULL DEFAULT 0 CHECK (is_human IN (0,1)),
  created_at INTEGER NOT NULL
);
-- Seeded on init: one row {name: 'human', is_human: 1}.

CREATE TABLE workflows (
  id            TEXT PRIMARY KEY,        -- "wf-XXXX"
  name          TEXT NOT NULL UNIQUE,    -- "feature-dev", "single:developer", ...
  description   TEXT NOT NULL DEFAULT '',
  first_step_id TEXT NOT NULL,           -- FK steps(id); set after steps insert
  is_synthetic  INTEGER NOT NULL DEFAULT 0
                CHECK (is_synthetic IN (0,1)),  -- 1 for single:<agent> workflows
  created_at    INTEGER NOT NULL
);

CREATE TABLE steps (
  id          TEXT PRIMARY KEY,          -- "st-XXXX"
  workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,             -- "dev", "review", "validator", or "do"
  agent_id    TEXT NOT NULL REFERENCES agents(id),
  UNIQUE (workflow_id, name)
);

CREATE TABLE step_transitions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id      TEXT NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
  next_step_id TEXT REFERENCES steps(id) ON DELETE CASCADE,  -- nullable
  task_status  TEXT,                                          -- nullable; one of human_feedback|done|cancelled
  prompt_rule  TEXT NOT NULL,
  CHECK ((next_step_id IS NULL) <> (task_status IS NULL)),    -- exactly one
  CHECK (task_status IS NULL OR task_status IN ('human_feedback','done','cancelled'))
);
CREATE INDEX idx_step_transitions_step ON step_transitions(step_id);

CREATE TABLE tasks (
  id              TEXT PRIMARY KEY,
  title           TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'new'
                  CHECK (status IN ('new','in_workflow','human_feedback','done','cancelled')),
  priority        INTEGER NOT NULL DEFAULT 2 CHECK (priority BETWEEN 0 AND 3),
  author_id       TEXT REFERENCES agents(id),
  workflow_id     TEXT REFERENCES workflows(id),
  current_step_id TEXT REFERENCES steps(id),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  -- Invariant (enforced by app, not DB): status='in_workflow'  ⇔  current_step_id IS NOT NULL.
  -- Invariant: current_step_id IS NOT NULL ⇒ workflow_id IS NOT NULL AND
  --           steps[current_step_id].workflow_id = workflow_id.
  CHECK (
    (status = 'in_workflow' AND current_step_id IS NOT NULL)
    OR (status <> 'in_workflow' AND (status <> 'new' OR current_step_id IS NULL))
  )
);
CREATE INDEX idx_tasks_status_prio ON tasks(status, priority, created_at);
CREATE INDEX idx_tasks_step        ON tasks(current_step_id);

CREATE TABLE task_deps (
  blocker_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  blocked_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL DEFAULT 'blocks' CHECK (kind = 'blocks'),
  PRIMARY KEY (blocker_id, blocked_id, kind),
  CHECK (blocker_id <> blocked_id)
);
CREATE INDEX idx_deps_blocked ON task_deps(blocked_id);

CREATE TABLE comments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  author_id  TEXT NOT NULL REFERENCES agents(id),
  text       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_comments_task ON comments(task_id, created_at);
```

```sql
-- 002_daemon_runs.sql (v0.2)
-- One row per STEP execution. step_id is always set, because every running
-- task is in a workflow (real or synthetic single:<agent>).

CREATE TABLE daemon_runs (
  job_id            TEXT PRIMARY KEY,                                 -- "job-XXXXXX"
  task_id           TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  step_id           TEXT NOT NULL REFERENCES steps(id) ON DELETE RESTRICT,
  status            TEXT NOT NULL CHECK (status IN ('queued','running','done','failed','cancelled')),
  transition_id     INTEGER REFERENCES step_transitions(id),         -- set on success
  exit_code         INTEGER,
  pid               INTEGER,
  pi_session_id     TEXT,
  session_path      TEXT,
  error             TEXT,
  corrections_used  INTEGER NOT NULL DEFAULT 0,
  max_corrections   INTEGER NOT NULL DEFAULT 3,
  created_at        INTEGER NOT NULL,
  started_at        INTEGER,
  finished_at       INTEGER
);
CREATE INDEX idx_runs_task   ON daemon_runs(task_id, created_at);
CREATE INDEX idx_runs_status ON daemon_runs(status, created_at);

CREATE TABLE step_signals (
  run_id        TEXT PRIMARY KEY REFERENCES daemon_runs(job_id) ON DELETE CASCADE,
  task_id       TEXT NOT NULL,
  transition_id INTEGER NOT NULL REFERENCES step_transitions(id),
  created_at    INTEGER NOT NULL
);
```

**Notes**

- `prompt`, `model`, `thinking`, `cwd`, `auto_claim`, `pre_blocked_by`, `closure_kind`, and `agent_id` are **gone** from `daemon_runs`. The model/thinking come from the agent config file; the cwd comes from the daemon's `--cwd`; the prompt is rendered fresh from `(task, current_step, comments)` at spawn time; the agent is derived from `step_id → steps.agent_id`; on success we record `transition_id` and that's it.
- `step_transitions.id` is `INTEGER AUTOINCREMENT` because transitions are never referenced by users from the CLI; only the engine writes/reads them.
- `step_signals` is a staging table written by the pi-extension `step next` tool and consumed by the executor. `PK(run_id)` enforces "exactly one signal per run".
- The CHECK constraint on `tasks` is the closest thing SQL gives us to expressing "`status` and `current_step_id` move together"; the app layer enforces the rest of the invariants.

### 4.2 Agent config files

Path: `<project>/.autosk/agents/<name>.toml` (TOML chosen over JSON for
human-readability; can flip in P-review).

```toml
# .autosk/agents/developer.toml
model        = "sonnet:high"
thinking     = "high"
system_prompt = """
You are the developer agent in the feature-dev workflow.
You implement the task in this codebase, then call `autosk step next <id> --to review`.
"""
extra_args   = ["--no-tool", "web_fetch"]
```

The daemon reads the file at spawn time. Missing/invalid file → run fails
fast with `error=agent_config_invalid`. Editing the file affects only **new**
spawns; in-flight runs use the snapshot they spawned with.

`human` does not need a config file: the poller filters out tasks whose
current step's agent is human, so the executor never spawns for it.

### 4.3 ID shapes

| Entity | Prefix | Width |
|---|---|---|
| task | `as-` | 4 hex (unchanged) |
| job | `job-` | 6 hex (unchanged) |
| agent | `ag-` | 4 hex |
| workflow | `wf-` | 4 hex |
| step | `st-` | 4 hex |

`step_transitions.id` and `comments.id` are plain auto-increment; users never type them.

---

## 5. Workflow engine

### 5.1 States & transitions for `tasks`

```
   ┌─────┐  --workflow / --agent      ┌─────────────┐  agent emits
   │ new │ ─────────────────────────▶ │ in_workflow │ ────────────┐
   └──┬──┘                            └──┬────────┬─┘  step next  │
      │ assign --agent N                 │        │   --to <task_status>
      │ (uses single:N)                  │        │               │
      ▼                                  │        ▼               │
   in_workflow ◀────────────────────────┘  ┌────────────────┐    │
                          autosk resume    │ human_feedback │    │
                                           └────────────────┘    │
                                                                  ▼
                                                         done | cancelled
```

Rules:

- `status = new` ⇒ `current_step_id = NULL` and `workflow_id` is either NULL (task created without `--workflow`/`--agent` and not yet assigned) or set (created with one but not yet engine-picked — but in practice the engine sets `in_workflow` at create time, so this combination is rare).
- `status = in_workflow` ⇒ `current_step_id` IS NOT NULL ⇒ `workflow_id` IS NOT NULL.
- `human_feedback`: `current_step_id` preserved (so `resume` knows where to come back); `workflow_id` preserved.
- `done` / `cancelled`: sticky until `autosk reopen` (P10); `current_step_id` cleared, `workflow_id` kept for reference.

**There is no `assigned_agent_id` field.** The current agent is
`steps[current_step_id].agent_id`. Past assignments are visible via
`daemon_runs` rows (each run pinpoints both step and agent through it).

### 5.2 Poller

The daemon polls every `--poll-interval` (default `2s`):

```sql
SELECT t.id
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

Each row found is enqueued (creates a `daemon_runs` row in `queued`).

### 5.3 Step executor (single path)

```
1. MarkRunning(job_id).
2. Render the prompt from (task, current_step, comments):
     - task.title + task.description
     - "You are agent '<name>' on step '<step.name>' of workflow '<wf.name>'."
     - Outgoing transitions, each rendered as:
         "- to advance to <step|task_status:<x>>: <prompt_rule>"
     - "When done, call: autosk step next <task-id> --to <name>."
     - Recent comments, oldest first.
3. Spawn pi --mode rpc using the agent config file.
4. SendPrompt(rendered prompt).
5. WaitForAgentEnd.
6. Look up step_signals.run_id = job_id:
     - present and valid → mark run done(transition_id=…), advance task
       atomically (§5.4); poller picks up the next step (or terminal
       status flips the task).
     - absent → kickback (corrective message), up to max_corrections;
       then fail with error=agent_did_not_emit_transition.
     - present but agent invoked `step next` twice → second call already
       rejected at insert time (PK violation); first signal stands.
7. Clean shutdown of pi.
```

No `--agent`-vs-`--workflow` branching. `single:<agent>` is just a workflow.

### 5.4 `step next` semantics & atomic advance

`autosk step next <task-id> --to <name>` is a pi-extension tool (new). It:

- Resolves the active run (latest `daemon_runs` row for `task_id` with `status='running'`).
- Resolves `<name>` against the step's outgoing transitions: either a sibling step name or one of the allowed `task_status` values.
- **Persists the signal** into `step_signals(task_id, run_id, transition_id, created_at)`. It does **not** mutate `tasks` directly — the daemon owns the advance.

Advance, performed by the daemon in one transaction after the signal is observed:

- `transition.next_step_id` set → `tasks.current_step_id = next_step_id`, `status='in_workflow'`.
- `transition.task_status='human_feedback'` → `current_step_id` preserved, `status='human_feedback'`.
- `transition.task_status='done'` / `'cancelled'` → `status` flipped, `current_step_id=NULL`. `workflow_id` left alone for audit.

### 5.5 `single:<agent>` synthetic workflows

When the user runs `autosk create … --agent foo` or
`autosk assign <id> --agent foo` and the workflow `single:foo` does **not**
exist yet, autosk creates it transactionally:

```jsonc
{
  "name": "single:foo",
  "description": "Auto-generated single-agent workflow for foo.",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": "foo",
      "next_steps": [
        {"task_status": "done",            "prompt_rule": "When the work is complete."},
        {"task_status": "cancelled",       "prompt_rule": "When the task cannot be completed."},
        {"task_status": "human_feedback",  "prompt_rule": "When you need a human decision or input."}
      ]
    }
  }
}
```

`workflows.is_synthetic=1` so `autosk workflow list` can hide them by
default (`--all` shows them). `workflow delete single:foo` is allowed only
when no tasks reference it.

### 5.6 Resume

`autosk resume <id> [--to <step>]`:

- Requires `tasks.status='human_feedback'`.
- `--to` defaults to `current_step_id` (i.e. retry the same step with the new comments). If `--to <other-step>`, jump to that step in the **same** workflow.
- Sets `status='in_workflow'`. Poller picks it up.

For tasks whose workflow is `single:<agent>`, `--to` is unnecessary (only
one step exists); `resume <id>` is enough.

### 5.7 Comments

`autosk comment add <task-id> <text>` (alias: `autosk comment <task-id> <text>`):

- `author_id` defaults to the caller's agent identity (P3).
- Plain insert into `comments`. No status mutation.

Comments are surfaced **on the next step spawn** in the rendered prompt
(P7). v1 includes them all, oldest first; if this grows unmanageable, we
add `--since-step` filtering in v1.1.

---

## 6. CLI surface

### 6.1 New / changed commands

```
Tasks (changed)
  autosk create [title] [-d desc | -d -] [-p N]
                [--workflow NAME | --agent NAME]
                [--blocks ID]... [--blocked-by ID]...
  autosk assign <id> --agent NAME       # NEW; replaces `claim`
  autosk resume <id> [--to STEP]        # NEW; out of human_feedback
  autosk done  <id>                     # direct flip; status → done
  autosk cancel <id>                    # direct flip; status → cancelled
  autosk reopen <id>                    # done|cancelled → new; clears step

Workflows (new)
  autosk workflow create --file PATH
  autosk workflow list [--all]          # --all also shows single:* synthetics
  autosk workflow show <name>
  autosk workflow delete <name>         # refuses if any task references it

Agents (new)
  autosk agent create <name> [--human]  # also lazily on first --agent / env-set
  autosk agent list
  autosk agent show <name>

Steps (new — agent-facing)
  autosk step next <task-id> --to <step-or-status>

Comments (new)
  autosk comment add <task-id> <text> [--author NAME]
  autosk comment list <task-id> [--json]

Removed
  autosk claim       # → use `autosk assign <id> --agent human`
```

`autosk assign <id> --agent N`:

- From `new`: ensures `single:N` exists, sets `workflow_id`, `current_step_id`, status=`in_workflow`.
- From `in_workflow` or `human_feedback`: **rejected** (`error: task is already in a workflow; cancel and recreate, or resume`).
- From `done`/`cancelled`: **rejected** (`error: reopen first`).

`autosk daemon serve` learns one new flag, `--poll-interval`, default `2s`.
The `daemon submit` flow keeps working as a manual escape hatch (single
job, bypasses the poller), but `autosk create --workflow|--agent` is the
primary entry point.

### 6.2 `workflow create --file` input format

The JSON in `docs/notes/workflow-example.json` is the v1 input. We fix the
typo at the schema level (`next_steps`, not `next_setps`) and add `description`:

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

Validation on create:

- `name` unique; reserved prefix `single:` is rejected (users can't create synthetic-shaped names).
- Every `agent` referenced must exist in `agents` table.
- Every `step` ref must exist within this workflow's `steps`.
- `task_status` must be in `{human_feedback, done, cancelled}`.
- `first_step` must be a key in `steps`.
- Each step has ≥1 transition (otherwise it would be a dead-end).
- Cycles between named steps are allowed (`dev ↔ review`) — author owns termination.

---

## 7. Drop list (what we remove from v0.1)

| Removed | Replacement |
|---|---|
| `tasks.status='claimed'` | `autosk assign <id> --agent human` (status → `in_workflow` via `single:human`). |
| `autosk claim` | `autosk assign`. |
| `tasks.assigned_agent_id` (was in plan draft) | Derived: `steps[current_step_id].agent_id`. |
| `tasks.git_branch` (was in plan draft) | Deferred until worktree-isolated runs land. |
| `daemon_runs` columns: `auto_claim`, `pre_blocked_by`, `prompt`, `model`, `thinking`, `cwd`, `closure_kind`, `agent_id` | Derived per spawn from `(task, step, agent-config)`; `agent_id` from `step_id`; success is recorded as `transition_id`. |
| `verifyClosure` "done\|cancelled\|decomposed" path | Gone. Every run is a workflow run; closure = exactly one row in `step_signals`. |
| "blocked is a stored thing" | Still derived. Unchanged. |

No backward-compat shims, no migration. `autosk migrate` on a v0.1 DB
reports `schema_v1_unsupported` and tells the user to wipe `.autosk/db`.

---

## 8. Phases

Sized one autosk task per phase. Dependencies are linear except where
noted.

| ID | Phase | Done when |
|---|---|---|
| **W0** | **Lock proposals** | Open issue/PR with §2.2 checked off. `(P)` items become locked. |
| **W1** | **Schema rewrite** | `001_init.sql` & `002_daemon_runs.sql` replaced; `migrations.go` refuses v0.1 DBs with a friendly error. Conformance tests cover all new tables. |
| **W2** | **Agents: store + CLI** | `agents` table, `autosk agent create/list/show`, lazy insert on env-set caller, `human` seeded on init. |
| **W3** | **Workflows: store + CLI + synthetic** | `workflows`/`steps`/`step_transitions` tables, `autosk workflow create --file/list/show/delete`. JSON parser + validator in `internal/workflow/`. `EnsureSingle(agent)` helper used by W4. |
| **W4** | **Tasks: new shape** | Task table fields wired in store + render + golden tests. `create --workflow` and `create --agent` (auto-uses `single:<agent>`) set `status='in_workflow'` and the right FKs. `assign`, `resume`, `done`, `cancel`, `reopen` updated. |
| **W5** | **Comments** | Table, CLI (`comment add/list`), prompt-surfacing helper. |
| **W6** | **Step executor (single path)** | Rewrite `internal/daemon/executor` for workflow runs: render prompt from (task, step, comments), spawn pi via agent-config file, watch for a `step_signals` row at end-of-turn, advance task on success, kickback on miss. No `--agent` sibling branch. |
| **W7** | **`step next` pi-extension tool** | autosk pi-extension exposes `autosk step next <id> --to <name>`. CLI validates the transition target against the current step's outgoing edges and inserts into `step_signals`. Integration test with fake pi. |
| **W8** | **Poller** | Daemon polls `in_workflow` tasks where current step's agent is non-human and feeds the scheduler. Restart recovery: in-flight runs marked `failed/daemon_restart`; tasks stay `in_workflow` and get re-picked. |
| **W9** | **End-to-end test** | The acceptance scenario in §9 runs green against a fake pi that emits canned transition signals. |
| **W10** | **Docs + AGENTS.md update + workflow example** | `docs/workflows.md` (concept + CLI), refreshed `docs/notes/workflow-example.json` (typo fixed), AGENTS.md gains "when running inside a step, call `autosk step next` before stopping". Update README roadmap section. |

Each phase ends with a runnable artifact and lands as one PR. The
`(P)`-items confirmed in W0 are the contract; later phases don't revisit
them.

---

## 9. Acceptance scenario (W9)

```bash
# Setup
autosk init
autosk agent create developer
autosk agent create code-reviewer
autosk agent create task-validator
cat > .autosk/agents/developer.toml <<EOF
model = "sonnet:high"
thinking = "high"
system_prompt = "You are the developer agent."
EOF
# (same for code-reviewer, task-validator)

autosk workflow create --file docs/notes/workflow-example.json
# wf-XXXX

# Start a task in the workflow
id=$(autosk create "Implement auth module with jwt" --workflow feature-dev --json | jq -r .id)
autosk show "$id" --json | jq '{status, workflow_id, current_step}'
# → status=in_workflow, workflow_id=wf-XXXX, current_step=dev
# (current_agent is derived in render: "developer")

# Run the daemon (in another shell)
autosk daemon serve --workers 1 &

# Watch transitions
while true; do
  autosk show "$id" --json | jq -r '"status=\(.status) step=\(.current_step // \"-\")"'
  sleep 2
done
# expected sequence:
#   status=in_workflow step=dev
#   status=in_workflow step=review
#   status=in_workflow step=validator
#   status=human_feedback step=validator
#
autosk resume "$id"
# status flips back to in_workflow at step=validator, daemon retries
# eventually: status=done step=-
```

The fake pi used in W9 emits one `autosk step next` call per turn,
selecting transitions according to a small script.

A second acceptance run covers `--agent` (synthetic workflow):

```bash
id2=$(autosk create "Bump version to 0.2" --agent developer --json | jq -r .id)
autosk show "$id2" --json | jq '{status, workflow_id, current_step}'
# → status=in_workflow, workflow=single:developer, current_step=do

# Daemon runs the agent. The agent eventually emits:
#   autosk step next $id2 --to done
# → status=done.
```

---

## 10. Risks & open questions

| Risk | Mitigation |
|---|---|
| The pi-extension `step next` tool must reliably reach the autosk CLI. | The autosk pi-extension is shipped in this repo (`extension/`). The daemon sets `PATH` so its child resolves `autosk` from this build. |
| Agents emit zero or multiple `step next` calls. | The PK on `step_signals(run_id)` makes "exactly one" trivial to enforce. Zero → kickback. Two → second call rejected with `error="step_next_already_emitted"`. |
| Comments grow unbounded in long workflows. | v1 surfaces all comments. v1.1 adds `--since-step` or token-budget trimming. |
| Agents try to mutate `tasks.status` directly via `autosk update`. | We forbid `update --status` on `in_workflow` tasks. `done`/`cancel` for humans bypass `step_signals` (P9) but are only valid on tasks whose current step's agent is `human`, or on `human_feedback` tasks. |
| `human_feedback` resume loses context the agent had at the step. | The agent re-spawns with comments included; we accept losing in-RAM state. v1.1 could persist a "step memo" file. |
| Workflow definition drift between file and DB. | `workflow create` is one-shot. To "edit" a workflow, the user creates a new one with a new name and points new tasks at it. v1 does not support in-place edits. |
| Config files in `.autosk/agents/*` leak secrets. | Document: never put API keys in config files; pi reads them from env. README and `docs/workflows.md` repeat this. |
| Race: `autosk done <id>` runs while the daemon is mid-step for the same task. | Can't happen for non-human steps: the poller filters `is_human=0`, and `done` for non-human in-flight tasks is rejected. For `single:human` tasks the daemon never spawns, so `done` is always safe. |
| `single:<agent>` workflows pile up over time. | They are cheap (4 rows: 1 wf + 1 step + 3 transitions). `is_synthetic=1` lets `workflow list` hide them. `workflow delete` works once no tasks reference them. |
| Cycle `dev → review → dev → …` runs forever. | Out of scope for v1. The workflow author owns termination. |
| **Open:** Agent config format `.toml` vs `.json`. | Proposed TOML for readability; flip if the maintainer prefers consistency with `workflow-example.json`. |
| **Open:** Should `step next --to <step>` accept the step's **id** as well as its name? | Proposed no — names only. Step ids are an implementation detail. |

---

## 11. Layout summary

```
cmd/autosk/
  agent.go                # autosk agent create|list|show
  workflow.go             # autosk workflow create|list|show|delete
  comment.go              # autosk comment add|list
  step.go                 # autosk step next (agent-facing)
  resume.go               # autosk resume
  assign.go               # autosk assign  (replaces claim)
  create.go               # +--workflow / --agent flags
internal/
  agent/
    store.go              # CRUD over agents (incl. lazy create)
    config.go             # read .autosk/agents/<name>.toml
  workflow/
    parse.go              # JSON → in-memory model
    validate.go           # well-formedness checks
    store.go              # CRUD over workflows/steps/step_transitions
    synthetic.go          # EnsureSingle(agent) for `--agent` sugar
  comments/
    store.go
  step/
    signals.go            # insert into step_signals; resolve --to
  daemon/
    poller.go             # NEW
    executor/             # rewritten; single workflow-run path
    server.go             # existing routes + /v1/workflows, /v1/agents (read-only)
  migrations/
    001_init.sql          # REWRITTEN (v0.2)
    002_daemon_runs.sql   # REWRITTEN (v0.2)
docs/
  workflows.md            # concept doc, CLI examples, security notes
  plans/
    20260517-Workflows-Plan.md  # this file
  notes/
    workflow-example.json # typo fixed (next_steps), description added
```

---

## 12. Tracking

Per AGENTS.md, each phase (W0…W10) is one autosk task. Dependencies
expressed via `autosk block` edges; umbrella task `Workflows & agents v0.2`
is blocked by all of W1…W10. Phase W0 (proposals lock) blocks all others.
