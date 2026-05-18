# autosk for AI agents

Use `autosk` as your durable task list during coding work. It is designed
for `ready → claim → done` loops.

## The 30-second rules

1. **Never edit `./.autosk/db` directly.** Always go through `autosk` CLI.
2. **Pick work via `autosk ready --json`**, not `list`. `ready` filters out
   blocked and in-progress tasks for you.
3. **`autosk claim <id>` is idempotent.** Calling it twice in a row is fine.
   It's how you mark "I'm working on this".
4. **Close tasks with `autosk done <id>`.** Use `cancel` for abandoned work.
5. **Express blockers with `autosk block <id> <blocker-id>`**, not by
   writing prose into the description.

## Canonical agent loop

```bash
while true; do
  next_json=$(autosk ready --json)
  id=$(echo "$next_json" | jq -r '.[0].id // empty')
  [ -z "$id" ] && break

  autosk claim "$id"
  # ... do the work ...
  autosk done "$id"
done
```

## Creating decomposed work

When a task is too big, decompose it before claiming. Each subtask is a real
task with a blocker edge:

```bash
parent=$(autosk create "Refactor auth module" -p 1)
a=$(autosk create "Extract token validation" --blocks "$parent")
b=$(autosk create "Move JWT signing to its own pkg" --blocks "$parent")
```

`parent` will not appear in `autosk ready` until both `a` and `b` are done.

## When you discover a blocker mid-task

```bash
new_blocker=$(autosk create "External service spec is missing")
autosk block <task-you-claimed> "$new_blocker"
```

Your in-progress task stays `claimed`; `ready` will not surface it again, and
`show` will report `blocked: true`.

## JSON shape (`autosk show --json`)

```json
{
  "id": "as-a1b2",
  "title": "Wire up the auth flow",
  "description": "...",
  "status": "claimed",
  "priority": 1,
  "created_at": "2026-05-13T10:00:00Z",
  "updated_at": "2026-05-13T11:42:13Z",
  "blocked": false,
  "blocked_by": [],
  "blocks": ["as-3c4d"]
}
```

`list` and `ready` return arrays of these. The shape is stable across patch
releases; renames / removals are breaking changes.

## When you're running inside a workflow step (v0.2)

If the daemon spawned you to work on an autosk task, you are operating
**inside a workflow step**. Before you stop your turn, you MUST call
`autosk step next` exactly once with one of the targets listed at the
top of your prompt:

```bash
autosk step next <as-id> --to <sibling-step-name>
autosk step next <as-id> --to done
autosk step next <as-id> --to cancelled
autosk step next <as-id> --to human_feedback
```

Valid targets are spelled out per step in your initial prompt (one bullet
per outgoing transition). The autosk pi-extension exposes this as the
`step_next` action; either form works.

If you stop without recording a transition, the daemon sends you a
corrective message and reruns the step. After `max_corrections` (default
3) attempts the run is marked `failed`.

**Do not** call `autosk done`/`cancel`/`update --status` while you're
inside a step — those are owned by the workflow engine. Use `step next`.

## When a daemon is running

If `autosk daemon serve` is running for the project, prefer **creating**
your next task in a workflow (or with `--agent`) so the engine picks it
up automatically:

```bash
autosk create "Implement auth module" --workflow feature-dev
autosk create "Bump version" --agent @autosk/developer  # single:@autosk/developer
```

The poller (default cadence 2s) surfaces `in_workflow` tasks whose
current step's agent is non-human. The executor resolves the agent's
config from the installed npm package at
`~/.autosk/packages/node_modules/<pkg>/package.json` (managed via
`autosk agent install/uninstall`) and dispatches to one of two
branches:

- standard → spawn `pi --mode rpc` with the package's settings
  (model, thinking, first-turn message, pi_extensions, pi_skills).
- custom → spawn `@autosk/agent-runtime` to run the package's
  TS/JS runner module.

Referencing an agent name that isn't installed produces
`agent_not_installed: <name>` at task-create / workflow-create time
(and at executor spawn time as a fail-fast).

`autosk daemon status <job-id>` and `autosk daemon messages <job-id>`
show the run's lifecycle and the transcript pi/the runner is writing.
See [`docs/daemon.md`](docs/daemon.md) and
[`docs/workflows.md`](docs/workflows.md).

## What autosk is NOT

- Not a comment thread. Use commit messages or PR descriptions for that.
- Not a workflow engine (yet). Any status can transition to any status.
- Not multi-writer (yet). Concurrent agents serialize on a file lock; if
  you see `database is locked`, your process is being asked to wait.
- Not federated. `autosk` lives entirely inside `./.autosk/`.

## Useful flags

| Flag | Effect |
|---|---|
| `--json` | Machine-readable output (works on every read command). |
| `--quiet` / `-q` | Suppress non-essential prints (good for scripted use). |
| `--db <path>` | Override database discovery. |
