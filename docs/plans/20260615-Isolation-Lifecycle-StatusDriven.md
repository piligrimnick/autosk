# Status-driven isolation lifecycle (acquire / release / reap) — plan (variant B)

**Status:** plan (not yet started).
**Date:** 2026-06-15.
**Owners:** autosk core.
**Predecessor:** the worktree-reap work (shipped, `[Unreleased]`): manual
`task.done`/`task.cancel` now reap a stranded worktree via an identity-based
`IsolationProvider.reap`, with the `ENVIRONMENT_DIRTY` warn-then-`--force` flow.
**Related code:**
`daemon/sdk/src/workflow.ts`,
`daemon/core/src/engine/session.ts`,
`daemon/core/src/engine/transition.ts`,
`daemon/core/src/rpc/daemon.ts` (`taskTerminal` / `reapIsolation`),
`daemon/extensions/worktree/src/index.ts`,
`daemon/core/test/{engine.isolation,rpc.behaviour,engine.featuredev}.test.ts`,
`daemon/extensions/worktree/test/worktree.test.ts`,
`daemon/core/test/extensions.registry.test.ts`,
`docs/workflows.md`, `CHANGELOG.md`.

---

## 1. Motivation

Isolation today is **scoped to a single step-session**. The engine runs each
workflow step as its own session: `SessionRuntime.run()` calls
`isolation.acquire()` at the start, and `commitTransit()` calls
`isolation.release(handle, { terminal })` on **every** transition — including
step → step. For the shipped worktree provider this is harmless (a non-terminal
`release` is a no-op; the next step's `acquire` re-uses the dir), but the
per-step granularity is the wrong abstraction for **stateful** providers:

- A future `dockerIsolation()` would `docker stop` + `docker start` (or
  `run`) the container on **every** step boundary — pointless churn for a
  multi-step workflow that should keep one container hot across its steps.
- The `terminal` flag and the `{ force }` we bolted onto `release` blur two
  genuinely different operations: *quiescing* a reusable environment vs.
  *destroying* a durable one.

We want isolation scoped to the **active run** (the contiguous time a task
spends in `work`), driven by the task's status machine rather than by session
boundaries — the lifecycle a human reasons about:

> Create/resume the environment when a task **enters** the workflow; keep it
> across step→step; quiesce it (but keep it resumable) when the task **leaves**
> into `human`; destroy it for good only on `done`/`cancel`.

This generalises cleanly to containers/VMs and matches how `sandcastle`
structures the same concern (durable git artifact addressed by identity;
ephemeral sandbox addressed by a live handle + `close()`).

---

## 2. The environment state machine

Three states + three transitions; the **task status transition is the trigger**.

```
 ABSENT ──acquire(create)──▶ RUNNING ──release(stop)──▶ DORMANT
                               ▲   │                        │
                               │   └──acquire(reuse)        │
                               │                            │
                               └────acquire(resume/start)───┘
                            (any) ──reap(force)──▶ GONE
```

| Trigger | Task-status transition | Provider call | worktree | docker (future) |
|---|---|---|---|---|
| **enter `work`** | `new→work` (enroll), `human→work` (resume) | `acquire(identity)` | create or re-use dir on `autosk/<id>` | `docker run` (absent) / `docker start` (dormant) / reuse (running) |
| **step → step** | within `work` | — (nothing) | — | — (container stays hot) |
| **leave to park** | `work→human` (incl. failure/abort park) | `release(handle)` | no-op | `docker stop` (keep container) |
| **leave to terminal** | `work→done`, `work→cancel` | `release(handle)` **then** `reap(identity,{force})` | release no-op; reap removes dir (branch kept) | `docker stop` then `docker rm -f` + remove dir |
| **manual terminal** | `done`/`cancel` from `human` (no live session) | `reap(identity,{force})` **only** | reap removes dir | `docker rm -f` (by deterministic name) + remove dir |

`acquire` therefore means **ensure-ready** (create | start | reuse) and MUST be
idempotent and recovery-safe. `release` means **quiesce** (stop, stay
resumable). `reap` means **destroy** durable artifacts (force-gated on a dirty
environment). The manual-terminal row is exactly what the predecessor work
already implements in `daemon/core/src/rpc/daemon.ts` (`reapIsolation`) — it
needs no change.

---

## 3. Locked decisions

Do not relitigate these; they are the contract for the rest of the plan.

| Topic | Choice | Rationale |
|---|---|---|
| Trigger model | status-driven (table §2), not session-driven | matches the active-run lifecycle |
| `release` triggers | only on leaving `work` (`human`/`done`/`cancel`); **never** step→step | no per-step churn |
| `reap` triggers | only on terminal (`done`/`cancel`) | destroy once, at the end |
| `release` signature | **drops `terminal` and `force`** → `release(handle)` | destroy moved to `reap`; quiesce needs neither |
| `acquire` cadence | **variant B**: stays per-step but idempotent (re-use/resume) | minimal change; engine stays stateless |
| `release` / `reap` | both **optional** on the provider; `acquire` mandatory | worktree omits `release`; container omits nothing |
| Dirty policy | unchanged: engine reaps with `force:true`; manual reaps with the `--force` flag, else `ENVIRONMENT_DIRTY` | preserves shipped behaviour |
| Go / CLI / TUI / GUI / wire | **untouched** | none of them see `acquire`/`release`/`reap`; they see `task.done`/`cancel` + `ENVIRONMENT_DIRTY` |

### 3.1 Why variant B (per-step idempotent `acquire`) over variant A

Variant A would hoist the handle to a per-task slot so `acquire` fires literally
once per active run. It honours "acquire only on enter" to the letter but adds
**per-task in-memory isolation state** plus its crash-recovery — against the
"engine is stateless, position lives in files" invariant.

Variant B keeps `acquire` per-step but **idempotent**: the first step after
entering `work` creates/starts the env; later steps re-use it; after a park the
next step starts the dormant env. Because we no longer `release` between steps,
nothing is torn down mid-run, so per-step `acquire` is a cheap "ensure-ready"
check (`stat`+`git verify` for worktree; `docker inspect` for a running
container) — never the expensive create/destroy pair. It realises the §2 trigger
semantics **behaviourally** while leaving the engine stateless and making crash
recovery a no-op (the next dispatch simply re-acquires the surviving env).

The only observable difference from variant A is "`acquire` fires once vs.
once-per-step"; on environment **state** the two are identical. If a provider
ever needs a true once-per-run hook with side effects that must not repeat, that
is a follow-up (variant A), out of scope here.

---

## 4. SDK contract change (`daemon/sdk/src/workflow.ts`)

```ts
export interface IsolationProvider {
  /** `"worktree"` | `"none"` | future: `"docker"`, … */
  tag: string;

  /**
   * Ensure the environment for `(projectRoot, taskId)` exists AND is ready to
   * run, returning the handle the session runs in. MUST be idempotent and
   * recovery-safe: create when ABSENT, resume when DORMANT, re-use when RUNNING.
   * Called on entering `work` (enroll / resume) and re-entered per step.
   */
  acquire(ctx: { projectRoot: string; taskId: string }): Promise<IsolationHandle>;

  /**
   * Quiesce a LIVE environment when the task LEAVES `work` (park or terminal):
   * stop it but keep it cheaply resumable by a later `acquire`. NO destruction
   * happens here (that is `reap`), so no `terminal`/`force`. Optional — a
   * provider with nothing to stop (e.g. worktree) omits it.
   */
  release?(handle: IsolationHandle): Promise<void>;

  /**
   * Destroy the durable artifacts for `(projectRoot, taskId)` on a TERMINAL
   * transition (`done`/`cancel`), keyed by identity so it works with no live
   * handle (manual terminal after a park, or crash recovery). `force:false`
   * refuses to discard uncommitted changes and reports `{ dirty:true }`;
   * `force:true` removes regardless (branches are always preserved). Optional.
   */
  reap?(ctx: { projectRoot: string; taskId: string }, opts: { force: boolean }):
    Promise<IsolationReapResult>;
}
```

`IsolationHandle` and `IsolationReapResult` are unchanged. The only edits:
`release` loses `{ terminal; force }` and becomes optional; doc comments reframe
the three methods around the §2 state machine.

> **Note:** this revises the `release(handle, { terminal, force })` shape shipped
> in the predecessor work. That shape is `[Unreleased]`, and the only
> implementors are the shipped worktree provider plus a handful of test fakes, so
> the churn is contained.

---

## 5. Engine changes (`daemon/core/src/engine/session.ts`)

### 5.1 `run()` — unchanged

`acquire` stays where it is (in the worker, bounded by the pool), called at the
start of every step-session. It is now an idempotent "ensure-ready" call. No
edit beyond comments.

### 5.2 `commitTransit(to)` — branch the cleanup on the target

Replace the current

```ts
const terminal = "status" in to && (to.status === "done" || to.status === "cancel");
await this.releaseIsolation(terminal, { parkOnFailure: true });
```

with status-driven dispatch:

```ts
if ("step" in to) {
  // sibling step: keep the env RUNNING for the next step's acquire to reuse.
} else if (to.status === "human") {
  await this.quiesceIsolation({ parkOnFailure: true });          // release(handle)
} else {
  // done | cancel
  await this.quiesceIsolation({ parkOnFailure: true });          // release(handle)
  await this.reapIsolationTerminal({ parkOnFailure: true });     // reap(identity, {force:true})
}
```

(ordering relative to `setPosition` / transcript / session-seal is preserved
exactly as today — the isolation step just moves from one call to this branch.)

### 5.3 `finalizeFailed` / `finalizeAborted` — park ⇒ quiesce only

Both park the task to `human`. Replace their
`releaseIsolation(false, { parkOnFailure: false })` with
`this.quiesceIsolation({ parkOnFailure: false })` (no reap — the env stays
DORMANT for a resume).

### 5.4 New helpers (replace `releaseIsolation`)

```ts
// quiesce a LIVE handle exactly once
private async quiesceIsolation(opts: { parkOnFailure: boolean }): Promise<void> {
  if (!this.isolation || this.isolationQuiesced) return;
  this.isolationQuiesced = true;
  const release = this.wf.isolation?.release;
  if (!release) return;                       // provider has nothing to quiesce
  try {
    await release.call(this.wf.isolation, this.isolation);
  } catch (e) {
    this.host.logger.warn(`session ${this.id}: isolation release failed (${errMsg(e)})`);
    if (opts.parkOnFailure)
      await this.host.park(this.project, this.taskId, `isolation_release_failed: ${errMsg(e)}`).catch(() => {});
  }
}

// destroy durable artifacts by IDENTITY exactly once (engine terminal ⇒ force:true)
private async reapIsolationTerminal(opts: { parkOnFailure: boolean }): Promise<void> {
  if (this.isolationReaped) return;
  this.isolationReaped = true;
  const reap = this.wf.isolation?.reap;
  if (!reap) return;
  try {
    await reap.call(this.wf.isolation, { projectRoot: this.project.root, taskId: this.taskId }, { force: true });
  } catch (e) {
    this.host.logger.warn(`session ${this.id}: isolation reap failed (${errMsg(e)})`);
    if (opts.parkOnFailure)
      await this.host.park(this.project, this.taskId, `isolation_reap_failed: ${errMsg(e)}`).catch(() => {});
  }
}
```

`reap` is **identity-based** even on the engine path (the engine has
`projectRoot`+`taskId`), so the engine and the manual RPC path share one reap
shape. The `isolationReleased` field is replaced by the two `isolationQuiesced`
/ `isolationReaped` guards.

`detach()` (engine-stop, no terminal write) keeps doing nothing to isolation —
the env survives for crash recovery, which the next `acquire` re-uses.

---

## 6. Worktree provider changes (`daemon/extensions/worktree/src/index.ts`)

- **Remove `release` entirely.** A worktree has nothing to "stop"; keeping the
  dir across a park is the *absence* of teardown. With `release` optional, the
  engine simply skips it (`this.wf.isolation?.release?.(…)`).
- **Keep `reap` unchanged** — it already implements the dirty/force-gated
  `cleanupTerminal` (remove dir, preserve branch).
- **Delete the `worktree_dirty:` throw path** that the predecessor added inside
  `release` (it no longer exists). The dirty refusal now lives solely in `reap`
  (via `cleanupTerminal` returning `{ dirty:true }`), surfaced as
  `ENVIRONMENT_DIRTY` by the manual path and never hit on the engine path
  (`force:true`).
- `acquire` is unchanged; its existing "missing-dir re-allocation" behaviour is
  exactly the idempotent ensure-ready §2 wants.

Net: the worktree provider becomes `{ tag, acquire, reap }` — no `release`.

---

## 7. Manual terminal path — no change

`daemon/core/src/rpc/daemon.ts` `taskTerminal` → `reapIsolation` already:

- runs only when there is **no live session** (a session-free terminal),
- resolves the workflow's provider and calls `reap({projectRoot, taskId}, {force})`,
- rejects with `ENVIRONMENT_DIRTY` on `{ dirty && !removed }`.

Under §2 the env is already DORMANT at that point (the park `release` stopped
it), so reap-only is correct. Confirm with a comment; no code edit.

---

## 8. Idempotency, failure & recovery

- **`acquire` idempotent / recovery-safe** is now load-bearing (per §3.1 +
  crash recovery). Worktree already satisfies it; a docker provider must key its
  container on a deterministic name derived from `(projectRoot, taskId)`.
- **Crash mid-run:** the in-memory handle is lost but the env survives (no
  `release` ran). On restart the task is re-dispatched and the next `acquire`
  re-uses/restarts the surviving env. No persistent isolation state to recover.
- **Terminal = release then reap:** for a stateful provider this is `stop` then
  `rm` — `reap` MUST tolerate "already quiesced / already gone". For worktree
  `release` is absent, so terminal is just `reap`.
- **Park failure:** `quiesceIsolation` warns and (on the happy-path commit)
  parks with `isolation_release_failed:`, mirroring today's behaviour.

---

## 9. Test plan

### Daemon (`bun test`)

- `daemon/core/test/engine.isolation.test.ts` — rewrite the recording fake to
  log `{ acquire | release | reap }` (drop the `terminal` field on `release`;
  add `reap` with its `force`). Assert the new sequences:
  - `→done`: `acquire`, `release`, `reap{force:true}`.
  - `→cancel`: `acquire`, `release`, `reap{force:true}`.
  - `→human` (park): `acquire`, `release` (no reap).
  - **multi-step** `do→next→done` (new case): `acquire`, [no release at
    `do→next`], `acquire`, `release`, `reap{force:true}` — proves step→step
    does NOT release.
  - acquire-failure park: `acquire` (throws) ⇒ no `release`/`reap`.
- `daemon/extensions/worktree/test/worktree.test.ts` — replace
  `prov.release(…)` calls with `prov.reap(…)` (the provider no longer exposes
  `release`); drop the `release({terminal,force:false})` dirty-throw test (moved
  entirely to `reap`, which is already covered). Keep the `reap` matrix
  (clean-remove + branch kept, missing no-op, dirty-refuse, force-remove).
- `daemon/core/test/rpc.behaviour.test.ts` — the two manual-reap tests stay
  green unchanged (they already drive `reap`).
- `daemon/core/test/{engine.featuredev,extensions.registry}.test.ts` — update
  their `async release() {}` fakes to the new optional/no-arg shape (or drop
  `release`).
- `bun run typecheck` across the workspace.

### Go

No Go changes. Run `make test` only to confirm the wire/CLI/TUI/GUI are
untouched and the daemon still builds.

---

## 10. Docs & changelog

- `docs/workflows.md` — replace the `IsolationProvider` block with the §2 state
  machine + the §4 contract (`acquire` ensure-ready, `release?` quiesce-on-exit,
  `reap?` destroy-on-terminal). Note both `release`/`reap` are optional.
- `CHANGELOG.md` — **no operator-visible change** (worktree behaviour is
  identical end-to-end: dir kept across steps/park, removed on terminal, branch
  preserved; `ENVIRONMENT_DIRTY` flow unchanged). Per the changelog policy this
  is internal SDK churn and needs no entry. Optionally add a one-liner under
  `### Changed` noting the `IsolationProvider` contract reshape for extension
  authors.

---

## 11. Out of scope / follow-ups

- **Execution seam for docker.** A real `dockerIsolation()` needs the agent to
  run *inside* the container, which `IsolationHandle` (cwd-only today) cannot
  express — `pi-agent` spawns `pi` on the host at `ctx.cwd`. Adding an
  `exec`/`spawn` seam to the handle (sandcastle's `handle.exec()` model) and
  teaching `pi-agent` to route through it is a separate plan; this lifecycle
  refactor is orthogonal to it.
- **`dockerIsolation()` provider** — depends on the exec seam above.
- **Orphan sweep (`pruneStale`).** A periodic/at-acquire sweep that removes
  isolation artifacts for tasks that no longer exist (defence-in-depth against
  leaks from crashes or out-of-band task deletes), mirroring sandcastle's
  `WorktreeManager.pruneStale`. Independent hardening.
- **Variant A** (true once-per-run handle via a per-task slot) — only if a
  provider ever needs a non-idempotent once-per-run `acquire` hook.

---

## 12. Implementation checklist

1. `daemon/sdk/src/workflow.ts`: `release?(handle)` (drop `terminal`/`force`,
   make optional); reframe doc comments around §2. `acquire` mandatory, `reap?`
   unchanged.
2. `daemon/core/src/engine/session.ts`: replace `releaseIsolation` with
   `quiesceIsolation` + `reapIsolationTerminal` (guards `isolationQuiesced` /
   `isolationReaped`); branch `commitTransit` on `to` (§5.2); point
   `finalizeFailed`/`finalizeAborted` at `quiesceIsolation`.
3. `daemon/extensions/worktree/src/index.ts`: remove `release` and its
   `worktree_dirty:` throw; keep `acquire` + `reap`.
4. `daemon/core/src/rpc/daemon.ts`: confirm (comment only) the manual path is
   reap-only.
5. Tests (§9): engine.isolation rewrite + multi-step case; worktree
   `release`→`reap` call sites; fix `release` fakes in featuredev/registry tests.
6. `bun run typecheck` + `bun test` green; `make test` green (Go untouched).
7. `docs/workflows.md` update; optional `CHANGELOG.md` `### Changed` line.
