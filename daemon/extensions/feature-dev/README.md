# @autosk/feature-dev

The shipped **reference workflow** for autoskd v2: a full feature-development
cycle wired to [`@autosk/pi-agent`](../pi-agent) roles and
[`@autosk/worktree`](../worktree) isolation. It replaces v1's
`feature-dev-generic` bootstrap (design
`docs/plans/20260612-Bun-Daemon-Extensions.md` §3.6, P6).

## The workflow

```text
dev ──▶ review ──▶ docs ──▶ validator ──▶ accept (human)
 ▲        │                    │
 └────────┘────────────────────┘   (review→dev and validator→dev bounce-backs)
```

| Step        | Role                          | Notes                                                        |
| ----------- | ----------------------------- | ------------------------------------------------------------ |
| `dev`       | `@autosk/pi-agent/dev`        | first step; implements the task                              |
| `review`    | `@autosk/pi-agent/review`     | thorough code review (`thinking: xhigh`); bounces back to `dev` |
| `docs`      | `@autosk/pi-agent/docs`       | documentation pass                                           |
| `validator` | `@autosk/pi-agent/validator`  | independent item-by-item verification; bounces back to `dev` |
| `accept`    | _human_                       | the engine parks here for a human's final acceptance         |

- **Isolation:** `worktreeIsolation()` — each task runs in its own git worktree.
- **Visit cap:** `onTransit` rejects a bounce-back into `dev` once the task has
  already entered `dev` `DEV_VISIT_CAP` (5) times — the 6th `dev` entry is
  rejected (via `ctx.visits("dev")`), so a task that keeps failing review/
  validation parks for a human instead of looping forever.

The role prompts live under [`prompts/`](./prompts) and are seeded into each pi
agent as its `firstMessageFile`.

## Discovery — how every project gets it

`feature-dev` ships as a **daemon-bundled** extension (declared via
`package.json#autosk.extensions`). The daemon discovers `daemon/extensions/` at
the **lowest** priority, so every project can enroll into `feature-dev` with **no
per-project files**:

```
project-local (.autosk/extensions) ▸ global (~/.autosk/extensions) ▸ npm (settings.json) ▸ BUNDLED
```

A project- or global-level extension that registers a workflow/agent of the same
name **overrides** the bundled one (first-registered wins). The bundled dir is
`$AUTOSK_BUNDLED_EXTENSIONS`, else `daemon/extensions/` resolved relative to the
daemon. To enroll a task:

```
autosk enroll <task-id> --workflow feature-dev
```

## Configuration

This package exposes no per-step config knobs of its own — the agent behaviour
is configured on the [`@autosk/pi-agent`](../pi-agent) roles (model, thinking,
extra args, …) constructed in `featureDevAgents()`. To customise, copy this
extension into `~/.autosk/extensions/` (or your project's `.autosk/extensions/`)
and edit the `piAgent({...})` / `featureDevWorkflow({...})` calls; your copy then
overrides the bundled one.

## Exports

- default export — the extension factory (registers the four roles + the
  workflow).
- `featureDevWorkflow(options?)` → `WorkflowDefinition` (a factory; tests inject
  a custom `isolation`).
- `featureDevAgents()` → the four `piAgent` role definitions.
- `DEV_VISIT_CAP`, `ROLE_PREFIX` — the cap constant and role-name prefix.
