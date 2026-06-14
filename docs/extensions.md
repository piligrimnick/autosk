# Extensions — workflows & agents as code

In autosk v2, **workflows and agents are code**, not database rows or installed
npm-package "agents". You teach a project new workflows and agents by writing an
**extension**: a small module the daemon loads in-process, exactly the way
[pi](https://pi.dev) loads its extensions. The engine itself knows nothing about
graphs, visit caps, or prompts — it only drives the task status machine and calls
the hooks your code registers.

This page covers how extensions are discovered and loaded. For the
`WorkflowDefinition` / `AgentDefinition` contracts your extension registers, see
[docs/workflows.md](workflows.md).

## The entry point — a default-export factory

An extension is a module with a **default export** that is a factory function.
The daemon calls it once per project with an [`AutoskAPI`](../daemon/sdk/src/api.ts):

```ts
import type { AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { worktreeIsolation } from "@autosk/worktree";

export default function (autosk: AutoskAPI) {
  autosk.registerAgent(piAgent({
    name: "@me/dev",
    model: "sonnet:high",
    firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
  }));

  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      dev:    { agent: "@me/dev" },
      accept: { human: true },
    },
    onTransit(ctx, to) {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= 5) {
        throw new Error("dev bounced back too many times — park it");
      }
    },
    isolation: worktreeIsolation(),
  });
}
```

`AutoskAPI` has exactly two methods:

- `registerWorkflow(workflow: WorkflowDefinition)` — adds a workflow to the
  calling project's registry.
- `registerAgent(agent: AgentDefinition)` — adds an agent.

Both write into **that project's** registry. The daemon imports TypeScript
natively (it runs on Bun), so an extension can be plain `.ts` — no build step.

## Discovery order

For each project, the daemon discovers extensions from four sources and merges
them in **precedence order** (highest first):

1. **project-local** — `./.autosk/extensions/`
2. **global** — `~/.autosk/extensions/`
3. **npm packages** listed under `"extensions"` in `settings.json` (project
   `./.autosk/settings.json` first, then global `~/.autosk/settings.json`),
   installed under `~/.autosk/packages/node_modules/`
4. **daemon-bundled** — the extensions shipped with `autoskd` (lowest precedence;
   this is how `@autosk/feature-dev` reaches every project)

Within a directory, discovery is one level deep, in sorted filename order:

- a direct `*.ts` / `*.js` file is an entry;
- a subdirectory with an `index.ts` / `index.js` is an entry;
- a subdirectory with a `package.json` declaring `"autosk": { "extensions":
  ["./src/index.ts", …] }` contributes those declared entries.

Dedup is by entry path (first/highest-precedence occurrence wins), and on a
**name collision** the first-registered definition wins. Because the bundled
source is last, **any project-local, global, or npm extension that registers a
workflow/agent of the same name overrides the bundled one** — e.g. drop your own
`feature-dev` into `.autosk/extensions/` to replace the shipped reference
workflow.

A `settings.json` simply lists package names:

```jsonc
// ~/.autosk/settings.json  (or ./.autosk/settings.json)
{ "extensions": ["@me/autosk-flows", "@acme/review-bot"] }
```

## No trust model

An installed/discovered extension is **loaded, period** — there is no
prompt-on-first-load gate (unlike pi). Putting code into `.autosk/extensions/`
or naming a package in `settings.json` *is* the consent. Treat these locations
as you would any code you run.

## Error isolation & `project.diagnostics`

A broken extension never takes the daemon down. Each of these is caught,
recorded, and the rest of the registry stays usable:

- the module fails to import;
- it has no default-export factory;
- the factory throws;
- it registers a name that collides with an already-registered one;
- a `settings.json` package is listed but not installed / declares no extension.

Each failure becomes a load diagnostic tagged with the offending source (a path,
or an npm package name). Surface them with:

```bash
autosk project diagnostics
```

(The GUI shows a ⚠ badge on the project switcher with the same list.)

## The live-code hazard

Because workflows are code, editing them can leave in-flight tasks pointing at a
workflow or step that no longer exists. There are no frozen copies and no
versioning — **the registry at daemon start is the truth**. On project (re)load,
every `work` / `human` task is validated against the registry; a task whose
workflow or step has vanished is parked to `human` with
`error="workflow_missing: …"`. Fix the code (or restore the name) and resume the
task.

## The `singleStep` builtin

You don't always need a full workflow. `task.enroll {agent}` (CLI: `autosk
enroll <id> --agent <name>`) materialises a one-step workflow named
`single:<agent>` on demand — no persisted rows, no registry entry — whose only
step runs that agent. This replaces v1's `single:<agent>` synthetic workflows.

## Shipped (bundled) extensions

`autoskd` ships three extensions in `daemon/extensions/`, packaged beside the
binary for releases and discovered via the bundled source (override the location
with `AUTOSK_BUNDLED_EXTENSIONS`):

- **[`@autosk/worktree`](../daemon/extensions/worktree/README.md)** —
  `worktreeIsolation()`, the per-task git-worktree isolation provider you attach
  to any workflow.
- **[`@autosk/pi-agent`](../daemon/extensions/pi-agent/README.md)** —
  `piAgent({...})`, an agent that drives `pi --mode rpc`, mirrors pi's transcript
  entries 1:1, and bridges step transitions through an injected `autosk_transit`
  pi-tool.
- **[`@autosk/feature-dev`](../daemon/extensions/feature-dev/README.md)** — the
  reference workflow `dev → review → docs → validator → accept` (with bounce-backs
  and a `dev` visit cap), wired to four `@autosk/pi-agent` roles and
  `worktreeIsolation()`. It is the workflow every project can enroll into with no
  per-project files.

## Writing your own

The smallest extension is a single file:

```ts
// ~/.autosk/extensions/hello.ts
import type { AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerAgent({
    name: "@me/echo",
    async onRun(ctx) {
      await ctx.comment("hello from @me/echo");
      await ctx.transit({ status: "done" });
    },
  });
}
```

```bash
autosk enroll <task-id> --agent @me/echo   # uses the singleStep builtin
```

To customise a shipped extension, copy it into `~/.autosk/extensions/` (or your
project's `.autosk/extensions/`) and edit it; your copy overrides the bundled one
by name. See [docs/workflows.md](workflows.md) for the full
`WorkflowDefinition` / `AgentDefinition` / `IsolationProvider` contracts and the
`AgentRunContext` your `onRun` receives.
