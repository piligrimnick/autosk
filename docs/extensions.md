# Extensions — workflows & agents as code

In autosk v2, **workflows and agents are code**, not database rows or installed
npm-package "agents". You teach a project new workflows and agents by writing an
**extension**: a small module the daemon loads in-process, exactly the way
[pi](https://pi.dev) loads its extensions. The engine itself knows nothing about
graphs, visit caps, or prompts — it only drives the task status machine and calls
the hooks your code registers.

This page covers how extensions are discovered and loaded. For the
`WorkflowDefinition` / `AgentDefinition` contracts your extension registers, see
[docs/workflows.md](workflows.md). If you'd rather learn by building one, two
tutorials walk you through it: the [extension tutorial](extensions-tutorial.md)
goes from an empty directory to a workflow you can run (and deliberately break)
in a few minutes, and the [Claude Code workflow tutorial](extensions-tutorial-claude.md)
wires a real `@autosk/claude-agent` agent into a per-task git worktree.

## The entry point — a default-export factory

An extension is a module with a **default export** that is a factory function.
The daemon calls it once per project with an [`AutoskAPI`](../daemon/sdk/src/api.ts):

```ts
import { statusStep, type AutoskAPI } from "@autosk/sdk";
import { piAgent } from "@autosk/pi-agent";
import { sandboxCleanupStep, worktreeSandbox } from "@autosk/sandbox";

export default function (autosk: AutoskAPI) {
  const sandbox = worktreeSandbox();   // a per-task git worktree the agent runs in
  autosk.registerWorkflow({
    name: "my-flow",
    firstStep: "dev",
    steps: {
      // The step key IS the agent name; registering the workflow registers
      // its inline agents — there is no separate registerAgent call.
      dev: piAgent({
        sandbox,
        model: "sonnet:high",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
      }),
      accept: statusStep("human"),
      // Cleanup is a normal step: route terminals through it so the worktree
      // never leaks (`done`/`cancel` are now a raw status flip).
      cleanup: sandboxCleanupStep(sandbox),
    },
    onTransit(ctx, to) {
      if ("step" in to && to.step === "dev" && ctx.visits("dev") >= 5) {
        throw new Error("dev bounced back too many times — park it");
      }
    },
  });
}
```

`AutoskAPI` has two methods:

- `registerWorkflow(workflow: WorkflowDefinition)` — adds a workflow to the
  calling project's registry. The workflow's agents are declared **inline** as
  step values (an `AgentDefinition` per agent step, the step key being the agent
  name), so registering a workflow registers its inline agents.
- `registerAgent({ name, description?, agent })` — registers a **named,
  standalone agent** that can back an interactive (taskless) chat session (the
  agent picker lists every registered agent, via `registry.agent.list`). See
  [docs/workflows.md → Named agents](workflows.md#named-agents--interactive-sessions).

It writes into **that project's** registry. The daemon imports TypeScript
natively (it runs on Bun), so an extension can be plain `.ts` — no build step.

### What a valid registration requires

Both methods **validate** what you hand them. A violation is never fatal: the
offending registration is **skipped** and recorded as a
[`project.diagnostics`](#error-isolation--projectdiagnostics) entry, while the
rest of the same factory keeps running. So one bad workflow in a factory that
registers three leaves the other two registered.

`registerWorkflow(workflow)` requires:

- `name` — a **non-empty string**, and **unique** in the project's registry
  (a duplicate is rejected; the first-registered, higher-precedence definition
  keeps the name).
- `steps` — a present **object** (step name → definition).
- `firstStep` — a key that **exists in `steps`**.
- each **step value** — either an **agent** (an object with an `onRun`
  function; the step key is the agent name) or a `statusStep` whose `status` is
  one of `done` / `cancel` / `human`. Any other shape (or a `statusStep` with a
  different status) is rejected.

`registerAgent({ name, description?, agent })` requires:

- `name` — a **non-empty string**, and **unique** among registered agents.
- `agent.onRun` — a **function** (the same `onRun` an inline agent step uses).

## Discovery order

For each project, the daemon discovers extensions from three sources and merges
them in **precedence order** (highest first):

1. **project-local** — `./.autosk/extensions/`
2. **global** — `~/.autosk/extensions/`
3. **settings packages** listed under `"extensions"` in `settings.json` (project
   `./.autosk/settings.json` first, then global `~/.autosk/settings.json`). Each
   entry is either an `npm:<spec>` package — resolved under the **same scope's**
   packages prefix (project settings → `<root>/.autosk/packages/node_modules/`,
   global settings → `~/.autosk/packages/node_modules/`) — or an absolute local
   path resolved in place. Dedup within this source is npm by **name** / local by
   **path**, project beating global.

There is **no daemon-bundled source**: the reference `@autosk/feature-dev`
workflow is an ordinary npm package that the daemon **provisions on first run**
(see [First-run bootstrap](#first-run-bootstrap)) into source (3), so every
project discovers it with no per-project files.

At a glance:

| # | Source | Where it lives | Dedup within the source |
|---|--------|----------------|-------------------------|
| 1 | project-local dir | `./.autosk/extensions/` | by entry path |
| 2 | global dir | `~/.autosk/extensions/` | by entry path |
| 3 | settings packages | `settings.json#extensions` (project file, then global file) | npm by **name**, local by **path** — project beats global |

Across all three, the final list is then deduped **by entry path** (the first,
highest-precedence occurrence wins), and a **name collision** between two loaded
definitions is resolved **first-registered-wins** — so a higher-precedence
source always keeps the name.

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

Each `settings.json#extensions` entry is one of **two** explicit forms — an
`npm:<spec>` package (the `<spec>` may include an `@version`) or an absolute
local path to a `.ts`/`.js` file or a directory:

```jsonc
// ~/.autosk/settings.json  (or ./.autosk/settings.json)
{
  "extensions": [
    "npm:@autosk/feature-dev",
    "npm:@acme/review-bot@1.4.0",
    "/Users/me/work/my-ext"
  ]
}
```

A relative path in a `settings.json` entry resolves against that file's
`.autosk/` directory; the stored form is absolute. An entry that is **neither**
`npm:`-prefixed nor a path (a bare `review-bot`) is **invalid** — there is no
implicit bare-name → npm form: it is recorded as a `project.diagnostics` entry
and shown as `kind:invalid` by [`autosk ext list`](#managing-extensions).
Prefer managing this file with [`autosk ext`](#managing-extensions) over
editing it by hand.

## Managing extensions

Manage `settings.json#extensions` with the `autosk ext` command group rather
than hand-editing the file (modeled on [pi](https://pi.dev)'s package
management). A **source** is always explicit — either an `npm:`-prefixed package
spec or a local path; there is no implicit bare-name → npm form.

```bash
# npm package — installed into the scope's packages prefix
autosk ext add npm:@acme/review-bot
autosk ext add npm:@acme/review-bot@1.4.0   # pin a version

# local directory or a single .ts/.js file — referenced in place, never copied
autosk ext add ./my-ext
autosk ext add ~/work/autosk-flows
autosk ext add /abs/path/to/ext.ts

autosk ext list                             # both scopes: kind + resolved
autosk ext remove npm:@acme/review-bot      # drop the entry (matches any version)
autosk ext update                           # bump floating npm entries to latest
autosk ext update --dry-run                 # report available updates, install nothing
autosk ext reload                           # re-apply the registry live (no restart)
```

- **Sources.** An `npm:<spec>` source `npm install`s the package (the `<spec>`
  may carry an `@version`) into the scope's packages prefix and records
  `npm:<spec>` in `settings.json`. A local path (`/abs`, `./rel`, `../rel`,
  `~/path`) is resolved to an **absolute** path, checked to exist, and recorded
  as-is — the file/dir is loaded in place, never copied. Anything else (a bare
  `review-bot`) is rejected with an error.
- **Scope — global by default, `-l/--local` for the project.** A bare `ext add`
  is **global**: the package lands in `~/.autosk/packages/` and the entry in
  `~/.autosk/settings.json`. Pass **`-l/--local`** to target the current project
  instead — npm packages install into `<project>/.autosk/packages/` and the entry
  goes into `<project>/.autosk/settings.json`. A `-l` install requires a project
  at the cwd; a global install does not.
- **`ext list`.** Shows both scopes' entries with their `kind`
  (`npm` / `local` / `invalid`) and a `resolved` flag — whether the entry
  actually resolves to a loadable extension *right now* (an installed npm
  package, or an existing local path that declares an extension). A local source
  is only checked for existence at install time, so a path that exists but
  declares no loadable extension still records the entry and then shows
  `resolved:false` here. `--json` returns the structured form.
- **`ext remove`.** Drops the matching entry from the scope's
  `settings.json` (global by default, `-l/--local` for the current project) —
  npm matches by **name** (any version), local by absolute **path**. It does
  **not** uninstall the package from `node_modules` (like pi); only the settings
  entry goes. Because `remove` takes a valid source, an
  `invalid` entry that `ext list` flags must be cleared by hand-editing
  `settings.json`.
- **`ext reload`.** Rebuilds the current project's merged (global + project)
  registry on demand and atomically swaps it onto the live daemon — no restart.
  Picks up added/removed extensions (including a brand-new or deleted file under
  `.autosk/extensions/`) and prints the root, its registered workflows, and any
  load diagnostics + parked tasks (the diagnostics/parks go to stderr). Editing
  an installed extension's *code in place* (or `ext update`) still needs a
  restart — the Bun module-cache wall.
- **`ext update [source]`.** Bumps installed **floating** npm extensions
  (`npm:foo`) to their newest registry version, in place. The version check is
  `npm view <name> version` against the installed
  `node_modules/<name>/package.json` — when they differ the package is
  re-installed with `<name>@latest` into that scope's `packages/` prefix. The
  floating `settings.json` entry needs no rewrite (only `node_modules` moves).
  - **Scope.** Outside a project it updates the **global** scope only; inside a
    project it updates the **union** of global + project (mirroring how a project
    loads extensions). Pass **`--global`** to force global-only, or
    **`-l/--local`** to force project-only (which requires a project, like
    `add`/`remove`); the two flags are mutually exclusive.
  - **Skips.** Version-pinned npm entries (`npm:foo@1.2.3`) and local-path
    entries are **skipped** (reported with a reason) — a pin is intentional and
    a local path is loaded in place, so there is nothing to bump.
  - **Targeting.** An optional `[source]` (`npm:<name>`) updates a single
    extension; a name that matches nothing errors with a "did you mean
    npm:<name>?" hint, and a pinned/local match is reported as `skipped`.
  - **`--dry-run` / `--check`.** Report available updates (with from → to
    versions) and install nothing; this path always exits 0. A registry lookup
    that fails is **fail-open** — a real run updates anyway, while `--dry-run`
    surfaces the row as `unknown`.
  - The table is `SCOPE PACKAGE FROM TO STATUS` (status one of `updated` /
    `up-to-date` / `skipped` / `failed` / `available` / `unknown`) plus a
    summary; `--json` emits the structured result. A real run that left any
    package `failed` exits non-zero (for both the table and `--json`).
- **Always runs.** An explicit `autosk ext add` / `autosk ext update` is **not**
  gated by `AUTOSK_NO_AUTO_INSTALL` — that switch only disables the automatic
  [first-run bootstrap](#first-run-bootstrap) and
  [reconcile](#auto-install-reconcile-every-start).

**When it takes effect — `add`/`remove` hot-reload.** `autosk ext add` and
`autosk ext remove` apply **live**: the daemon rebuilds the affected project's
extension registry and atomically swaps it in, so a newly-added workflow/agent
is immediately schedulable and a removed one stops being scheduled — **no
restart**. A **global** add/remove reloads every currently-open project; a
`-l/--local` one reloads just that project. The command reports `applied live
to N open project(s)` instead of a restart hint (when no project is open the
change simply lands on the next open, and the soft restart hint is printed
instead).

**Running sessions are never disturbed.** A live session keeps the
workflow/agent objects it captured at dispatch and finishes on that code; only
new dispatch / enroll / resume / interactive-create see the new registry. A task
whose workflow was just removed mid-session is **not** parked out from under its
run — it parks only after the session settles (see
[The live-code hazard](#the-live-code-hazard)).

**Update / in-place edits are still restart-only.** `autosk ext update` and
editing an installed extension's *code in place* re-use the same module path,
which Bun will not re-import with new code in one process (the module-cache
wall) — so `ext update` keeps its restart hint. Adding a brand-new file or
deleting one under `.autosk/extensions/` *is* an add/remove and hot-applies; use
[`autosk ext reload`](#managing-extensions) to pick those up on demand.

**From the GUI.** The desktop app offers an equivalent install path for npm
extensions: the **Workflows** panel header has a `＋` action (shown when a
project is active) that opens a browser of npm packages published with the
`autosk-extension` keyword, sorted by weekly downloads. Picking **Install** asks
whether to install **Globally** or **To this project**, then calls the same
`extension.install` RPC (`{ local: false | true }`) these `autosk ext add` /
`autosk ext add -l` commands use. The daemon hot-applies the install the same
way (the workflow is schedulable immediately), but the desktop app's Workflows
panel currently refreshes its list on the next project open, so it still shows a
reopen hint. The GUI install covers `npm:` packages only; local-path sources are
still CLI-only.

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
- it registers a workflow or agent name that collides with an already-registered one;
- a workflow step is neither an inline agent (`onRun`) nor a `statusStep`;
- a `settings.json` entry is listed but not installed / declares no extension, or
  is neither `npm:`-prefixed nor a path (an invalid entry).

Each failure becomes a load diagnostic tagged with the offending source (a path,
or an npm package name). Surface them with:

```bash
autosk project diagnostics
```

(The GUI shows a ⚠ badge on the project switcher with the same list.)

## The live-code hazard

Because workflows are code, removing or editing them can leave in-flight tasks
pointing at a workflow or step that no longer exists. There are no frozen copies
and no versioning — **the current registry is the truth**. The registry is
validated against in-flight tasks at project open **and on every hot-reload**
(`ext add` / `remove` / `reload`): every `work` / `human` task whose workflow or
step has vanished is parked to `human` with `error="workflow_missing: …"`. Fix
the code (or restore the name) and resume the task.

A task that currently has a **live session** is exempt from this guard — it is
never parked out from under a running run. Its session finishes on the
workflow/agent objects it captured at dispatch; if the workflow is gone by then,
the scheduler parks the task on the next scan, so a removed-mid-run task
self-heals to `human` only after its session settles, never during it. Editing
an installed extension's code in place still needs a restart (the Bun
module-cache wall), so the open-time validation remains the catch-all for code
edits.

## First-run bootstrap

`autoskd` ships **no bundled extensions**. On a brand-new machine — detected by
the absence of `~/.autosk/settings.json` — the daemon provisions the default
extensions itself on startup: it shells out to `npm` to install
`@autosk/feature-dev` (which pulls `@autosk/pi-agent` / `@autosk/sandbox` /
`@autosk/sdk` transitively) into `~/.autosk/packages/`, then writes
`~/.autosk/settings.json` listing the explicit `npm:@autosk/feature-dev` entry.
Every project then discovers `feature-dev` through the npm-packages source above.

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
installs any `npm:` package listed under `"extensions"` that is not yet present
under the scope's `packages/node_modules/` prefix (local-path entries are skipped
— they load in place). So after you add an `npm:` entry to a `settings.json` by
hand — or `autosk ext add` records one — the next daemon start (re)spawns and
installs it for you, no manual `npm install` step.

- **What runs when.** The **global** `~/.autosk/settings.json` is reconciled once
  per daemon start; each project's **project-local** `./.autosk/settings.json` is
  reconciled the first time that project is opened (its `npm:` packages install
  into the **project's own** `<root>/.autosk/packages/` prefix — the same place
  the loader resolves project-settings packages from). Both happen after the
  socket is accepting, so auto-spawn readiness is never blocked by an install.
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

- **[`@autosk/sandbox`](../daemon/extensions/sandbox/README.md)** — the userspace
  **sandbox library**: the structural `Sandbox` shape plus `worktreeSandbox()`
  (a per-task git worktree), `dockerSandbox({ image })` (a per-task `docker run`
  container), and `sandboxCleanupStep()` (teardown as a normal step). Isolation
  is no longer an engine/SDK concept — an agent wraps its harness with a sandbox
  (see [docs/workflows.md → Isolation](workflows.md#isolation-agent-owned-sandboxes)).
  This package absorbed the retired `@autosk/worktree` + `@autosk/docker`
  providers.
- **[`@autosk/pi-agent`](../daemon/extensions/pi-agent/README.md)** —
  `piAgent({ sandbox?, ... })`, an agent that drives `pi --mode rpc`, mirrors pi's
  transcript entries 1:1, and bridges step transitions through an injected
  `autosk_transit` pi-tool.
- **[`@autosk/feature-dev`](../daemon/extensions/feature-dev/README.md)** — the
  reference workflow `dev → review → docs → validator → accept (human) → cleanup
  → done` (with bounce-backs and a `dev` visit cap), wired to four
  `@autosk/pi-agent` roles each given a per-task `worktreeSandbox()` and a
  `sandboxCleanupStep()` teardown step. It is the workflow every project can
  enroll into with no per-project files.

## Opt-in extensions

Not every extension is bootstrapped. These you add explicitly (with
[`autosk ext add`](#managing-extensions) or from the GUI):

- **[`@autosk/feature-dev-cc`](../daemon/extensions/feature-dev-cc/README.md)** —
  the Claude Code twin of `@autosk/feature-dev`: the same
  `dev → review → docs → validator → accept → cleanup → done` graph driven by
  `@autosk/claude-agent` roles, agents in a per-task `worktreeSandbox()` (claude
  on the host). It is **not** bootstrapped: install it explicitly (`autosk ext
  add npm:@autosk/feature-dev-cc`). (A Claude `dockerSandbox` variant is deferred;
  the thin operator image already lives at `daemon/extensions/claude-agent/docker/`.)
- **[`@autosk/feature-dev-docker`](../daemon/extensions/feature-dev-docker/README.md)** —
  the Docker variant of `@autosk/feature-dev`: the same pi cycle, but every agent
  step runs inside a per-task `dockerSandbox({ image })` container
  (`ghcr.io/wierdbytes/pi-runtime`, reaching the host MCP over
  `host.docker.internal`; image = `$AUTOSK_PI_DOCKER_IMAGE`). Auth rides in via
  the mounted `~/.pi`. Not bootstrapped: `autosk ext add npm:@autosk/feature-dev-docker`.
  Docker isolation is just `dockerSandbox({ image })` from the bootstrapped
  `@autosk/sandbox` — there is no separate isolation-provider extension to add.
- **[`@autosk/merge-to-current`](../daemon/extensions/merge-to-current/README.md)** —
  a single-step workflow that merges a task's `autosk/<task-id>` branch **into
  the branch you currently have checked out**, running non-isolated in the
  project's working tree (`merge → done | human`, with full rollback on failure).
  Not bootstrapped: `autosk ext add npm:@autosk/merge-to-current`.

The agent the Claude workflows use is **not** provisioned by the bootstrap — it
is an opt-in alternative harness you can also wire into your own workflow
(`autosk ext add npm:@autosk/claude-agent`, then use it in place of
`piAgent({...})`):

- **[`@autosk/claude-agent`](../daemon/extensions/claude-agent/README.md)** —
  `claudeAgent({...})`, the structural twin of `@autosk/pi-agent` that drives
  [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude -p`
  headless stream-json) instead of `pi --mode rpc`. It exposes its transit / task /
  comment tools through a per-session, host-side HTTP MCP server (minted by
  `ctx.newMCPServer()` and registered via Claude's `--mcp-config` as a
  `type:"http"` server with a bearer token), so a thin container image needs
  neither `autosk` nor a mounted daemon socket. (The standalone
  [`autoskd mcp`](daemon.md#the-autoskd-mcp-tool-server) stdio server stays for
  external use.)

For the full catalog of these shipped workflows and agents — graphs, step
tables, config knobs, install/enroll how-tos, and an end-to-end tutorial — see
**[docs/shipped.md](shipped.md)**.

## Writing your own

> New to this? Follow the step-by-step [extension tutorial](extensions-tutorial.md)
> instead — it builds the example below in a throwaway project and shows the
> discover → run → break → recover loop end to end.

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
`WorkflowDefinition` / `AgentDefinition` / `StatusStep` contracts, the
[`Sandbox`](workflows.md#isolation-agent-owned-sandboxes) shape, and the
`AgentRunContext` your `onRun` receives.
