# @autosk/merge-to-current

A single-step autoskd v2 workflow that merges a task's autosk-managed branch
`autosk/<task-id>` **into the branch the operator currently has checked out**
(the CURRENT branch), running **non-isolated** in the project's working tree.

It is the v2 port of the v1 `merge-to-main` export, with one change: the
destination is no longer the mainline branch (`main`/`master`) but whatever
`HEAD` points at — there is no `main`/`master` detection and no branch switch.

## The workflow

```text
merge ──▶ done      (the branch landed cleanly on the current branch)
   └────▶ human     (rolled back; a human must take over)
```

| Step    | Kind                       | Notes                                                                |
| ------- | -------------------------- | ------------------------------------------------------------------- |
| `merge` | `piAgent` (name `merge`)   | first/only step; integrates `autosk/<task-id>` into the current branch |

`done` and `human` are terminal/park statuses the agent transitions to directly
(they are always-available targets — there is no separate `statusStep`).

- **Sandbox:** none — no agent step passes a `sandbox`, so `merge` runs on the
  host at the project root and operates directly on the operator's working tree
  (the whole point of a merge step is to touch the real branch).
- **Thinking:** the `merge` agent runs at `thinking: "high"` (matches the v1
  export's agent params).

## What the `merge` step does

The prompt ([`prompts/merge.md`](./prompts/merge.md)) drives `git` directly and
never touches the network (no `fetch`/`pull`/`push`):

1. **Verify** the task branch `autosk/<task-id>` exists.
2. **Auto-commit** any pending edits in the task's worktree onto the task branch
   first (the worktree is located via `git worktree list --porcelain`). These
   auto-commits are intentionally **preserved** even if the merge later rolls
   back.
3. **Snapshot** the current branch (`DEST = git symbolic-ref --short HEAD`) and
   **refuse** if HEAD is detached or the project root has a dirty tree. Refuse
   too if the current branch *is* the task branch.
4. **Overlap analysis** between the two diff sets; any non-trivial reconciliation
   parks the task for a human before any merge work.
5. **Merge:** fast-forward (`git merge --ff-only`) when the current branch has
   not moved since the fork; otherwise a `--no-ff` merge commit
   (`git merge --no-commit --no-ff`, with only trivial conflict resolutions
   allowed).
6. On success the merge lands on the current branch and the task transitions to
   `done`; on any failure the current branch is reset to its starting SHA and the
   task transitions to `human`.

Because the destination is the current branch, `DEST` is **both** the merge
target and the rollback target — HEAD never leaves it.

## Discovery

This package is an **npm extension** (declared via
`package.json#autosk.extensions`). It is **not** part of the first-run bootstrap
(only `feature-dev` is), so install it explicitly to make the workflow available:

```
autosk ext add npm:@autosk/merge-to-current  # add to ~/.autosk/settings.json + install
autosk enroll <task-id> --workflow merge-to-current
```

A project- or global-level extension that registers a workflow of the same name
**overrides** the npm one (first-registered wins).

## Configuration

This package exposes no per-step config knobs of its own — the agent behaviour is
configured on the inline [`@autosk/pi-agent`](../pi-agent) step value (model,
thinking, extra args, …) inside `mergeToCurrentWorkflow()`. To customise, copy
this extension into `~/.autosk/extensions/` (or your project's
`.autosk/extensions/`) and edit the `piAgent({...})` call or the prompt under
`prompts/`; your copy then overrides the npm one.

## Exports

- default export — the extension factory (registers the workflow, whose single
  step carries the inline `merge` agent).
- `mergeToCurrentWorkflow()` → `WorkflowDefinition`.
