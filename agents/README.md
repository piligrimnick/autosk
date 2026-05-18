# `agents/` — reference autosk agent packages

This directory holds the reference autosk agent packages.

The agents wired into [`docs/notes/workflow-example.json`](../docs/notes/workflow-example.json):

| Name | Step in `feature-dev` | Package |
|---|---|---|
| `developer`      | `dev`       | [`./developer/`](./developer/) |
| `code-reviewer`  | `review`    | [`./code-reviewer/`](./code-reviewer/) |
| `task-validator` | `validator` | [`./task-validator/`](./task-validator/) |

Plus a no-config fallback for ad-hoc tasks:

| Name | Description | Package |
|---|---|---|
| `@autogent/generic` | Empty `autosk.agent` block; `pi` runs with the user's own defaults. | [`./generic-agent/`](./generic-agent/) |

All four are **standard** agents (no `runner`): the autosk daemon
spawns `pi --mode rpc` with each package's configured model, thinking
level, and first-turn message.

## Install them

Install into your local autosk packages prefix:

```bash
autosk agent install ./agents/developer
autosk agent install ./agents/code-reviewer
autosk agent install ./agents/task-validator
autosk agent install ./agents/generic-agent

# verify
autosk agent list
# NAME                SOURCE   VERSION  IN_DB
# @autogent/generic   package  0.1.0    yes
# code-reviewer       package  0.1.0    yes
# developer           package  0.1.0    yes
# human               builtin  -        yes
# task-validator      package  0.1.0    yes
```

`autosk agent install <path>` accepts any directory whose `package.json`
has the autosk `agent` block; the registered name comes from the
package.json `name` field. Iterating on a prompt? Re-run the install
to update the prefix's copy.

## Use them in a workflow

The workflow definition that ties them together ships at
[`docs/notes/workflow-example.json`](../docs/notes/workflow-example.json):

```bash
autosk workflow create --file docs/notes/workflow-example.json
autosk create "Implement auth module" --workflow feature-dev
autosk daemon serve --workers 1
```

The daemon picks up the task at step `dev`, runs `developer`, advances
to `review`, runs `code-reviewer`, and so on. See
[`docs/workflows.md`](../docs/workflows.md) for the full walkthrough.

## Customise

Each package is a few files: `package.json` + a markdown first-message body.
Fork any of them under your own scope (`@your-org/developer` etc.) and
tune the prompt or model. The daemon picks up changes on the next
spawn — there's no warm-cache to invalidate.

See [`docs/plans/20260518-Agent-Packages.md`](../docs/plans/20260518-Agent-Packages.md)
for the package format reference.
