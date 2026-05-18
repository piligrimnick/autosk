# autosk — agent packages plan

**Date:** 2026-05-18
**Status:** Locked (decisions taken in `ask_user` rounds 2026-05-18).
**Predecessors:** [`20260517-Workflows-Plan.md`](20260517-Workflows-Plan.md),
[`20260517-Daemon-Plan.md`](20260517-Daemon-Plan.md).
**Compat:** none. The TOML-on-disk agent config format introduced in v0.2 is
removed entirely. No auto-migration: projects that still reference
file-only agents are expected to install npm packages to provide them.

---

## 1. Purpose

Replace `.autosk/agents/<name>.toml` with **npm-package agents**: an agent
is an npm package installed into a global autosk-owned prefix, and the
agent's name in the DB is the **full npm package name** (e.g.
`@autosk/developer`).

Two flavours, behind one schema:

1. **Standard agent** — package declares prompts + pi options. The autosk
   daemon executor still spawns `pi --mode rpc`, just configured from the
   package instead of from a project-local TOML.

2. **Custom agent** — package exports a TS/JS `runAgent(ctx)` function.
   The daemon spawns a tiny Node bootstrapper (`@autosk/agent-runtime`)
   that imports the runner via `tsx`, builds a typed `RunContext`, and
   awaits it. The runner is free to do anything — call out to any LLM, run
   a deterministic shell pipeline, delegate back into pi via
   `ctx.spawnPi(...)`. Closure is observed the same way for both
   flavours: `step_signals` row inserted via `autosk step next`.

Wiring the new system removes the TOML-config code path entirely. There
is **one** lookup ("resolve npm-package agent config") and **two**
spawning branches in the executor (`runner` present vs absent).

---

## 2. Locked decisions

| Topic | Decision |
|---|---|
| **Custom-agent boundary** | JS/TS module exporting `runAgent(ctx)`. autosk spawns Node + `tsx` and calls it. Process-only runners and language-agnostic runners are explicitly deferred. |
| **Discovery** | Explicit registration only — `autosk agent install <pkg>`. No filesystem scan, no Node `require`-resolution fallback. |
| **Naming** | The agent's name in `agents.name` (and in workflow JSON) is the **full npm package name** (`@autosk/developer`). No aliasing, no `--as`. One package = one agent name. |
| **Install scope** | Global. All packages live in `~/.autosk/packages/` (one npm prefix per OS user). |
| **TS support** | `tsx` is the loader; runners may be authored in `.ts` directly. No prebuild required by package authors (matches pi-extension UX). |
| **Per-project overrides** | **Not in scope** for this plan. The package is the source of truth. Overrides will get a follow-up plan when the first real need arises. |
| **TOML agent configs** | Removed. `internal/agent/config.go` is deleted. `.autosk/agents/<name>.toml` files left in the repo are ignored, not parsed; the doc tells users they can `rm -rf .autosk/agents/`. |
| **Migration** | None. Projects that had file-based agents must install (or author) replacement packages and re-create their agent rows. The first executor spawn for a missing package fails fast with `agent_not_installed: <name>`. |

Open items deferred to a future plan: per-project config overrides;
worktree-isolated runs per custom runner; no-Node fallback for users
without a Node toolchain; package signing / trust policy.

---

## 3. High-level shape

```
┌──────────────────────────────────────────────────────────────────────┐
│ autosk daemon serve                                                  │
│                                                                      │
│  poller ──▶ scheduler ──▶ executor                                   │
│                              │                                       │
│                              ▼                                       │
│           pkgregistry.Resolve("@autosk/developer")                   │
│            └─▶ ~/.autosk/packages/node_modules/@autosk/developer/    │
│                  package.json                                        │
│                                                                      │
│            ┌─────── runner present? ───────┐                         │
│            │                               │                         │
│            ▼ no                            ▼ yes                     │
│   spawn pi --mode rpc           spawn node + tsx + bootstrapper      │
│   (first_message, model,          ↳ stdin = JSON(RunContext)         │
│    thinking, extra_args,          ↳ imports runner, awaits           │
│    pi_extensions, pi_skills)         runAgent(ctx)                   │
│                                                                      │
│            ┌──────── both branches converge ────────┐                │
│            ▼                                                         │
│   watch step_signals (unchanged) → kickback → advance task           │
└──────────────────────────────────────────────────────────────────────┘
```

The poller, scheduler, `step_signals`, kickback, and task advance from
v0.2 are unchanged. Only the executor's "load agent config + spawn pi"
section changes shape.

---

## 4. Data model

### 4.1 Schema (no change)

The v0.2 schema (`001_init.sql`, `002_daemon_runs.sql`) is unchanged. In
particular `agents (id, name, is_human, created_at)` stays exactly as is.

What changes is **what valid values of `agents.name` mean**:

- `human` — seeded by migrations, identifies a human caller.
- `<npm-pkg-name>` — must match a row in `~/.autosk/packages/registry.json`.

`agent.Store.EnsureByName` gains a validation step (§6.2): for any name
other than `human`, the package must be installed. Otherwise the call
fails with `ErrAgentNotInstalled` (new sentinel).

### 4.2 Global packages directory layout

```
~/.autosk/packages/
  package.json              # autosk-managed; tracks installed agent pkgs as deps
  package-lock.json         # autosk-managed
  registry.json             # autosk-managed; index of installed agent packages
  node_modules/
    @autosk/agent-runtime/  # bootstrapper (auto-installed dep)
    @autosk/developer/      # an installed standard agent
    @autosk/lint-fixer/     # an installed custom agent
    tsx/                    # transitive (peer of @autosk/agent-runtime)
    ...
```

`registry.json`:

```jsonc
{
  "schema_version": 1,
  "agents": {
    "@autosk/developer":  { "version": "0.3.1", "installed_at": "2026-05-18T16:42:11Z" },
    "@autosk/lint-fixer": { "version": "1.2.0", "installed_at": "2026-05-18T16:45:02Z" }
  }
}
```

`registry.json` is the source of truth for "is this name a real installed
agent". `package.json`/`package-lock.json` are managed by `npm` and exist
for reproducibility and `npm update`.

`autosk agent install` is the only writer of this directory. autosk shells
out to `npm` (`npm --prefix ~/.autosk/packages install <pkg>@<ver>`),
reads back the resolved `version` from the installed `package.json`, and
appends/updates the registry entry.

`@autosk/agent-runtime` is installed lazily on the first
`autosk agent install` (or eagerly by `autosk init --global` /
`autosk agent runtime install`). It is not in `registry.json` because it's
not an agent itself.

### 4.3 Package shape: `autosk.agent` block in `package.json`

```jsonc
{
  "name":    "@autosk/developer",
  "version": "0.3.1",
  "type":    "module",
  "autosk": {
    "agent": {
      // Optional. If present → custom runner.
      "runner": "./src/agent.ts",

      // The fields below are read when `runner` is absent (standard agent).
      // They are ignored when `runner` is present (the runner can call
      // ctx.spawnPi(...) itself if it wants pi semantics).
      "model":              "sonnet:high",
      "thinking":           "high",
      "first_message":      "Inline prompt string.",
      "first_message_file": "./prompts/first_message.md",   // alt to first_message
      "extra_args":         ["--no-tool", "web_fetch"],
      "pi_extensions":      ["./extensions/foo.ts"],
      "pi_skills":          ["./skills"]
    }
  },
  "peerDependencies": {
    "@autosk/agent-sdk": ">=0.1.0 <0.2.0"
  }
}
```

> `first_message`(_file) is the text the executor prepends to the very
> first user turn the spawned pi child sees. It is **not** a system-role
> prompt — pi has its own system prompt; this is just the leading block
> of the user-side prompt. Earlier drafts called this `system_prompt`;
> the rename happened on 2026-05-18.

Validation on `agent install`:

- `name` is required and ≠ `human`.
- `autosk.agent` block is required.
- Exactly one of `first_message`, `first_message_file` may be set; both
  are optional if `runner` is set.
- `thinking`, if present, must be one of
  `off|minimal|low|medium|high|xhigh`.
- `runner`, if present, must be a relative path inside the package and
  exist on disk.
- `first_message_file`, `pi_extensions`, `pi_skills` paths are resolved
  relative to the package install dir, and must exist.

Unknown keys under `autosk.agent` are warned about (not rejected — npm
ecosystems tolerate forward-compat keys).

---

## 5. `@autosk/agent-sdk` and `@autosk/agent-runtime`

Two npm packages, both authored in this repo (under
`extension/runtime/` and `extension/sdk/` — see §10 layout), versioned
together with the autosk binary.

### 5.1 `@autosk/agent-sdk`

Types-only package (peer dep of every custom agent). Defines the
`RunContext` and `RunAgent` types. No runtime code.

```ts
// @autosk/agent-sdk/index.ts

export interface TaskSnapshot {
  id:          string;
  title:       string;
  description: string;
  status:      "in_workflow";   // executor only spawns for in_workflow tasks
  priority:    number;
  workflow_id: string;
  current_step_id: string;
  created_at:  string;          // ISO-8601
  updated_at:  string;
}

export interface StepSnapshot {
  id:    string;
  name:  string;
  agent: string;                // = npm package name of the agent
}

export interface WorkflowSnapshot {
  id:   string;
  name: string;
  description: string;
}

export interface CommentSnapshot {
  id:         number;
  author:     string;
  text:       string;
  created_at: string;
}

export interface Transition {
  kind:        "step" | "task_status";
  target:      string;          // sibling step name, or one of done|cancelled|human_feedback
  prompt_rule: string;
}

export interface CliResult {
  stdout: string;
  stderr: string;
  code:   number;
}

export interface PiSpawnOpts {
  firstMessage?: string;
  model?:        string;
  thinking?:     "off" | "minimal" | "low" | "medium" | "high" | "xhigh";
  extraArgs?:    string[];
  extensions?:   string[];
  skills?:       string[];
}

export interface PiResult {
  exitCode: number;
}

export interface RunContext {
  task:        TaskSnapshot;
  step:        StepSnapshot;
  workflow:    WorkflowSnapshot;
  comments:    CommentSnapshot[];
  transitions: Transition[];

  projectRoot: string;
  jobId:       string;
  agentName:   string;   // = the npm package name of this agent

  cli(args: string[]):                 Promise<CliResult>;
  stepNext(to: string):                Promise<void>;
  spawnPi(opts: PiSpawnOpts):          Promise<PiResult>;
}

export type RunAgent = (ctx: RunContext) => Promise<void>;
```

`@autosk/agent-sdk` ships with these types and an `assertExactlyOneStepNext`
helper used by `@autosk/agent-runtime` to validate the runner's exit
contract.

### 5.2 `@autosk/agent-runtime`

Runtime/bootstrapper. Installed as a dep of the autosk packages prefix
(not as an agent). Exposes one CLI entry: `bootstrap`.

Invocation (executor → bootstrapper):

```
node --import tsx \
     <packages-prefix>/node_modules/@autosk/agent-runtime/dist/bootstrap.js \
     --pkg @autosk/developer \
     --runner ./src/agent.ts
```

stdin: JSON-encoded `RunContextSeed` (everything in `RunContext` except
the function members). stdout/stderr: streamed back to the executor and
captured into the run's transcript.

The bootstrapper:

1. Parses stdin JSON.
2. Resolves the runner path inside the named package's install dir.
3. Imports it (`await import(...)`).
4. Builds the function members:
   - `cli(args)` → spawns `autosk` (resolved via `AUTOSK_BIN` env or PATH)
     with the provided args, `cwd = projectRoot`, env-passes
     `AUTOSK_AGENT = agentName` so the CLI records the right author.
   - `stepNext(to)` → convenience over
     `cli(["step", "next", task.id, "--to", to])`.
   - `spawnPi(opts)` → spawns `pi --mode rpc` with the same env+cwd as
     the standard branch would, awaits `agent_end`, returns `{exitCode}`.
     Errors are surfaced as rejected promises. The pi-extension is
     wired so any `autosk step next` calls inside pi land in the parent
     run's `step_signals`.
5. `await module.default(ctx)`.
6. On rejection: writes the error to stderr and exits non-zero.
7. On success: exits zero. The executor (Go side) inspects
   `step_signals` exactly like it does for the pi branch.

The bootstrapper does **not** touch the autosk DB directly. Anything it
needs to persist goes through `ctx.cli(...)`.

---

## 6. Executor changes

### 6.1 Agent config resolution (`internal/agent/pkgregistry`)

New package. Exposes:

```go
package pkgregistry

type PackageConfig struct {
    Name             string   // = directory + name in package.json
    Version          string
    InstallDir       string   // absolute
    Runner           string   // "" → standard agent; else absolute path
    Model            string
    Thinking         string
    FirstMessage     string   // inlined: file content read at resolve time
    ExtraArgs        []string
    PiExtensions     []string // absolute paths
    PiSkills         []string // absolute paths
}

type Registry struct { /* opaque; backed by ~/.autosk/packages/registry.json */ }

func Open(prefixDir string) (*Registry, error)
func Default() (*Registry, error) // honours $AUTOSK_PACKAGES; defaults to ~/.autosk/packages

func (r *Registry) Resolve(name string) (PackageConfig, error)        // ErrNotInstalled if missing
func (r *Registry) List() ([]Entry, error)
func (r *Registry) Install(ctx, name, version string) (Entry, error)  // shells npm
func (r *Registry) Uninstall(ctx, name string) error
func (r *Registry) EnsureRuntime(ctx) error                           // installs @autosk/agent-runtime if absent
```

`Resolve` is the only entry the executor uses. It does the I/O once per
spawn (no in-memory caching in v1 — file is small).

### 6.2 Wiring into the executor

`internal/daemon/executor/executor.go` change:

- Replace
  ```go
  agentCfg, err := agent.Load(e.cfg.ProjectRoot, stepRow.AgentName)
  ```
  with
  ```go
  pkgCfg, err := e.deps.Packages.Resolve(stepRow.AgentName)
  ```
  (`Deps` gains a `Packages *pkgregistry.Registry`).

- Render the prompt header from `pkgCfg.FirstMessage` exactly like before.

- Branch on `pkgCfg.Runner`:

  - **runner == ""** → existing pi-spawn path, but use
    `pkgCfg.Model`, `pkgCfg.Thinking`, `pkgCfg.ExtraArgs`,
    `pkgCfg.PiExtensions`, `pkgCfg.PiSkills` (the last two added as
    `--extension <path>` and `--skill <path>` args appended to
    `ExtraArgs` — same convention pi uses for its own CLI).

  - **runner != ""** → new path:
    - Build a `RunContextSeed` (task/step/workflow/comments/transitions
      snapshots + project root + job id + agent name).
    - Spawn
      `node --import tsx <runtime-bootstrap-js> --pkg <name> --runner <path>`,
      cwd = `e.cfg.ProjectRoot`, env passes `AUTOSK_BIN` (absolute path
      to the current `autosk` binary) and `AUTOSK_AGENT = stepRow.AgentName`.
    - Pipe stdin = JSON seed.
    - Wait for exit. The same `step_signals` polling that today follows
      `WaitForAgentEnd` is run after the process exits.

- The kickback loop, `step_signals` PK enforcement, and the
  `advanceTask` block are unchanged.

### 6.3 PiRunner abstraction tightening

To accommodate the two branches with minimal duplication, introduce a
`Runner` interface that both `pi.Runner` and the new `node.Runner` (in
`internal/daemon/agentnode/`) implement:

```go
type Runner interface {
    PID() int
    Wait(ctx context.Context, grace time.Duration) (int, error)
    Terminate() error
    Kill() error
    // pi-only methods (no-op or unsupported on node runner):
    SendPrompt(ctx context.Context, message string) error
    WaitForAgentEnd(ctx context.Context) error
    Abort(ctx context.Context) error
    CloseStdin() error
    GetState(ctx context.Context) (pi.SessionInfo, error)
    Events() <-chan pi.Event
}
```

The node runner's prompt/turn behaviour collapses into a single "wait for
process exit" cycle: there are no turn boundaries with a non-pi child.
`SendPrompt` writes the JSON seed once at start; subsequent calls are
no-ops (the agent contract is one process = one turn). `WaitForAgentEnd`
becomes `Wait`. The kickback path is therefore never used for custom
runners: a runner that exits without `step_signals` is a hard failure
with `error=agent_did_not_emit_transition`, no retries. Document this
distinction.

---

## 7. CLI surface

### 7.1 New / changed commands

```
Agents (changed)
  autosk agent install <npm-name> [--version SPEC]
  autosk agent uninstall <npm-name>
  autosk agent list   [--json]              # shows installed packages + DB rows
  autosk agent show <npm-name> [--json]     # union of registry entry + DB row
  autosk agent runtime install              # eager @autosk/agent-runtime install
  autosk agent runtime version              # prints installed runtime version

Removed
  autosk agent create <name>               # use `autosk agent install` instead
                                            # (kept as `--force` escape hatch?
                                            #  → no in v1, gated removal)
```

`autosk agent install`:

1. Ensures the global prefix exists (creates dir + initial `package.json`).
2. Ensures `@autosk/agent-runtime` is installed (lazy first-call install).
3. Shells `npm --prefix ~/.autosk/packages install <pkg>@<ver>` (default `@latest`).
4. Reads the installed package's `package.json`, validates the
   `autosk.agent` block, rolls back the npm install on validation failure.
5. Writes the registry entry.
6. Prints the resolved version + a sample workflow snippet referencing
   the new agent name.

`autosk agent uninstall`:

1. Refuses if any workflow step references the agent and `--force` is not given.
2. Shells `npm --prefix ... uninstall <pkg>`.
3. Removes the registry entry.

`autosk agent list` is now a union of:

- Installed packages (`registry.json`).
- DB rows in `agents` (mostly: `human` + any rows lazily inserted from
  workflow inserts).

Format:

```
NAME                    SOURCE      VERSION   IN_DB
human                   builtin     -         yes
@autosk/developer       package     0.3.1     yes
@autosk/lint-fixer      package     1.2.0     no
@orphan/agent           db_only     -         yes     # workflow references it; package not installed
```

The `db_only` row is the diagnostic surface for "this project's workflows
reference an agent that no one has installed".

### 7.2 `autosk init`

`autosk init` already ensures `.autosk/db`. We extend it (idempotently)
to also ensure `~/.autosk/packages/` exists with an initial empty
`package.json` and an empty `registry.json`. We do **not** auto-install
the runtime here — that's done lazily on the first `agent install`.

### 7.3 Workflow JSON

Workflow JSON's `"agent"` is the per-step agent object
`{ "name": "...", "params": {...} }`; `name` is the full npm package
name. The bare-string form (`"agent": "name"`) used in earlier drafts
is no longer accepted — the parser rejects it with a hint to switch to
the object form.

Validation:

- `workflow create --file` requires every referenced agent name to either
  be `human` or resolve to an installed package (so workflows can't be
  imported referencing a package the local user hasn't installed).
- Synthetic `single:<agent>` names are generated as
  `single:<urlencoded-pkg-name>` to avoid clashing with the reserved
  workflow-name prefix. (Slashes in `@scope/name` are escaped; the
  resulting synthetic workflow name is never user-typed.)
- `agent.params` (optional) overrides the standard agent fields
  (`model`, `thinking`, `first_message`/`first_message_file`,
  `extra_args`, `pi_extensions`, `pi_skills`) per step. Override
  semantics, the closed key set and the runner-package guard are
  documented in [`docs/workflows.md`](../workflows.md#per-step-agent-overrides).

---

## 8. Migration & rollout

There is no migration. The first executor spawn after the upgrade for a
project that still has workflows pointing at "file-based" agent names
will fail fast with `agent_not_installed: <name>` and that name is
shown in `autosk daemon list` / `daemon status` / `autosk agent list`.

Recovery for an existing project:

1. `autosk agent install <pkg>` for each agent referenced by workflows.
2. (Optional) `rm -rf .autosk/agents/` — the TOML files are now ignored.

The README upgrade notes spell this out (one bullet, one sample command).

---

## 9. Phases

Each phase is one autosk task, blocked by its predecessor unless noted.

| ID | Phase | Done when |
|---|---|---|
| **A0** | **Plan doc** | This file lands. Tracked as `as-b497`. |
| **A1** | **Drop TOML config + replace `agent.Load`** | `internal/agent/config.go` and its test removed. New `internal/agent/pkgregistry` package with `Resolve`/`List`/`Install`/`Uninstall`/`EnsureRuntime`. Executor switched over (standard branch only, custom branch stubbed). `EnsureByName` rejects non-`human` names that aren't installed. `as-a387`. |
| **A2** | **CLI: `agent install/uninstall/list/show` + lazy runtime install** | New CLI verbs. `autosk init` ensures `~/.autosk/packages/`. `agent list` shows the union table. End-to-end: install a tiny fixture package (`testdata/pkg-foo/`) into a tmp prefix and assert resolve. `as-146a`. |
| **A3** | **Executor: standard branch parity** | Standard pi spawn driven by `pkgCfg` (`PiExtensions`/`PiSkills` appended to `ExtraArgs`). Golden tests cover the rendered prompt and `pi.Opts`. `as-1860`. |
| **A4** | **Executor: custom branch — Node bootstrapper + `@autosk/agent-runtime`** | `extension/runtime/` (TS sources) + bundled `bootstrap.js`. New `internal/daemon/agentnode` runner spawns `node --import tsx <bootstrap.js>`, feeds JSON seed on stdin, waits for exit, surfaces stdout/stderr. End-to-end test with a fixture custom runner that calls `ctx.stepNext("done")`. `as-d006`. |
| **A5** | **Docs + acceptance scenario** | `docs/workflows.md` and README updated; new sample agent under `docs/notes/agent-package-example/`. Acceptance: install fixture pkg, create task with `--agent <fixture>`, observe daemon run it to completion. `as-03b9`. |

A0 blocks A1+A2; A1+A2 block A3; A3 blocks A4 (so the standard branch is
green before the custom branch is added on top); A4 blocks A5.

---

## 10. Layout summary

```
cmd/autosk/
  agent.go                          # install/uninstall/list/show + runtime subcmd
internal/
  agent/
    pkgregistry/
      registry.go                   # ~/.autosk/packages I/O
      resolve.go                    # PackageConfig builder
      install.go                    # npm shell + validation
      registry_test.go
      testdata/
        pkg-foo/                    # standard-agent fixture
        pkg-custom/                 # custom-runner fixture
    store.go                        # unchanged
    config.go                       # DELETED
    config_test.go                  # DELETED
  daemon/
    agentnode/                      # NEW — Node bootstrapper runner
      runner.go
      runner_test.go
    executor/
      executor.go                   # branches on pkgCfg.Runner
      executor_test.go              # both branches exercised
      fs.go                         # unchanged
extension/
  runtime/                          # @autosk/agent-runtime
    package.json
    src/bootstrap.ts
    src/helpers.ts
    dist/bootstrap.js               # built artifact, shipped with the package
  sdk/                              # @autosk/agent-sdk (types-only)
    package.json
    src/index.ts
docs/
  plans/
    20260518-Agent-Packages.md      # this file
  workflows.md                      # updated: agents come from npm packages
  notes/
    agent-package-example/          # tiny example custom-runner package
README.md                           # agents subcommand reference updated
```

---

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| User has no Node toolchain on PATH. | `autosk agent install` and the custom-runner branch both detect missing `node`/`npm` and fail with a clear error. Standard agents (without a runner) only need `node`/`npm` for the install step; runtime spawn still uses pi only. (Open: a TODO to lift the install-time Node requirement by vendoring an installer.) |
| `npm install` is slow / network-dependent. | `autosk agent install` is the only path that hits npm. Once `registry.json` is populated, subsequent autosk spawns do zero network I/O. Lockfile under the prefix means CI restore is `npm ci --prefix ~/.autosk/packages`. |
| Package-prefix shared across projects → version drift between projects. | Acceptable for v1: the package version is global. A future per-project override plan can pin per-project versions if/when needed. |
| `~/.autosk/packages` becomes a global junk drawer. | `autosk agent uninstall` exists; `agent list` shows the inventory; `npm dedupe` works inside the prefix. |
| `tsx` ESM loader changes break the bootstrapper. | Pin `tsx` peer-version in `@autosk/agent-runtime`; CI tests the bootstrapper end-to-end on a Node LTS matrix. |
| Custom runner hangs without emitting a signal. | The executor's idle/process-timeout still applies. Custom runners have **no kickback** (one shot per process); the run fails with `agent_did_not_emit_transition` immediately on clean exit without a signal. |
| `ctx.cli(...)` from inside a runner can call destructive autosk verbs. | Same trust boundary as today's pi-spawn: anything pi can do, a runner can do. Documented in §security of `docs/workflows.md`. |
| Two packages claim conflicting names by mistake. | Cannot: npm registry enforces unique names. Local dev (linked packages) is the only way to collide; `agent install` refuses if a different package is already installed under the same name. |
| Schema drift between `@autosk/agent-sdk` and autosk's `RunContextSeed`. | Both are versioned in this repo and released together. Bootstrapper checks `process.env.AUTOSK_RUNTIME_SCHEMA_VERSION` matches what the SDK was built against; mismatch → hard fail. |
| Removing `.autosk/agents/<name>.toml` is a breaking change for existing v0.2 projects. | This is the plan. Single line in upgrade notes: "install matching packages, then `rm -rf .autosk/agents/`". |

---

## 12. Tracking

Tracked as autosk umbrella task **`as-13d6`** with phase tasks:

- **A0** plan doc — `as-b497`
- **A1** drop TOML / pkgregistry skeleton — `as-a387`
- **A2** CLI verbs — `as-146a`
- **A3** executor standard branch — `as-1860`
- **A4** executor custom branch — `as-d006`
- **A5** docs + acceptance — `as-03b9`

Each task is in its own `single:human` workflow until the implementation
agents come online to take over.
