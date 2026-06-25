# Extension hot-reload (`ext add` / `ext remove` without a daemon restart)

Status: proposed · 2026-06-25

## Goal

Let the daemon pick up an **added** or **removed** extension without a restart
and **without disturbing any running session**. Today every `autosk ext add` /
`autosk ext remove` prints a "restart the daemon" hint because the per-project
extension registry is built once at project open and cached for the daemon's
lifetime. We want the new workflows/agents to become schedulable immediately,
and removed ones to stop being scheduled, while in-flight sessions keep running
the code they started with.

### Decisions (locked)

| Topic | Decision |
| --- | --- |
| Scope | **Add + remove** hot-apply. `ext update` / in-place code edits stay restart-only (out of scope — see [Why update is hard](#why-update--in-place-edits-are-out-of-scope)). |
| Trigger | **Both**: (1) auto — `extension.install` / `extension.remove` reload the affected project(s) as part of the RPC, so the CLI drops the restart hint; (2) explicit — a new `autosk ext reload` verb (`extension.reload` RPC) for on-demand reloads and the local-edit workflow. |
| Running sessions | **Never disturbed.** A live session keeps the workflow/agent objects it captured at dispatch; only new dispatch / enroll / resume / interactive-create see the new registry. |
| Mechanism | **Rebuild a fresh `ExtensionRegistry` and atomically swap the reference** on both the project handle and the engine's `EngineProject`. No in-process module-cache busting (impossible in Bun — see below). |
| Global vs project | A **global** add/remove reloads **every currently-open project** (all of them merge global extensions). A `-l/--local` add/remove reloads **only that project**. |

---

## Why a restart is needed today (current state)

Three facts, all verified in the tree:

1. **The registry is built once and cached.** `ProjectManager.open()`
   (`daemon/core/src/project/manager.ts`) calls
   `loadProjectRegistry(root, env)` (`daemon/core/src/extensions/loader.ts`)
   exactly once on first open and stores the result on
   `ProjectHandle.extensions`. There is no rebuild path.

2. **Install/remove only touch disk.** `extension.install` / `extension.remove`
   (`daemon/core/src/rpc/daemon.ts`) delegate to
   `ProjectManager.installExtension` / `removeExtension`, which run `npm install`
   + edit `settings.json` (`daemon/core/src/extensions/install.ts`). They never
   touch the in-memory registry — hence the restart hint printed in
   `cmd/autosk/ext.go:70` (add) and `:137` (remove).

3. **Bun's module cache is the real blocker for *editing* code.**
   `loader.ts` documents it and a web check confirms it still holds (2024–2025):
   Bun caches every imported module by resolved specifier for the process
   lifetime, query-string cache-busting is now *actively broken* (it hangs), and
   there is no `uncachedImport`. So a module path that has **already** been
   imported cannot be re-imported with new code in the same process.

### The key insight: add/remove dodge the cache wall; running sessions are safe

The cache wall only bites when the **same path** must yield **new** code. Split
hot-reload by case:

| Case | Path state | Module cache? | In scope |
| --- | --- | --- | --- |
| **Add** a new extension | never-before-imported path | No — fresh `import()` returns fresh code | ✅ yes |
| **Remove** an extension | path simply stops being discovered | No — the stale cached module is never imported again | ✅ yes |
| **Update / edit in place** | same path, re-imported | **Yes** — Bun returns the cached old module | ❌ no (restart) |

Running sessions are safe because of how the engine consumes the registry:

- The engine resolves workflows/agents **by name from `project.registry`** only
  at *dispatch* (`Engine.dispatch`), *enroll*, *resume*, and
  *createInteractiveSession* (`daemon/core/src/engine/engine.ts`). Fresh lookups,
  every time.
- A **running `SessionRuntime` captures `this.wf` + `this.agent` as object
  references** at construction (`daemon/core/src/engine/session.ts`) and never
  re-resolves from the registry during the run.

So if we build a **new** `ExtensionRegistry` and swap the reference, running
sessions keep their captured (old) objects and finish undisturbed, while every
*new* scheduling decision sees the new set. That is exactly the requirement.

---

## Design

### 1. Rebuild + atomic swap

Add a reload path that:

1. Rebuilds a fresh registry off to the side:
   `const next = await loadProjectRegistry(root, env)` — unchanged loader; for an
   **add** the new entry path imports fresh, for a **remove** discovery no longer
   returns the dropped path, so `next` simply lacks it.
2. **Swaps two references back-to-back** (synchronous, no `await` between):
   - `handle.extensions = next` (the `ProjectHandle`, read by RPC handlers
     `registry.workflow.list` / `registry.agent.list` / `project.diagnostics`,
     each of which reads `handle.extensions` fresh per call).
   - `engineProject.registry = next` via a new `Engine.setProjectRegistry(root,
     next)` (the engine reads `project.registry` fresh on every dispatch).
3. Runs the **live-code hazard guard** against `next`, skipping any task that
   currently has a live session (see §3).
4. `engine.kickScan()` so a newly-available workflow immediately picks up any
   `work`/ready task waiting for it.
5. Emits a notification so GUIs refresh (see §5).

Because Bun/JS is single-threaded and `next` is fully built before the swap,
no concurrent dispatch ever observes a half-built registry. The micro-gap
between the two adjacent assignments is benign: a dispatch landing in it uses the
old (internally consistent, still-valid) engine registry and runs old code; the
`kickScan()` after the swap re-evaluates everything.

### 2. Who orchestrates it

The engine is owned by `Daemon`, and `EngineProject.registry` lives in the
engine — so the `Daemon` is the natural orchestrator (it already holds both
`this.engine` and `this.projectManager`).

- `ProjectManager.rebuildRegistry(root)` (new): builds `next`, swaps
  `handle.extensions`, runs the hazard guard with a skip-live predicate, and
  returns `{ registry, diagnostics, parked, workflows }`. No-op (returns a
  "not open" marker) if the root is not currently open — an unopened project
  rebuilds naturally on its first open, nothing to do.
- `Engine.setProjectRegistry(root, registry)` (new): `p.registry = registry;
  this.kickScan();`. `EngineProject.registry` is already a mutable field
  (`daemon/core/src/engine/types.ts`).
- `Daemon.applyExtensionReload(root)` (new, private): calls
  `rebuildRegistry`, then `setProjectRegistry`, then emits the notification.
  Reused by all three triggers (install / remove / explicit reload).

Scoping (driven by the install/remove result's `scope`):

- `scope:"project"` → `applyExtensionReload(projectRoot)` for the one root.
- `scope:"global"` → `applyExtensionReload(root)` for **every** root in
  `projectManager.loaded()` (each re-merges global + its own project settings).

### 3. The live-code hazard guard under hot-reload

Today `validateInFlightTasks` (`daemon/core/src/extensions/hazard.ts`) runs only
at project open — i.e. on a fresh process with **no live sessions**. Under
hot-reload there can be live sessions, so two changes are needed:

1. **Skip tasks that have a live session.** Add an optional predicate to
   `validateInFlightTasks`, e.g. `opts.isLive?: (taskId) => boolean`, wired to
   `handle.store.sessions.hasLiveSession`. A `work` task whose workflow was just
   removed but is *currently running* must **not** be parked out from under its
   session — it keeps running its captured workflow object and settles normally.

2. **Make the scheduler self-heal a now-orphaned task after its session ends.**
   `Engine.dispatch` currently *silently returns* when `resolveWorkflow` /
   `wf.steps[step]` is missing, deferring to the open-time guard:
   ```ts
   const wf = project.registry.resolveWorkflow(row.workflow);
   if (!wf) return; // unknown workflow — the live-code hazard guard parks it
   const step = wf.steps[row.step];
   if (!step) return; // unknown step — hazard guard parks it
   ```
   With no open-time guard re-running, change these to **park** the task
   (`host.park(project, taskId, "workflow_missing: …")`). A task only reaches
   `dispatch` when it has *no* live session (the scan skips live ones), so
   parking here is always safe. This cleanly handles the remove-with-live-session
   case: the running session finishes on its captured code, and the next scan
   (kicked from `runJob`'s `finally`) reaches `dispatch`, sees the missing
   workflow, and parks the task — no stuck-`work`-forever limbo. It also makes
   the scheduler robust to a missing workflow in general, not just at open.

Net behaviour for **remove**:
- Non-live invalid `work` tasks → parked to `human` immediately at reload.
- A task mid-session on the removed workflow → finishes its current run, then
  parks on the next scan. No disruption to the live run.

### 4. Triggers

**(a) Auto on install/remove.** In the `extension.install` / `extension.remove`
handlers (`daemon/core/src/rpc/daemon.ts`), after the existing
`projectManager.install/remove` call succeeds, run `applyExtensionReload`
scoped by the result's `scope`. The reload is awaited *before* the RPC returns,
so by the time `autosk ext add` exits, the workflow is already live. The result
gains a small field (e.g. `reloaded: boolean` / `reloaded_projects: number`) so
the CLI can print "applied to N open project(s)" instead of the restart hint.

**(b) Explicit `autosk ext reload`.** New verb + `extension.reload` RPC.
Reloads the project resolved from `cwd` (which re-merges global + project), so
it doubles as the escape hatch for the local-edit workflow *for add/remove of
files in `.autosk/extensions/`* (a brand-new local file is a fresh path → picked
up; a removed file → dropped). Editing an existing local file's *contents* still
won't take effect (cache wall) — documented. Returns a summary
`{ root, diagnostics, workflows, parked }`.

> Filesystem-watch auto-reload (watch `settings.json` / `packages/`) is
> deliberately **not** in this plan — it adds debouncing + partial-write races
> during `npm install`. The explicit verb + auto-on-command cover the stated
> need. Listed as a Phase 2 idea.

### 5. Notifications (GUI refresh)

A GUI that cached `registry.workflow.list` / `extension.list` must learn the set
changed. Add a dedicated **`registry-changed`** proto-v2 notification
`{ root }`, emitted from `applyExtensionReload`, and have subscribers re-fetch.
The CLI path does not need it (it re-reads synchronously), so this is a small,
self-contained addition; if we want to defer it, reusing `project-changed` as a
refresh nudge is a stopgap, but a dedicated signal is the correct shape and is
cheap. Mirror it into the Go notification list and the GUI event layer.

---

## Change inventory

### Daemon (`daemon/core`, `daemon/sdk`)

| File | Change |
| --- | --- |
| `daemon/sdk/src/proto.ts` | Add `extension.reload` to `RpcMethodMap` + `RPC_METHODS`; add `ExtensionReloadParams` (`ProjectSelector`) + `ExtensionReloadResult` (`{ root; diagnostics: ExtensionLoadError[]; workflows: string[]; parked: { task_id; error }[] }`). Add `reloaded`/`reloaded_projects` to `ExtensionInstallResult` + `ExtensionRemoveResult`. Add `"registry-changed"` to `RPC_NOTIFICATIONS` + `RegistryChangedParams { root }`. |
| `daemon/core/src/extensions/hazard.ts` | `validateInFlightTasks(store, registry, { author?, isLive? })`: skip tasks where `isLive(id)` is true (don't park out from under a live session). |
| `daemon/core/src/engine/engine.ts` | Add `setProjectRegistry(root, registry)` (swap `EngineProject.registry` + `kickScan`). In `dispatch`, change the two "unknown workflow/step ⇒ return" branches to **park** the task (`this.park(...)`) so an orphaned `work` task self-heals after its session ends. |
| `daemon/core/src/project/manager.ts` | Add `rebuildRegistry(root)`: build `loadProjectRegistry`, swap `handle.extensions`, run `validateInFlightTasks` with the skip-live predicate, return summary. (Optionally factor the build+park block shared with `open()`.) |
| `daemon/core/src/rpc/daemon.ts` | Add private `applyExtensionReload(root)` (rebuild → `engine.setProjectRegistry` → emit `registry-changed`) and `reloadAfterChange(scope, cwd)` (project root vs all loaded). Call it from the `extension.install` / `extension.remove` handlers. Add the `extension.reload` handler. Add `emitRegistryChanged(root)`. |
| `daemon/core/src/extensions/loader.ts` | Update the "Reload semantics" doc comment: add/remove are now hot-applied via rebuild-and-swap; the cache wall is documented as applying only to *in-place edits / updates*. |

### Go front end (`cmd/autosk`, `internal/daemon`)

| File | Change |
| --- | --- |
| `internal/daemon/api/types.go` | Mirror the new `reloaded*` result fields + `ExtensionReloadResult` + `RegistryChangedParams`. |
| `internal/daemon/rpcclient/writes.go` | Add `ReloadExtensions(ctx, …)`; update the `Install`/`Remove` result structs. |
| `cmd/autosk/ext.go` | `add`/`remove`: replace the restart hint with an "applied to N open project(s)" line when `reloaded`; keep a soft hint only if nothing was reloaded (e.g. project not open). Add `newExtReloadCmd()` (`autosk ext reload`) → `extension.reload`, registered in `newExtCmd()`. Keep `ext update`'s restart hint (still restart-only). |

### Docs & changelog

- `docs/extensions.md` — new "Hot-reload" section: add/remove apply live;
  running sessions keep their code; update/in-place edits still need a restart;
  `autosk ext reload`.
- `docs/cli.md` — document `autosk ext reload`; note `add`/`remove` apply
  without a restart.
- `docs/daemon.md` — note the registry is now rebuildable + the
  `registry-changed` notification.
- `CHANGELOG.md` (`## [Unreleased]`):
  - Added: `autosk ext reload` — reload extensions without restarting the daemon.
  - Changed: `ext add`/`ext remove` now apply live to open projects (no restart).

---

## Edge cases & risks

1. **Factories of already-loaded extensions re-run on every reload.**
   `loadProjectRegistry` re-imports *all* entries; Bun returns the cached module
   for unchanged ones, so their `default` factory runs again into the fresh
   registry. This is consistent with today's behaviour (factories already re-run
   on every project open) and the contract that factories are pure registration.
   *Risk:* a factory with side effects (I/O, starting something) repeats them.
   *Mitigation:* document the purity expectation; Phase 2 can import only the
   delta of new entries.

2. **`ctx.workflows.list/get` in a running session is slightly stale.**
   `buildWorkflowsApi(registry, …)` (`daemon/core/src/engine/context.ts`)
   closes over the registry instance captured at run start, so a long-running
   session's `ctx.workflows.list()` won't show a freshly-added workflow until its
   next step. *Decision:* accept it — a stable per-run view is arguably correct.
   Phase 2 could pass a live holder if desired.

3. **Removing a workflow under a live session.** Handled by §3: skip-live at
   reload + park-on-missing in `dispatch` after the session settles. The session
   itself always finishes on its captured workflow object.

4. **Concurrency / locking.** Serialize a rebuild against first-open and other
   rebuilds of the same root by running it under the existing
   `openLocks.run(root, …)` (KeyedMutex). The preceding `npm install` already
   runs under `installLocks` (per packages dir); install completes (settings
   written) *before* the rebuild reads disk, so ordering is correct.

5. **Global reload cost.** A global add/remove rebuilds every open project's
   registry (re-imports for each). Typically a handful of projects and mostly
   cached imports — cheap. The single new path import dominates.

6. **Bun module cache grows monotonically.** Old modules from a removed/replaced
   extension stay resident for the process lifetime (no unload API). Acceptable;
   note it. A long-lived daemon churning many extensions slowly grows RSS — a
   restart reclaims it.

7. **Diagnostics surfacing.** A reload that introduces a load error (bad new
   extension, name collision) records diagnostics on the new registry; the
   reload result + `project.diagnostics` expose them, and the daemon log warns
   (mirror the `open()` path's diagnostic logging).

---

## Why update / in-place edits are out of scope

`ext update` and editing an installed extension's code re-use the **same module
path**, which Bun will not re-import with new code in-process (confirmed:
query-string busting hangs in current Bun; no `uncachedImport`). The only honest
ways to hot-apply them are:

- **Versioned install dirs** for npm: install `pkg@2` into a *new* path (e.g.
  `packages/<name>@<version>/`) so the entry specifier is genuinely new and
  imports fresh. Each extension already carries its own
  `@autosk/sdk` / `@autosk/pi-agent` / `@autosk/sandbox` in its `node_modules`
  (verified — these helpers are structural, so duplicate module instances are
  fine), so a versioned dir resolves correctly. This is a larger change to the
  install layout + discovery and is deferred.
- A **child-process loader** that imports extensions out-of-process — but agent
  `onRun` / workflow `onTransit` are live functions that must run in-process with
  `ctx`, MCP servers, etc., so this doesn't help execution.

So `ext update` keeps its restart hint for now. Phase 2 can adopt versioned
install dirs to make update hot-applicable through the same rebuild-and-swap
machinery this plan introduces.

---

## Testing

Daemon (`bun test`, `daemon/core`):
- Reload after a simulated **add** registers the new workflow; a `new` task
  enrolled into it now dispatches — all without re-opening the project.
- Reload after a simulated **remove** drops the workflow from
  `listWorkflows()`; a non-live `work` task on it is parked to `human`.
- **Running session survives a remove**: a session dispatched on workflow W keeps
  running (captured object) after W is removed; the task parks only after the
  session settles (via the park-on-missing dispatch path).
- **Atomic swap**: a dispatch interleaved with a reload never sees a half-built
  registry (old-or-new, never partial).
- **Global reload** updates every loaded project; **project reload** updates only
  the targeted root.
- `validateInFlightTasks` skips tasks flagged live by `isLive`.
- Conformance: `registeredMethods()` includes `extension.reload`;
  `RPC_NOTIFICATIONS` includes `registry-changed` (no-drift test).

Go (`go test`, verb tests, auto-spawned daemon):
- `autosk ext add` (local-path fixture) then immediately enroll/list shows the
  new workflow without a restart; output no longer prints the restart hint.
- `autosk ext reload` returns the summary and refreshes the registry.

---

## Rollout / sequencing

1. SDK proto additions (`extension.reload`, `registry-changed`, result fields) +
   Go mirrors + conformance tests green.
2. Engine `setProjectRegistry` + park-on-missing `dispatch`.
3. `hazard.ts` skip-live predicate.
4. `ProjectManager.rebuildRegistry` + `Daemon.applyExtensionReload` + handler
   wiring (auto trigger).
5. `extension.reload` handler + `autosk ext reload` CLI + drop restart hint.
6. Docs + CHANGELOG.

Each step is independently testable; the auto-trigger (step 4) is the one that
delivers the headline "`ext add` with no restart".
