# @autosk/agent-sdk

Types and tiny helpers for authoring **custom-runner** agents for
[`autosk`](../../README.md).

The autosk daemon spawns a Node bootstrapper for any agent package
whose `package.json` declares an `autosk.agent.runner` path. The
bootstrapper imports the runner module, constructs a `RunContext`, and
awaits the runner's default export.

## Minimal example

```ts
// my-agent/src/agent.ts
import type { RunAgent } from "@autosk/agent-sdk";

const run: RunAgent = async (ctx) => {
  // Inspect the task.
  console.log(`task ${ctx.task.id}: ${ctx.task.title}`);

  // Do whatever you like — call APIs, run shell, edit files.
  await ctx.cli(["comment", "add", ctx.task.id, "starting"]);

  // ...

  // Tell autosk how this turn should end. MUST be called exactly once.
  await ctx.stepNext("done");
};

export default run;
```

```jsonc
// my-agent/package.json
{
  "name": "@me/my-agent",
  "version": "0.1.0",
  "type": "module",
  "autosk": { "agent": { "runner": "./src/agent.ts" } },
  "peerDependencies": { "@autosk/agent-sdk": ">=0.1.0 <0.2.0" }
}
```

Install with:

```bash
autosk agent install @me/my-agent
```

## Contract

| Rule | Effect of breaking it |
|---|---|
| Default export is `RunAgent`. | The bootstrapper exits with a clear error and the run fails. |
| `ctx.stepNext(...)` is called exactly once before the function resolves. | Zero calls → run fails with `agent_did_not_emit_transition`. Two calls → second one is rejected by autosk (PK on `step_signals.run_id`). |
| The `to` arg is one of the targets listed in `ctx.transitions`. | Rejected with `unknown_transition`. |

See [`docs/plans/20260518-Agent-Packages.md`](../../docs/plans/20260518-Agent-Packages.md)
for the design and rationale.

## License

MIT.
