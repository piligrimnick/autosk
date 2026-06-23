# @autosk/feature-dev

The shipped **reference workflow** for autoskd v2: a full feature-development
cycle wired to [`@autosk/pi-agent`](../pi-agent) roles, each running its harness
in a per-task [`@autosk/sandbox`](../sandbox) `worktreeSandbox()`, with teardown
as a normal `cleanup` step. It replaces v1's `feature-dev-generic` bootstrap
(design `docs/plans/20260612-Bun-Daemon-Extensions.md` §3.6, P6).

## The workflow

```text
dev ──▶ review ──▶ docs ──▶ validator ──▶ accept (human) ──▶ cleanup ──▶ done
 ▲        │                    │
 └────────┘────────────────────┘   (review→dev and validator→dev bounce-backs)
```

| Step        | Kind                          | Notes                                                        |
| ----------- | ----------------------------- | ------------------------------------------------------------ |
| `dev`       | `piAgent` (name `dev`)        | first step; implements the task                              |
| `review`    | `piAgent` (name `review`)     | thorough code review (`thinking: xhigh`); bounces back to `dev` |
| `docs`      | `piAgent` (name `docs`)       | documentation pass (leaves `CHANGELOG.md` to `validator`)    |
| `validator` | `piAgent` (name `validator`)  | independent item-by-item verification; on success runs release hygiene (CHANGELOG `[Unreleased]` + a clean, committed worktree) before `accept`; bounces back to `dev` |
| `accept`    | `statusStep("human")`         | the engine parks here for a human's final acceptance         |
| `cleanup`   | `sandboxCleanupStep(sandbox)` | removes the worktree (branch preserved), then transits to `done` |

Each agent step is an inline `@autosk/pi-agent` value: the **step key is the
agent name** (`dev`/`review`/`docs`/`validator`), and registering the workflow
registers those agents — there is no separate agent registration.

- **Sandbox:** `worktreeSandbox()` — each task runs in its own git worktree, torn
  down by the `cleanup` step (the human resumes an accepted task into it via
  `autosk resume <id> --to cleanup`). Routing terminals through `cleanup` is what
  keeps a task from leaking its worktree now that `done`/`cancel` are a raw flip.
- **Visit cap:** `onTransit` rejects a bounce-back into `dev` once the task has
  already entered `dev` `DEV_VISIT_CAP` (5) times — the 6th `dev` entry is
  rejected (via `ctx.visits("dev")`), so a task that keeps failing review/
  validation parks for a human instead of looping forever. The count is the
  persistent `metadata.step_visits.dev` the engine maintains; reset it with
  `autosk metadata unset <id> step_visits` to let a parked task bounce through
  `dev` again.

The role prompts live under [`prompts/`](./prompts) as `.md` files; the workflow
reads each one and seeds it into the corresponding pi agent as its `firstMessage`.
Each prompt ends with an **Available transitions** section that names the
intended `autosk_transit` targets for that step (e.g. `review → docs | dev`),
so the agent picks the right edge out of the conservative target superset the
engine injects at runtime.

## Discovery — how every project gets it

`feature-dev` is an **npm package** (declared via `package.json#autosk.extensions`).
The daemon installs it into `~/.autosk/packages/` on first run (see the
[extensions docs → First-run bootstrap](../../../docs/extensions.md#first-run-bootstrap))
and lists it in `~/.autosk/settings.json`, so every project can enroll into
`feature-dev` with **no per-project files**:

```
project-local (.autosk/extensions) ▸ global (~/.autosk/extensions) ▸ npm (settings.json)
```

A project- or global-level extension that registers a workflow/agent of the same
name **overrides** the npm one (first-registered wins). To enroll a task:

```
autosk enroll <task-id> --workflow feature-dev
```

## Configuration

This package exposes no per-step config knobs of its own — the agent behaviour
is configured on the inline [`@autosk/pi-agent`](../pi-agent) step values (model,
thinking, extra args, …) inside `featureDevWorkflow()`. To customise, copy this
extension into `~/.autosk/extensions/` (or your project's `.autosk/extensions/`)
and edit the `piAgent({...})` / `featureDevWorkflow({...})` calls; your copy then
overrides the npm one.

## Exports

- default export — the extension factory (registers the workflow, whose steps
  carry the four inline agents).
- `featureDevWorkflow(options?)` → `WorkflowDefinition` (a factory; tests inject
  a custom `sandbox`).
- `DEV_VISIT_CAP` — the `dev` re-entry cap constant.
