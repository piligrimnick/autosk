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
import { statusStep, type AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { worktreeIsolation } from "@autosk/worktree";

export default function (autosk: AutoskAPI) {
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key IS the agent name; registering the workflow registers
      // its inline agents — there is no separate registerAgent call.
      dev: piAgent({
        model: "sonnet:high",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      accept: statusStep("human"),
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

`AutoskAPI` has exactly one method:

- `registerWorkflow(workflow: WorkflowDefinition)` — adds a workflow to the
  calling project's registry. The workflow's agents are declared **inline** as
  step values (an `AgentDefinition` per agent step, the step key being the agent
  name), so registering the workflow registers its agents — there is no separate
  `registerAgent`.

It writes into **that project's** registry. The daemon imports TypeScript
natively (it runs on Bun), so an extension can be plain `.ts` — no build step.

## Discovery order

For each project, the daemon discovers extensions from three sources and merges
them in **precedence order** (highest first):

1. **project-local** — `./.autosk/extensions/`
2. **global** — `~/.autosk/extensions/`
3. **npm packages** listed under `"extensions"` in `settings.json` (project
   `./.autosk/settings.json` first, then global `~/.autosk/settings.json`),
   installed under `~/.autosk/packages/node_modules/`

There is **no daemon-bundled source**: the reference `@autosk/feature-dev`
workflow is an ordinary npm package that the daemon **provisions on first run**
(see [First-run bootstrap](#first-run-bootstrap)) into source (3), so every
project discovers it with no per-project files.

Within a directory, discovery is one level deep, in sorted filename order:

- a direct `*.ts` / `*.js` file is an entry;
- a subdirectory with an `index.ts` / `index.js` is an entry;
- a subdirectory with a `package.json` declaring `"autosk": { "extensions":
  ["./src/index.ts", …] }` contributes those declared entries.

Dedup is by entry path (first/highest-precedence occurrence wins), and on a
**name collision** the first-registered definition wins. Because the npm source
is last, **any project-local or global extension that registers a workflow/agent
of the same name overrides an npm one** — e.g. drop your own `feature-dev` into
`.autosk/extensions/` to replace the provisioned reference workflow.

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
- the factory throws (e.g. a stale extension calling the removed `registerAgent`);
- it registers a name that collides with an already-registered one;
- a workflow step is neither an inline agent (`onRun`) nor a `statusStep`;
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

## First-run bootstrap

`autoskd` ships **no bundled extensions**. On a brand-new machine — detected by
the absence of `~/.autosk/settings.json` — the daemon provisions the default
extensions itself on startup: it shells out to `npm` to install
`@autosk/feature-dev` (which pulls `@autosk/pi-agent` / `@autosk/worktree` /
`@autosk/sdk` transitively) into `~/.autosk/packages/`, then writes
`~/.autosk/settings.json` listing `@autosk/feature-dev`. Every project then
discovers `feature-dev` through the npm-packages source above.

- `settings.json` **is** the "already initialised" marker: once it exists the
  bootstrap is a no-op, so an operator who manages extensions by hand (or is
  air-gapped) is never surprised by a network install. Provide your own
  `~/.autosk/settings.json` to opt out entirely.
- The install needs `npm` on `PATH` (override with `$AUTOSK_NPM_BIN`) and network
  access. A failed install is **logged, never fatal** — the daemon keeps serving
  and leaves `settings.json` absent so the next start retries.

## Auto-install reconcile (every start)

The first-run bootstrap only fires when `settings.json` is **absent**. To keep a
hand-edited config in sync, the daemon also runs a **reconcile** pass that
installs any package listed under `"extensions"` that is not yet present under
`~/.autosk/packages/node_modules/`. So after you add a package name to a
`settings.json` by hand, the next daemon start (re)spawns and installs it for you
— no manual `npm install` step.

- **What runs when.** The **global** `~/.autosk/settings.json` is reconciled once
  per daemon start; each project's **project-local** `./.autosk/settings.json` is
  reconciled the first time that project is opened (its packages install into the
  same global `~/.autosk/packages/` prefix). Both happen after the socket is
  accepting, so auto-spawn readiness is never blocked by an install.
- **Missing only.** Only packages whose `node_modules/<name>` directory is absent
  are installed; already-installed packages are left untouched (no upgrade), so a
  fully-provisioned environment never hits the network. A failed install is
  logged, never fatal — the listed-but-missing package simply stays a
  `project.diagnostics` entry until the next start retries.
- **Opt out.** Set **`AUTOSK_NO_AUTO_INSTALL`** (to any value other than
  empty / `0` / `false`) to disable **all** automatic installs — the first-run
  bootstrap *and* the reconcile both become no-ops, leaving any listed-but-missing
  package as a diagnostic only. This is the escape hatch for air-gapped or
  hand-managed environments that provision `~/.autosk/packages/` themselves.

## The default extensions

These are the npm packages the first-run bootstrap provisions (and the building
blocks for your own workflows):

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
import { type AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerWorkflow({
    name: "echo",
    firstStep: "echo",
    steps: {
      // The step key "echo" is the inline agent's name.
      echo: {
        async onRun(ctx) {
          await ctx.comment("hello from echo");
          await ctx.transit({ status: "done" });
        },
      },
    },
  });
}
```

```bash
autosk enroll <task-id> --workflow echo
```

To customise a default extension, copy it into `~/.autosk/extensions/` (or your
project's `.autosk/extensions/`) and edit it; your copy overrides the npm one
by name. See [docs/workflows.md](workflows.md) for the full
`WorkflowDefinition` / `AgentDefinition` / `StatusStep` / `IsolationProvider`
contracts and the `AgentRunContext` your `onRun` receives.
