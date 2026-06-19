# @autosk/feature-dev-cc

The **Claude Code twin** of [`@autosk/feature-dev`](../feature-dev/README.md): the
same full feature-development cycle, but every agent step is an inline
[`@autosk/claude-agent`](../claude-agent/README.md) role (driving `claude -p`
headless stream-json) instead of an [`@autosk/pi-agent`](../pi-agent/README.md)
role. Isolation is still [`@autosk/worktree`](../worktree/README.md).

## The workflow

```text
dev ──▶ review ──▶ docs ──▶ validator ──▶ accept (human)
 ▲        │                    │
 └────────┘────────────────────┘   (review→dev and validator→dev bounce-backs)
```

| Step        | Kind                            | Notes                                                        |
| ----------- | ------------------------------- | ------------------------------------------------------------ |
| `dev`       | `claudeAgent` (name `dev`)      | first step; implements the task                              |
| `review`    | `claudeAgent` (name `review`)   | thorough code review (`effort: "xhigh"`); bounces back to `dev` |
| `docs`      | `claudeAgent` (name `docs`)     | documentation pass (leaves `CHANGELOG.md` to `validator`)    |
| `validator` | `claudeAgent` (name `validator`)| independent item-by-item verification; on success runs release hygiene (CHANGELOG `[Unreleased]` + a clean, committed worktree) before `accept`; bounces back to `dev` |
| `accept`    | `statusStep("human")`           | the engine parks here for a human's final acceptance         |

Each agent step is an inline `@autosk/claude-agent` value: the **step key is the
agent name** (`dev`/`review`/`docs`/`validator`), and registering the workflow
registers those agents — there is no separate agent registration.

- **Isolation:** `worktreeIsolation()` — each task runs in its own git worktree.
- **Visit cap:** `onTransit` rejects a bounce-back into `dev` once the task has
  already entered `dev` `DEV_VISIT_CAP` (5) times — the 6th `dev` entry is
  rejected (via `ctx.visits("dev")`), so a task that keeps failing review/
  validation parks for a human instead of looping forever. The count is the
  persistent `metadata.step_visits.dev` the engine maintains; reset it with
  `autosk metadata unset <id> step_visits` to let a parked task bounce through
  `dev` again.

The role prompts live under [`prompts/`](./prompts) as `.md` files; the workflow
reads each one and seeds it into the corresponding Claude agent as its
`firstMessage`. They are adapted from `feature-dev`'s prompts for the Claude MCP
tool surface — each prompt instructs the model to use the `comment` tool and to
call the `transit` tool exactly once (the model sees these as
`mcp__autosk__comment` / `mcp__autosk__transit`).

## Requirements

Because the agent steps are `@autosk/claude-agent`, the same runtime
requirements apply (see that package's README):

- **`claude`** (the Claude Code CLI) on `PATH`, or `$AUTOSK_CLAUDE_BIN`, already
  authenticated (the headless run is unattended).
- **`autosk`** (the CLI) on `PATH`, or `$AUTOSK_BIN` — the MCP server's
  `task` / `comment` tools shell out to it.
- A **git repo** at the project root (worktree isolation).

## Install — local checkout (for testing)

`feature-dev-cc` is **not** part of the first-run bootstrap (unlike
`feature-dev`). Add it explicitly. Because it depends on `@autosk/claude-agent`
(which is not yet bootstrapped either), the most reliable way to test it is to
install this directory **in place** from the repo checkout, where its
`@autosk/*` deps resolve through the Bun workspace:

```bash
# from anywhere — point autosk at this directory (loaded in place, never copied)
autosk ext add /ABS/PATH/TO/autosk/daemon/extensions/feature-dev-cc

# or scope it to the current project only
autosk ext add -l /ABS/PATH/TO/autosk/daemon/extensions/feature-dev-cc

# the registry is built once per daemon start, so restart / reopen the project:
autosk workflow list            # feature-dev-cc should appear
autosk enroll <task-id> --workflow feature-dev-cc
# or create + enroll in one shot:
autosk create "Fix the flaky test" --workflow feature-dev-cc
```

Extensions are loaded once when a project is first opened and cached for the
daemon's lifetime — a freshly-added extension is picked up only on the **next
daemon start / first project open**, so restart the daemon (or reopen the
project) after `ext add`. If the workflow does not show up, check
`autosk project diagnostics` for a load error (e.g. an unresolved
`@autosk/claude-agent`).

## Configuration

This package exposes no per-step config knobs of its own — the agent behaviour
is configured on the inline [`@autosk/claude-agent`](../claude-agent/README.md)
step values (model, effort, permission posture, extra args, …) inside
`featureDevCcWorkflow()`. To customise, copy this extension into
`~/.autosk/extensions/` (or your project's `.autosk/extensions/`) and edit the
`claudeAgent({...})` / `featureDevCcWorkflow({...})` calls; your copy then
overrides this one by name.

## Exports

- default export — the extension factory (registers the workflow, whose steps
  carry the four inline Claude agents).
- `featureDevCcWorkflow(options?)` → `WorkflowDefinition` (a factory; tests inject
  a custom `isolation`).
- `DEV_VISIT_CAP` — the `dev` re-entry cap constant.
