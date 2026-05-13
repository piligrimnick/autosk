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
