# pi-autosk

pi extension that wraps the [autosk](../README.md) CLI as a small set of
LLM-callable tools. The agent stops shelling out to `bash autosk ...` and gets
a structured, rendered task tracker instead.

## Layout

```
extension/
├── package.json          # pi.extensions → ./src/index.ts
├── tsconfig.json
├── README.md
├── test/
│   └── smoke.ts          # end-to-end smoke against a real `autosk` binary
└── src/
    ├── index.ts          # extension factory: registers the three tools
    ├── cli.ts            # spawn + error normalization + JSON parsing
    ├── types.ts          # wire types (Task, Comment, StepSignal, details union)
    ├── task.ts           # tool `autosk_task`     — create / update / show
    ├── comment.ts        # tool `autosk_comment`  — add / list
    └── step.ts           # tool `autosk_step`     — next
```

## Tool surface

Three tools, each with a `{ action, args }` shape:

### `autosk_task`

| action  | required args                                         | optional args                                                                 |
|---------|-------------------------------------------------------|-------------------------------------------------------------------------------|
| create  | `title`                                               | `description`, `priority` (0..3), `blocks[]`, `blocked_by[]`, `workflow`, `agent`, `step` |
| update  | `id` + at least one of `title`/`description`/`priority`/`status` | —                                                                  |
| show    | `id`                                                  | —                                                                             |

`details = { kind: "task", domain: "task", action, task }`. The `task` object
carries `blocked_by`, `blocks`, `blocked`, plus `workflow_id` / `current_step`
/ `current_agent` when the task is inside a workflow.

`workflow` and `agent` are mutually exclusive. `step` requires `workflow`.
Status edits on `in_workflow` tasks are rejected by autosk — advance via
`autosk_step` instead.

### `autosk_comment`

| action | required args            | optional args |
|--------|--------------------------|---------------|
| add    | `task_id`, `text`        | `author`      |
| list   | `task_id`                | —             |

`details = { kind: "comment", ... }` for `add`,
`details = { kind: "comments", task_id, comments }` for `list`. Comments are
immutable; "edits" are appended as new comments.

### `autosk_step`

| action | required args            |
|--------|--------------------------|
| next   | `task_id`, `to`          |

`to` is a sibling step name in the current workflow, or one of
`done` / `cancelled` / `human_feedback`. Records a `step_signals` row that the
autosk daemon consumes after the agent's turn ends.

`details = { kind: "step_signal", domain: "step", action: "next", signal: {...} }`.

`step next` may be called **at most once per active run**; calling it without
an active run (e.g. on a `status='new'` task) returns `kind: "error"` with
`reason: "cli_error"`.

### Errors

Every tool normalizes failures into:

```
details = {
  kind: "error",
  domain: "task" | "comment" | "step",
  action: <string>,
  reason: "missing_binary" | "invalid_args" | "cli_error" | "parse_error" | "aborted",
  message,
  stderr?, stdout?,
}
```

## Renderer

Each tool ships its own `renderCall` / `renderResult`:

- `autosk_task`    — single-task block (id, status badge, title, description,
  workflow/step/agent line, dep summary).
- `autosk_comment` — table-style list, or single-comment block on `add`.
- `autosk_step`    — `task_id  run=…` header + colored target line + rule.
- error            — red one-liner + dim detail (consistent across tools).

## Install (development)

The `autosk` binary must be on `$PATH`:

```bash
cd ~/me/dev/autosk
make build           # writes ./bin/autosk
export PATH="$PWD/bin:$PATH"   # or symlink into /usr/local/bin
```

Then expose the extension to pi. Pick one:

```bash
# 1. Per-project: drop a symlink so pi auto-discovers it.
mkdir -p .pi/extensions
ln -s ~/me/dev/autosk/extension .pi/extensions/autosk

# 2. Global: same, under your pi config dir.
mkdir -p ~/.pi/agent/extensions
ln -s ~/me/dev/autosk/extension ~/.pi/agent/extensions/autosk

# 3. Explicit: add to ~/.pi/config.json
#    { "extensions": ["~/me/dev/autosk/extension"] }
```

pi reads `package.json#pi.extensions` and loads `./src/index.ts` from this
directory.

## Typecheck / smoke

```bash
cd extension
npm install
npm run typecheck

# Real end-to-end (needs `autosk` on PATH):
node --experimental-strip-types test/smoke.ts
```

The smoke test creates a throwaway `.autosk/db` in `$TMPDIR`, exercises every
positive path on `autosk_task` and `autosk_comment`, and verifies that
`autosk_step` propagates both `invalid_args` (missing `to`) and `cli_error`
(no active run) correctly.
