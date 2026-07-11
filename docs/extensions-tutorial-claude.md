# Tutorial: a practical Claude Code workflow

The [first tutorial](extensions-tutorial.md) built a toy workflow whose "agent"
just posted a comment. This one is practical: you'll wire a **real harness** —
[`@autosk/claude-agent`](../daemon/extensions/claude-agent/README.md), which
drives Claude Code (`claude -p` headless) — into a small workflow that actually
writes code. Claude runs **isolated in a per-task git worktree**, parks at a
**human review gate**, and a **cleanup step** tears the worktree down — leaving
its commits on a branch you merge.

The workflow is deliberately small: `dev → accept (human) → cleanup → done`.

> **Already happy with a ready-made pipeline?** The shipped
> [`@autosk/feature-dev-cc`](extensions.md#opt-in-extensions) is a full
> Claude Code workflow (`dev → review → docs → validator → accept → cleanup`).
> Install it with `autosk ext add npm:@autosk/feature-dev-cc` and skip to
> [Step 4](#step-4-run-a-task-through-it). This tutorial shows how to *implement*
> your own.

---

## What you'll need

- `autosk` + `autoskd` on `PATH` (as in the [first tutorial](extensions-tutorial.md)).
- The **`claude` CLI**, already **authenticated**. The run is unattended — a
  headless Claude that hits an auth or permission prompt aborts. Point a custom
  binary at it with `$AUTOSK_CLAUDE_BIN` if it isn't named `claude` on `PATH`.
- **`npm`** (to install the extension's dependencies) and **`git`**: the project
  must be a git repo **with at least one commit** (the worktree branches from
  `HEAD`).

Unlike the first tutorial, this extension uses **runtime imports**
(`claudeAgent`, `worktreeSandbox`), not a type-only import. So the loader has to
*resolve* those packages — which is why we ship the extension as a small
**package directory** with its own `node_modules`, rather than a bare `.ts` file.

---

## Step 1: scaffold the extension as a package

Make a directory for the extension (anywhere — we'll reference it in place) with
a `package.json` that both declares the entry point (`autosk.extensions`) **and**
lists its dependencies:

```bash
mkdir -p ~/autosk-claude-flow/prompts && cd ~/autosk-claude-flow
```

```jsonc
// ~/autosk-claude-flow/package.json
{
  "name": "my-claude-flow",
  "private": true,
  "type": "module",
  "autosk": { "extensions": ["./index.ts"] },
  "dependencies": {
    "@autosk/sdk": "^0.1.0",
    "@autosk/claude-agent": "^0.1.0",
    "@autosk/sandbox": "^0.1.0"
  }
}
```

Install the dependencies **into this directory** — that local `node_modules` is
what lets the loader resolve the imports in `index.ts`:

```bash
npm install
```

```
added 3 packages
```

Now write the role prompt that seeds Claude's first message. The transition is
yours to drive: tell Claude to **commit** its work (uncommitted changes are lost
when the worktree is removed) and to **call the transit tool** to hand off:

```markdown
<!-- ~/autosk-claude-flow/prompts/dev.md -->
You are the `dev` agent. Implement the task described in this task's
title/description and comments.

When you are done:
1. Make sure the project still builds and its tests pass.
2. Commit your changes on the current branch (this is a dedicated worktree
   branch — commit, don't stash).
3. Call the `transit` tool with target step `accept` to hand off for human
   review.

If the task is unclear or impossible, call `transit` with status `human` and
explain why in a comment.
```

---

## Step 2: wire the workflow

Create `index.ts`. The default export is the factory; it registers one workflow
whose `dev` step is a `claudeAgent` running inside a per-task
`worktreeSandbox()`, gated by a human `accept` step, and torn down by
`sandboxCleanupStep`:

```ts
// ~/autosk-claude-flow/index.ts
import { statusStep, type AutoskAPI } from "@autosk/sdk";
import { claudeAgent } from "@autosk/claude-agent";
import { sandboxCleanupStep, worktreeSandbox } from "@autosk/sandbox";

export default function (autosk: AutoskAPI) {
  // One per-task git worktree, shared by every agent step and the cleanup step.
  const sandbox = worktreeSandbox();

  autosk.registerWorkflow({
    name: "claude-fix",
    description: "Tiny Claude Code flow: dev → accept (human) → cleanup → done",
    firstStep: "dev",
    steps: {
      // The step key "dev" IS the agent name. Claude runs in the worktree;
      // dangerouslySkipPermissions is safe because the worktree is the sandbox
      // and the run is unattended (a permission prompt would abort it).
      dev: claudeAgent({
        sandbox,
        model: "sonnet",
        firstMessageFile: new URL("./prompts/dev.md", import.meta.url).pathname,
        dangerouslySkipPermissions: true,
      }),
      // A human gate: the task parks here for your review.
      accept: statusStep("human"),
      // Removes the worktree (branch preserved), then transits to `done`.
      cleanup: sandboxCleanupStep(sandbox),
    },
  });
}
```

That's the whole extension. A few things worth knowing:

- **No `registerAgent`.** The agent is an inline step value — registering the
  workflow registers it.
- **Isolation is the agent's job.** `worktreeSandbox()` gives every run its own
  `~/.autosk/worktrees/<slug>/<task>` checkout on branch `autosk/<task-id>`;
  `ctx.cwd` stays the project root. (Swap in `dockerSandbox({ image })` to run
  Claude in a container instead — same shape.)
- **Cleanup is a normal step, and it `force`-removes by default.** That's why
  the prompt insists on committing: the branch survives the teardown, the
  worktree does not.

---

## Step 3: install it and confirm it loaded

Reference the directory in place with `autosk ext add` — `-l/--local` scopes it
to the current project. (A local path is recorded as an absolute path and loaded
where it sits; it is never copied, so keep the directory and its `node_modules`
around.)

```bash
cd /path/to/your/git/project
autosk ext add -l ~/autosk-claude-flow
```

```
registered /Users/you/autosk-claude-flow (project scope)
  settings: /path/to/your/git/project/.autosk/settings.json
applied live to 1 open project (no restart needed)
```

The add hot-applies to the open project (no restart) — the workflow is
immediately schedulable. Confirm it resolved cleanly:

```bash
autosk ext list
```

```
SCOPE    KIND   RESOLVED  SOURCE
project  local  yes       /Users/you/autosk-claude-flow
```

```bash
autosk workflow list
```

```
NAME        FIRST_STEP  STEPS
claude-fix  dev         accept,cleanup,dev
```

(You'll also see `feature-dev` and any other installed workflows; we're focused
on `claude-fix`.) `RESOLVED yes` (and an empty `autosk project diagnostics`) means the loader found
your `node_modules` and ran the factory. A `RESOLVED no`, or a `failed to import`
diagnostic, almost always means the deps aren't installed in the package
directory — re-run `npm install` there.

---

## Step 4: run a task through it

Create a task and enroll it in one go:

```bash
autosk create "Add a --version flag to the CLI" --workflow claude-fix
```

```
ask-7Q2K9F
```

The daemon schedules `dev`, spins up the worktree, and launches Claude Code in
it. Watch the run live:

```bash
autosk session list --task ask-7Q2K9F   # one row per agent run
autosk session transcript <session-id>  # the streamed Claude transcript
```

…or open `autosk lazy` for a live, rendered transcript. When Claude finishes and
calls `transit → accept`, the task **parks at `human`**:

```bash
autosk show ask-7Q2K9F
```

```
[ask-7Q2K9F]: Add a --version flag to the CLI
status:        human
workflow:      claude-fix
step:          accept
...
```

> **If it parks unexpectedly** with a comment like `agent_did_not_transit` or a
> failed session, check the transcript — usually the prompt didn't steer Claude
> to call `transit`, or `claude` wasn't authenticated. Fix and re-enroll, or
> `autosk resume ask-7Q2K9F --to dev` to try again.

---

## Step 5: review the work, then finish

Claude's commits are on the branch `autosk/ask-7Q2K9F` (preserved even after the
worktree is gone). Review them:

```bash
git log --oneline autosk/ask-7Q2K9F
git diff main...autosk/ask-7Q2K9F
```

Happy with it? Advance past the human gate — `resume --to cleanup` runs the
cleanup step, which removes the worktree (keeping the branch) and transits to
`done`:

```bash
autosk resume ask-7Q2K9F --to cleanup
autosk show ask-7Q2K9F          # status: done
```

Then merge the branch the normal way:

```bash
git merge autosk/ask-7Q2K9F
```

(Or let autosk do it: the shipped `@autosk/merge-to-current` workflow merges a
task's branch into your current branch as a single step — see
[docs/extensions.md → Opt-in extensions](extensions.md#opt-in-extensions).)

Not happy? `autosk resume ask-7Q2K9F --to dev` sends it back to Claude with your
new comments as context, or `autosk cancel ask-7Q2K9F` drops it (route a real
workflow's `cancel` through `cleanup` too, so the worktree never leaks).

---

## What you built

A small but real workflow where **Claude Code does the coding**, isolated per
task, with a human in the loop:

- a `claudeAgent` step running its harness in a **per-task git worktree**;
- a **human review gate** (`statusStep("human")`) you resume past;
- a **cleanup step** that tears the worktree down but preserves the branch;
- shipped as a **package directory** whose `node_modules` makes the runtime
  imports resolvable — the layout any non-trivial extension needs.

### Where to go next

- **Grow the graph.** Add `review` / `validator` steps and a `dev` visit cap in
  `onTransit`, exactly like the reference
  [`@autosk/feature-dev-cc`](../daemon/extensions/feature-dev-cc/README.md).
- **Containerize it.** Replace `worktreeSandbox()` with
  `dockerSandbox({ image })` to run Claude in a per-task container. See
  [docs/workflows.md → Isolation](workflows.md#isolation-agent-owned-sandboxes).
- **Tune the agent.** `effort`, `permissionMode`, `allowedTools`,
  `appendSystemPrompt`, and more — the full table is in the
  [`@autosk/claude-agent` README](../daemon/extensions/claude-agent/README.md#configuration--claudeagentoptions).
- **Publish it.** Once it's an npm package, install it anywhere with
  `autosk ext add npm:<spec>` (its deps come along) — see
  [docs/extensions.md → Managing extensions](extensions.md#managing-extensions).
