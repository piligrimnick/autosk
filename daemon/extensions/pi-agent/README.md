# @autosk/pi-agent

Drive [`pi`](https://github.com/earendil-works/pi) (`pi --mode rpc`) as an
autoskd v2 **agent**. `piAgent({...})` returns an
[`AgentDefinition`](../../sdk/src/agent.ts) the engine can run for a workflow
step; it ports v1's "standard branch" (spawn pi, seed the first message, drive
turns, kickback loop) onto the v2 `ctx.spawn` + `ctx.transit` API (design
`docs/plans/20260612-Bun-Daemon-Extensions.md` §3.4).

## Usage

```ts
import { piAgent } from "@autosk/pi-agent";
import { worktreeIsolation } from "@autosk/worktree";

export default function (autosk) {
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key IS the agent name; registering the workflow registers
      // its inline agents.
      dev: piAgent({
        model: "sonnet:high",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      review: piAgent({ thinking: "xhigh", firstMessageFile: ".../review.md" }),
    },
    isolation: worktreeIsolation(),
  });
}
```

A `piAgent({...})` is an inline **step value**: the step key is the agent name
(there is no `name` option — the driver takes its display name from
`ctx.workflows.current.step`), so registering the workflow registers its agents.

## How it works

On each `onRun` the agent:

1. spawns `pi --mode rpc` (with the role's `model` / `thinking` / extra args)
   and **injects a pi extension** that registers an `autosk_transit` tool;
2. seeds pi with the rendered step prompt (role first-message + task context +
   the available transitions + "call `autosk_transit`");
3. mirrors pi's session entries (messages / custom) into the autosk transcript
   1:1, so existing pi renderers stay reusable;
4. **observes the `autosk_transit` tool call** on pi's RPC event stream and
   translates it into `ctx.transit(...)` (the transit channel — core stays
   closed, no session-scoped daemon RPC);
5. runs a **kickback/corrections loop** (private to this extension): if a turn
   ends without a transit — or a chosen transition is rejected by the workflow's
   `onTransit` — it feeds a corrective message back to the model and retries, up
   to `maxCorrections` times. After the budget is spent it returns without a
   transit and the engine parks the task (`agent_did_not_transit`).

`onSteer` / `onFollowup` forward a `session.input` message into the **live** pi;
`onAbort` asks pi to wind down gracefully (the engine's abort signal already
terminates the child).

## Configuration — `PiAgentOptions`

The agent name is **not** an option — it is the workflow step key the `piAgent`
is assigned to (taken from `ctx.workflows.current.step` at run time).

| Option             | Default                       | Description                                                              |
| ------------------ | ----------------------------- | ------------------------------------------------------------------------ |
| `model`            | pi default                    | pi model spec, e.g. `"sonnet:high"` (`--model`).                          |
| `thinking`         | pi default                    | Thinking level `off`…`xhigh` (`--thinking`).                             |
| `firstMessage`     | `""`                          | Inline first-message seed (wins over `firstMessageFile`).                |
| `firstMessageFile` | —                             | Path to a file whose contents seed the first message.                   |
| `extraArgs`        | `[]`                          | Extra args forwarded verbatim to `pi`.                                   |
| `piExtensions`     | `[]`                          | pi extensions to load (`-e <path>` each).                                |
| `piSkills`         | `[]`                          | pi skills to enable (`--skill <name>` each).                            |
| `maxCorrections`   | `3`                           | Corrective turns before giving up (then the engine parks the task).     |
| `piBin`            | `$AUTOSK_PI_BIN` or `"pi"`    | `pi` binary to spawn (the e2e tests point this at a stub).              |

## The injected `autosk_transit` tool

`src/pi-transit-extension.ts` is the pi extension this package injects via
`pi -e`. It registers the `autosk_transit` tool (one `to` string: a sibling step
name, or `done` | `cancel` | `human`). The tool returns an immediate ack; the
real transition (and any rejection fed back as a correction) is driven by the
autosk daemon, which observes the call on pi's RPC event stream. That file is
loaded by **pi's** toolchain, not the daemon, so it is excluded from this
package's `tsc` typecheck.

## Exports

- `piAgent(options)` → `AgentDefinition`
- `buildPiCommand(options)`, `PiDriver`, `parseTarget`, the prompt renderers
  (`renderInitialPrompt`, `kickbackMessage`, `rejectionMessage`, `targetLabels`)
  — exported for tooling / tests.
- default export — a no-op extension factory (roles are registered by the
  consuming extension, e.g. `@autosk/feature-dev`).
