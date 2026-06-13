# @autosk/worktree

The shipped **isolation provider** for autoskd v2: per-task **git worktree**
isolation, attachable to any workflow. It ports v1's worktree behaviour
(`internal/worktree` / `crates/autosk-core/src/worktree.rs`) onto the v2
[`IsolationProvider`](../../sdk/src/workflow.ts) contract (design
`docs/plans/20260612-Bun-Daemon-Extensions.md` §3.5).

## Usage

```ts
import { worktreeIsolation } from "@autosk/worktree";

autosk.registerWorkflow({
  name: "feature-dev",
  firstStep: "dev",
  steps: { dev: { agent: "@autosk/pi-agent/dev" }, /* … */ },
  isolation: worktreeIsolation(),
});
```

Each isolated task runs in its own checkout so concurrent tasks never collide on
the working tree. The engine calls `acquire` before scheduling a session (its
returned `cwd` becomes `ctx.cwd`) and `release` on every transition.

## Behaviour

Deterministic mapping — **byte-identical to v1**, so a worktree allocated by
either stack resolves to the same place:

```text
~/.autosk/worktrees/<basename(canonRoot)>-<8hex(sha256(canonRoot))>/<task-id>
branch = autosk/<task-id>
```

- **acquire** — allocates the per-task worktree on branch `autosk/<task-id>`
  (off `HEAD`), or re-uses it when a prior step kept it. A **missing** dir is
  re-allocated on the *existing* branch (v1 "missing worktree auto-recovery").
  Returns `{ cwd, meta: { branch, projectRoot } }`.
- **release(`{terminal:true}`)** (done / cancel) — removes the worktree dir but
  **preserves** the `autosk/<task-id>` branch (so the work survives for review /
  merge).
- **release(`{terminal:false}`)** (sibling step / human-park) — keeps the dir so
  the next step re-uses the same checkout.

### Failure handling

The provider **only throws descriptive messages** — the engine wraps them
(`isolation_acquire_failed: …` on acquire, `isolation_release_failed: …` on a
happy-path release) and parks the task to `human`. It never parks or formats
those prefixes itself. Throw cases:

- **non-git root** — `not a git repository: <root>` (the project root isn't a
  git repo).
- **stranded dir** — `worktree_stranded: …` (a directory sits at the worktree
  path whose gitdir doesn't resolve to the project's — e.g. a foreign repo).
- **git missing** — `git binary not found on PATH`.

## Configuration

`worktreeIsolation(options?)`:

| Option   | Default            | Description                                                                 |
| -------- | ------------------ | --------------------------------------------------------------------------- |
| `home`   | `process.env.HOME` | Home dir the worktree tree lives under (`<home>/.autosk/worktrees/…`). Tests inject a temp home. |
| `gitBin` | `"git"`            | `git` binary to shell out to.                                               |

## Exports

- `worktreeIsolation(options?)` → `IsolationProvider`
- `branchFor(taskId)`, `pathFor(projectRoot, taskId, home?)`, `slugFor(canon)` —
  the deterministic derivation helpers (exported for tooling / tests).
- `WORKTREE_TAG` — the provider tag (`"worktree"`) rendered by `workflow.get`.
