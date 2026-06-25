# Tutorial: write your first autosk extension

By the end of this tutorial you'll have a working **project-local extension** —
a single TypeScript file that teaches one project a brand-new workflow — and
you'll have run a task through it. Along the way you'll see the three things that
make the extension system tick:

- the **default-export factory** the daemon calls with an `AutoskAPI`;
- **discovery** — how `autoskd` finds your file under `.autosk/extensions/`;
- **error isolation** — how a broken extension shows up in
  `autosk project diagnostics` instead of taking the daemon down.

This is the learning-oriented companion to
[docs/extensions.md](extensions.md) (the complete reference + the discovery /
provisioning model) and [docs/workflows.md](workflows.md) (the
`WorkflowDefinition` / `AgentDefinition` contracts). When you want the full
surface, go there; here we just build something that runs.

> **Scope.** We use a *project-local* extension (`./.autosk/extensions/`) in a
> throwaway directory, so nothing here touches your global `~/.autosk/`. Delete
> the directory when you're done and it's gone.

---

## What you'll need

- The `autosk` CLI and the `autoskd` daemon on `PATH`. From a checkout of this
  repo that's `make build` (the `autosk` binary) and `make build-autoskd` (the
  daemon); a released install already has both. You never start `autoskd`
  yourself — the CLI auto-spawns it.
- A scratch directory to work in. No Bun, no `npm`, no build step: `autoskd`
  embeds the Bun runtime and imports your `.ts` file directly.

You do **not** need any extension packages installed for this tutorial. Our
extension imports only a *type* from `@autosk/sdk`, and TypeScript type-only
imports are erased before the code runs — so the daemon never has to resolve a
package to load it.

---

## Step 1: create a scratch project

Make a directory and initialize an autosk project in it:

```bash
mkdir /tmp/echo-demo && cd /tmp/echo-demo
autosk init
```

```
initialized /tmp/echo-demo/.autosk
```

`autosk init` lays down the project skeleton — `tasks/`, `sessions/`, and the
`extensions/` directory we're about to use:

```bash
ls .autosk
```

```
extensions  sessions  tasks
```

That `.autosk/extensions/` directory is discovery source #1 — the
highest-precedence place the daemon looks for extensions. Anything you drop in
there belongs to *this* project only.

---

## Step 2: write the extension

An extension is a module with a **default export** that is a factory function.
The daemon calls it once, per project, handing it an
[`AutoskAPI`](../daemon/sdk/src/api.ts) with two methods —
`registerWorkflow(...)` and `registerAgent(...)`. Here we register one tiny
workflow whose single step is an **inline agent**: the step key *is* the agent
name, and the agent's `onRun` does the work.

Create `.autosk/extensions/echo.ts`:

```ts
// .autosk/extensions/echo.ts
import { type AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerWorkflow({
    name: "echo",
    firstStep: "say-hi",
    steps: {
      // The step key "say-hi" is the inline agent's name. An agent step is any
      // object with an `onRun`; the engine runs it for one step and the agent
      // MUST call ctx.transit(...) exactly once before returning.
      "say-hi": {
        async onRun(ctx) {
          await ctx.comment("hello from the echo workflow 👋");
          await ctx.transit({ status: "done" });
        },
      },
    },
  });
}
```

Three things to notice:

- The import is `import { type AutoskAPI }` — a **type-only** import. It gives
  you editor autocomplete but compiles away to nothing, so the running daemon
  needs no `node_modules` to load this file. (The moment you import a *runtime*
  value — `statusStep`, `piAgent`, `worktreeSandbox` — you'll need that package
  resolvable; see [the reference](extensions.md#discovery-order).)
- `onRun` receives a run context (`ctx`). `ctx.comment(...)` posts a task
  comment; `ctx.transit({ status: "done" })` moves the task to its terminal
  status. Every agent step must transit exactly once — returning without it
  fails the run and parks the task to `human`.
- There's no separate "register agent" call. The workflow's agents live
  **inline** as step values, so registering the workflow registers them.

---

## Step 3: discover and run it

You didn't restart anything — and you don't need to. The daemon builds a
project's registry the first time that project is opened, and we only just
created this one. The first command that touches it will discover `echo.ts`:

```bash
autosk workflow list
```

```
NAME         FIRST_STEP  STEPS
echo         say-hi      say-hi
feature-dev  dev         accept,cleanup,dev,docs,review,validator
```

There's `echo`. (You'll also see `feature-dev`, the reference workflow the daemon
[provisions on first run](extensions.md#first-run-bootstrap) into your global
`~/.autosk/` — every project discovers it. Ignore it for now; if your machine
is air-gapped or you opted out of the bootstrap, you may not have it at all.)

Now create a task and enroll it into your workflow in one go:

```bash
autosk create "Try the echo workflow" --workflow echo
```

```
ask-7Q2K9F
```

The daemon schedules the `say-hi` step in the background, runs your `onRun`,
posts the comment, and transits the task to `done`. That's your first extension
running. 🎉

---

## Step 4: verify what happened

Check the task — it should have landed at `done` (its `workflow`/`step` are
kept so you can see where it ran):

```bash
autosk show ask-7Q2K9F
```

```
[ask-7Q2K9F]: Try the echo workflow
status:        done
workflow:      echo
step:          say-hi
blocked:       no
blocked_by:    -
blocks:        -
comments:      1
...
```

And the comment your agent posted — note the **author** is `say-hi`, the agent
(step) name:

```bash
autosk comment list ask-7Q2K9F
```

```
ID       AUTHOR  CREATED              TEXT
cmt-...  say-hi  2026-06-24 12:00:00  hello from the echo workflow 👋
```

You can also see the run itself in the session list:

```bash
autosk session list --task ask-7Q2K9F
```

Each agent step gets one session row — here, the single `say-hi` run.

---

## Step 5: break it, then recover

A broken extension never takes the daemon down — the failure is **caught,
recorded, and isolated** so the rest of the registry keeps working. Let's prove
it.

Edit `.autosk/extensions/echo.ts` and make the factory throw:

```ts
// .autosk/extensions/echo.ts
import { type AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  throw new Error("kaboom");          // simulate a buggy extension
  autosk.registerWorkflow({ /* ... */ });
}
```

Editing code is **not** hot-reloaded: the registry the daemon built when it
first opened this project is cached for the daemon's lifetime. To pick up the
change, restart the daemon — the next `autosk` command auto-spawns a fresh one:

```bash
pkill autoskd        # stop the running daemon; the next command respawns it
```

Now ask the project for its load diagnostics:

```bash
autosk project diagnostics
```

```
project: /tmp/echo-demo
extension load errors (1):
  - /tmp/echo-demo/.autosk/extensions/echo.ts: factory threw: kaboom
```

The error is tagged with the offending source. And because the factory threw,
the workflow it would have registered is simply gone — while everything else
(the daemon, and the globally-provisioned `feature-dev`) is untouched:

```bash
autosk workflow list
```

```
NAME         FIRST_STEP  STEPS
feature-dev  dev         accept,cleanup,dev,docs,review,validator
```

That's error isolation: one broken extension dropped out, the rest of the
registry kept working, and the daemon never went down. (The desktop GUI shows
the same diagnostics as a ⚠ badge on the project switcher.)

Now fix it: restore the working version from Step 2, restart once more, and
`echo` is back alongside `feature-dev`:

```bash
pkill autoskd
autosk workflow list
```

```
NAME         FIRST_STEP  STEPS
echo         say-hi      say-hi
feature-dev  dev         accept,cleanup,dev,docs,review,validator
```

---

## What you built

You wrote a real autosk extension and saw the whole lifecycle:

- **The model.** A default-export factory, called with an `AutoskAPI`, that
  `registerWorkflow`s a workflow whose steps are inline agents.
- **Discovery.** Dropping a `.ts` file into `./.autosk/extensions/` is all it
  takes for *this* project to pick it up — no install, no config.
- **Reload model.** Editing an installed extension's *code* is restart-only (the
  registry is cached for the daemon's lifetime). Adding or removing an extension
  hot-applies to open projects — `autosk ext add`/`remove` do it automatically,
  and `autosk ext reload` re-applies a directory drop-in on demand.
- **Error isolation.** A throwing factory becomes a `project.diagnostics` entry,
  not a crash.

### Where to go next

- **A real agent.** Tutorial 2,
  [a practical Claude Code workflow](extensions-tutorial-claude.md), replaces the
  toy `onRun` with `@autosk/claude-agent` — Claude codes in a per-task git
  worktree, parks at a human gate, and leaves its work on a branch you merge.
- **A whole pipeline.** The reference workflow `feature-dev` chains several
  `@autosk/pi-agent` roles (`dev → review → docs → validator → accept`). See
  [docs/workflows.md → The reference workflow](workflows.md#the-reference-workflow-feature-dev).
- **Real harnesses & isolation.** Replace the toy `onRun` with `piAgent({...})`
  or `claudeAgent({...})`, and give each step a per-task git worktree with
  `worktreeSandbox()`. See
  [docs/workflows.md → Isolation](workflows.md#isolation-agent-owned-sandboxes).
- **Ship it.** Publish your extension as an npm package and add it with
  `autosk ext add npm:<spec>`, or make it global by dropping it in
  `~/.autosk/extensions/`. See
  [docs/extensions.md → Managing extensions](extensions.md#managing-extensions).
- **The full contract.** The complete `WorkflowDefinition` /
  `AgentDefinition` / `AgentRunContext` surface lives in
  [docs/workflows.md](workflows.md); the discovery, precedence, provisioning,
  and error-isolation rules live in [docs/extensions.md](extensions.md).
