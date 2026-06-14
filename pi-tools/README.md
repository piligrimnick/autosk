# @autosk/pi-tools

A [pi](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) extension
that exposes the [autosk](https://github.com/wierdbytes/autosk) management tools
to an AI agent:

- **`autosk_task`** — `create` / `update` / `show` / `list`
- **`autosk_comment`** — `add` / `list`

Both shell out to the local `autosk` CLI (must be on `$PATH`) and speak its
`--json` wire format. They are the agent-facing surface over the autosk daemon's
task tracker.

## Install

Add it to your pi config (`~/.pi/agent/settings.json`):

```json
{
  "packages": ["npm:@autosk/pi-tools"]
}
```

`autosk` itself must be installed and on `$PATH` (`make build` in the autosk
repo, then put `./bin` on `PATH`).

## Project resolution & worktree isolation

The tools resolve which project to act on from the `autosk` child process's
working directory (walk-up to the nearest `.autosk/`). When an autosk workflow
agent runs under **worktree isolation** its working directory is a throwaway
worktree that contains no `.autosk/`, so a naive walk-up would resolve the wrong
project (or none).

To make that work, the autosk agent runtime (`@autosk/pi-agent`) sets
**`AUTOSK_CWD`** in the spawned pi's environment to the real project root. The
`autosk` CLI honors `AUTOSK_CWD` as its project selector, so these tools — and
any `autosk` call the agent makes — target the original project regardless of
the worktree they run in. Outside a workflow (a normal interactive pi session)
`AUTOSK_CWD` is unset and resolution falls back to the working directory.

## Workflow transitions are not a tool here

There is intentionally no `autosk_step` / transition tool. An
`@autosk/pi-agent`-driven workflow step records its transition through the
in-process `autosk_transit` channel the daemon injects and observes, so a
CLI-backed transition tool would be a no-op outside a workflow run.

## Develop

```sh
bun install        # pi peer deps
bun run typecheck  # tsc --noEmit
bun test           # pure argv-builder unit tests
```
