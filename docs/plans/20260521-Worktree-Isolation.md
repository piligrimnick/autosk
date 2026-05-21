# Worktree isolation per workflow — plan

**Status:** plan (not yet started).
**Date:** 2026-05-21.
**Owners:** autosk core.
**Related code:**
`internal/workflow/{parse,validate,store}.go`,
`internal/migrations/`,
`internal/daemon/executor/executor.go`,
`internal/daemon/projectmgr/projectmgr.go`,
`internal/daemon/pi/runner.go`,
`internal/daemon/agentnode/runner.go`,
`cmd/autosk/{create,enroll,done,cancel,reopen,show,resume,update}.go`,
`internal/render/`.

---

## 1. Motivation

Two workflow tasks running concurrently on the same project root race on
the filesystem (`docs/daemon.md` § Security model documents this hole
explicitly). The global `--workers` knob caps concurrency at the daemon
level but does nothing to keep an agent on task A from clobbering a file
the agent on task B is mid-edit on.

We want to be able to enrol a task into a workflow such that **the
entire workflow run for that task happens in its own git worktree**, on
its own branch, independent of every other concurrent task — while the
project's `.autosk/db` stays a single source of truth.

Concretely, after this lands:

1. A workflow author marks a workflow as `isolation: "worktree"` in
   the JSON definition.
2. `autosk create … --workflow feature-dev` (or `enroll`) on a task
   targeting that workflow allocates `~/.autosk/worktrees/<slug>/<task-id>`
   with branch `autosk/<task-id>` before the task ever enters the
   workflow.
3. Every step run for that task is spawned with `cwd =` that worktree.
   The agent's `autosk step next …` keeps writing to the **canonical**
   `.autosk/db` (the daemon threads `AUTOSK_DB` into the child).
4. On `done`/`cancelled` (whether via `autosk step next`, `autosk
   done|cancel`, or the executor's failure-park path), the worktree
   directory is removed; the branch is preserved for human review /
   merge.
5. Two tasks of the same workflow can run literally at the same time
   — different `cwd`, different branch, zero collision.

Out of scope is anything broader than "git worktree as the isolation
primitive": container/VM isolation, per-step worktrees, push/pull
synchronisation between step worktrees, and shared-disk safety on
network filesystems. See §11.

---

## 2. Locked decisions (ask_user 2026-05-21)

Do not relitigate these — they are the contract for the rest of the plan.

| Topic | Choice |
|---|---|
| Granularity | **Per-task** worktree. One worktree shared by every step run of the task; reused on every transition; removed only on terminal status. |
| Opt-in | **Workflow-level**, via a new field `isolation` in the workflow JSON. Default `"none"` (= current behaviour). The only other accepted value in this iteration is `"worktree"`. |
| Cleanup | **Auto-remove** worktree directory on `done`/`cancelled`. **Branch is preserved.** No grace period, no `--keep-worktree` opt-out in v0.3. |
| Where worktrees live | `~/.autosk/worktrees/<slug>/<task-id>`, where `<slug> = filepath.Base(canonicalProjectRoot) + "-" + 8hex(sha256(canonicalProjectRoot))`. The hex suffix disambiguates same-name project roots (e.g. `~/work/foo` vs `~/oss/foo`). |
| Branch name | Always `autosk/<task-id>`. Not user-configurable. |
| `create` flags | `autosk create` accepts **no** worktree-related flags. When `--workflow` targets an isolated workflow, the worktree is allocated from `HEAD` of the project root. |
| `enroll` flags | New flag `--base-ref <ref>` (optional). Applies only when the target workflow has `isolation: "worktree"` and the branch `autosk/<task-id>` does not already exist. Default = `HEAD`. |
| Persisted state | **None new on `tasks`.** The worktree path and branch name are derived deterministically from `(canonicalProjectRoot, task.id)`. The only new persistent bit is the workflow's `isolation` value (workflows JSON → DB column). |
| Reopen semantics | `autosk reopen` does **not** touch git. Branch survives terminal status. A subsequent `enroll` into the same isolated workflow re-uses the existing branch (no `-b`, base-ref flag is ignored with a warning); a fresh worktree directory is created on the existing branch. |
| Non-git / git error fallback | **Hard error** at `create`/`enroll` time. No silent fallback to `projectRoot`, no auto `git init`. Tasks already in flight when git breaks fail their next run with a clear error and park to `human_feedback`. |
| Synthetic `single:<agent>` workflows | Always `isolation: "none"`. EnsureSingle never produces an isolated workflow; the `--agent` shorthand is unaffected. |

---

## 3. Design at a glance

```
┌────────────────────────────────────────────────────────────────────┐
│                          autosk CLI                                │
│                                                                    │
│  workflow create --file feature-dev.json                           │
│    └─ parse "isolation":"worktree" → workflows.isolation column   │
│                                                                    │
│  create / enroll …                                                 │
│    1. resolveWorkflowEntry(...) returns wf, st, isolated(bool)     │
│    2. if isolated:                                                 │
│       worktree.Ensure(projectRoot, taskID, baseRef)                │
│         → shells `git worktree add <path> -b autosk/<id> <ref>`    │
│         (no -b when branch already exists; base-ref ignored)       │
│    3. workflow.EnterStep(...)  (existing)                          │
│    4. commit                                                       │
│                                                                    │
│  done / cancel / reopen                                            │
│    └─ on terminal status: worktree.OnTerminal(projectRoot, taskID) │
│       → shells `git worktree remove --force <path>`                │
│         (branch left intact); idempotent                           │
└────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌────────────────────────────────────────────────────────────────────┐
│                          autosk daemon                             │
│                                                                    │
│  executor.Run(jobID)                                               │
│    ↓                                                               │
│  resolve wf for task                                               │
│    ↓                                                               │
│  if wf.Isolation == "worktree":                                    │
│      cwd      = worktree.PathFor(projectRoot, taskID)              │
│      env     += AUTOSK_DB=<canonicalDBPath>                        │
│      pre-check: stat(cwd) succeeds, .git inside it links back      │
│      to projectRoot's gitdir; else fail run with                   │
│      error="worktree_missing", park task → human_feedback.         │
│  else:                                                             │
│      cwd = projectRoot   (current behaviour)                       │
│  spawn pi / node bootstrapper with cwd + env                       │
│                                                                    │
│  on terminal transition (done/cancelled emitted via step_signals): │
│      after advanceTask succeeds → worktree.OnTerminal(...)         │
│                                                                    │
│  on failed run (parkTaskOnFailure): worktree is NOT removed —     │
│  human will inspect the worktree, then either resume or            │
│  done/cancel manually (cleanup happens at the manual close).       │
└────────────────────────────────────────────────────────────────────┘
```

The deterministic-derivation rule is load-bearing: every code path that
needs the worktree path computes it from `(canonicalProjectRoot,
taskID)` via one helper. No row in any table stores a copy. This means
moving the project directory after the fact is unsupported (same as for
`.autosk/db` itself).

---

## 4. Schema changes (migration `005_workflow_isolation.sql`)

Single column on `workflows`:

```sql
-- 005_workflow_isolation.sql
--
-- Per-workflow isolation mode. 'none' (the default) preserves current
-- behaviour: steps spawn with cwd=projectRoot. 'worktree' means the
-- CLI allocates a git worktree per task at enroll time and the daemon
-- spawns step runs inside that worktree.
--
-- The set is intentionally small. Future modes (e.g. 'container') will
-- ALTER the CHECK, not introduce a parallel column.

ALTER TABLE workflows ADD COLUMN isolation TEXT NOT NULL DEFAULT 'none'
       CHECK (isolation IN ('none','worktree'));
```

Why not also add `tasks.worktree_path` / `tasks.git_branch`?

- Both values are pure functions of `(canonicalProjectRoot, task.id)`.
  Persisting them duplicates state and creates a divergence risk between
  the row and the filesystem (the row says path X exists, the filesystem
  says X was `rm -rf`-ed, now what?). Today we have no row to lie about
  — `stat`-ing the derived path is the single source of truth.
- Synthetic single-agent workflows already pin `isolation='none'` via
  `EnsureSingle`; no per-task knob needed there.
- A future task-level override (`tasks.isolation_override`) can be
  layered on without touching this migration.

No backfill is needed — every existing workflow row gets `'none'`.

---

## 5. Workflow JSON shape

`internal/workflow/parse.go`'s `Definition` gains:

```go
type Definition struct {
    Name        string
    Description string
    FirstStep   string
    Isolation   IsolationMode // NEW: "none" | "worktree"; default "none"
    Steps       map[string]StepDef
    StepNames   []string
}

type IsolationMode string

const (
    IsolationNone     IsolationMode = "none"
    IsolationWorktree IsolationMode = "worktree"
)
```

JSON:

```jsonc
{
  "name": "feature-dev",
  "description": "...",
  "first_step": "dev",
  "isolation": "worktree",         // NEW; optional, default "none"
  "steps": { "dev": { ... }, "review": { ... }, "validator": { ... } }
}
```

Validation (extends `internal/workflow/validate.go`):

- `isolation` is optional. Absent / empty → `none`.
- Allowed values are exactly `"none"` and `"worktree"`. Any other
  string fails parse with `unknown isolation mode: %q (want none|worktree)`.
- `isolation: "worktree"` on a workflow whose `name` starts with the
  reserved `single:` prefix is rejected (synthetic workflows must
  remain `none`). This is checked in `EnsureSingle` as a defensive
  assertion as well — a programmer who tries to flip the column on a
  `single:*` row is caught loudly.

`workflow.Store.Create` writes the column; `GetByID`/`GetByName`/`List`
read it. `workflow show` surfaces it in both the text and `--json`
forms. The `--json` shape gains an `"isolation"` field; text output
prepends a `Isolation: worktree` line under the description when the
mode is non-default.

---

## 6. New package `internal/worktree`

Encapsulates every git-and-filesystem interaction for the feature.
Pure shell-out to `git worktree`; no cgit, no go-git library — we
already shell `pi`, one more child process is the same shape.

```go
// internal/worktree/worktree.go
package worktree

import (
    "context"
    "errors"
)

// Manager owns the deterministic mapping (projectRoot, taskID) → on-disk
// state. The implementation is goroutine-safe; the daemon and the CLI
// share a single instance per process.
type Manager interface {
    // PathFor returns the absolute path of the worktree directory for
    // (projectRoot, taskID). Pure function; does not touch disk.
    PathFor(projectRoot, taskID string) (path string, err error)

    // Branch returns the canonical branch name for the task. Pure.
    Branch(taskID string) string

    // Ensure allocates the worktree on disk if it doesn't already exist.
    //
    //   - If the branch `autosk/<taskID>` does NOT exist:
    //       git worktree add <path> -b autosk/<taskID> [baseRef]
    //     baseRef = "" means HEAD.
    //
    //   - If the branch exists (typical for reopen+enroll):
    //       git worktree add <path> autosk/<taskID>
    //     baseRef is ignored; if non-empty, the returned Result has
    //     BaseRefIgnored=true so the CLI can warn the user.
    //
    //   - If the worktree directory ALSO exists (and `git worktree
    //     list` knows about it), the call is a no-op; Result.Existing=true.
    //
    // Errors:
    //   - ErrNotGitRepo:   projectRoot is not inside a git repo.
    //   - ErrGitMissing:   `git` binary not on PATH.
    //   - ErrPathOccupied: the target directory exists but is not a
    //                      registered worktree.
    Ensure(ctx context.Context, projectRoot, taskID, baseRef string) (Result, error)

    // OnTerminal removes the worktree directory for a task. The branch
    // is left alone. Idempotent: missing directory + unknown-to-git
    // path both return (Result{Existed:false}, nil).
    OnTerminal(ctx context.Context, projectRoot, taskID string) (Result, error)

    // Verify is the daemon's pre-flight check before spawning a step.
    // It stats the path, checks `.git` resolves to the project gitdir,
    // and returns ErrWorktreeMissing / ErrWorktreeStranded on
    // mismatch. The executor maps the error into the run's failure
    // reason.
    Verify(ctx context.Context, projectRoot, taskID string) error
}

type Result struct {
    Path           string
    Branch         string
    Existing       bool // worktree was already there
    BaseRefIgnored bool // caller passed baseRef but branch already existed
    Existed        bool // for OnTerminal: a path was actually removed
}

var (
    ErrNotGitRepo        = errors.New("worktree: project root is not a git repo")
    ErrGitMissing        = errors.New("worktree: git binary not found on PATH")
    ErrPathOccupied      = errors.New("worktree: target path exists and is not a registered worktree")
    ErrWorktreeMissing   = errors.New("worktree: directory missing on disk")
    ErrWorktreeStranded  = errors.New("worktree: .git does not point at the project's gitdir")
)
```

### 6.1 Path derivation

```go
func slugFor(canonicalProjectRoot string) string {
    base := filepath.Base(canonicalProjectRoot)
    sum  := sha256.Sum256([]byte(canonicalProjectRoot))
    return base + "-" + hex.EncodeToString(sum[:4])  // 8 hex chars
}

func (m *manager) PathFor(projectRoot, taskID string) (string, error) {
    canon, err := filepath.EvalSymlinks(projectRoot)
    if err != nil { return "", err }
    home, err := os.UserHomeDir()
    if err != nil { return "", err }
    return filepath.Join(home, ".autosk", "worktrees", slugFor(canon), taskID), nil
}
```

The slug is computed against the canonical (symlink-resolved) root, so
the projectmgr's canonicalisation and worktree's canonicalisation agree.

### 6.2 Shell-out shape

Every command runs with the cwd set to the **canonical project root**
(not the worktree); that's how `git worktree …` knows which repo it's
operating on. Stdout/stderr are captured and surfaced verbatim in the
returned error.

| Operation | Command |
|---|---|
| Detect repo | `git -C <projectRoot> rev-parse --git-dir` |
| Branch exists? | `git -C <projectRoot> show-ref --verify --quiet refs/heads/autosk/<id>` |
| Worktree exists? | `git -C <projectRoot> worktree list --porcelain` parsed for `worktree <path>` lines |
| Create (new branch) | `git -C <projectRoot> worktree add <path> -b autosk/<id> <baseRef>` |
| Create (existing branch) | `git -C <projectRoot> worktree add <path> autosk/<id>` |
| Remove | `git -C <projectRoot> worktree remove --force <path>` |

`--force` on remove is necessary because the agent will have produced
uncommitted state inside the worktree by the time a step transitions to
`done`. The branch tip captures the work that was committed; anything
uncommitted is intentionally discarded (the contract is "agent commits
its work before signalling `done`"). This is documented in
`docs/workflows.md` § Worktree isolation.

### 6.3 Concurrency

The manager keeps a per-task `sync.Mutex` (keyed by canonical
projectRoot + taskID) so two callers that race on `Ensure` for the same
task serialise. Different tasks proceed in parallel. Locks live in
memory only — they don't survive a daemon restart, but the underlying
git operations are themselves race-safe (git worktree add is atomic
relative to other git operations on the same repo).

---

## 7. CLI wiring

### 7.1 `autosk workflow create`

Parse + validate the new `isolation` field. The `--file` parser inlines
the value into the Definition; `Store.Create` writes the column. No new
flags.

### 7.2 `autosk create`

`cmd/autosk/create.go`'s flow gains one branch between
`resolveWorkflowEntry` and `EnterStep`:

```go
wf, entryStep, err := resolveWorkflowEntry(...)
// NEW:
if wf.Isolation == workflow.IsolationWorktree {
    if _, err := wtMgr.Ensure(ctx, projectRoot, t.ID, ""); err != nil {
        // Roll back CreateTask: delete the orphan row so the user sees
        // "task not created" instead of a half-formed row. We already
        // do equivalent rollback when EnterStep fails on an existing
        // task; the same UpdateTask(... status=cancelled, …) shortcut
        // is wrong here because the task row exists but should not.
        _ = s.DeleteTask(ctx, t.ID)
        return fmt.Errorf("create %s: allocate worktree: %w", t.ID, err)
    }
}
```

Required new method `s.DeleteTask(ctx, id) error` on `store.Store` (and
its doltlite implementation) — see §9 for the small store delta.

No new flag. If a non-worktree workflow is used, the worktree branch is
not entered.

### 7.3 `autosk enroll`

`cmd/autosk/enroll.go` gains `--base-ref <ref>`:

- Flag is only meaningful when the resolved workflow has
  `isolation: "worktree"`. Passing it on a non-isolated workflow is a
  hard error (`--base-ref requires the target workflow to be
  isolation=worktree`).
- Order of operations: `checkEnrollable` → `resolveWorkflowEntry` →
  (if isolated) `wtMgr.Ensure(ctx, projectRoot, taskID, baseRef)` →
  `EnterStep`. If `Ensure` returns `BaseRefIgnored == true`, a warning
  is printed to stderr.
- On `Ensure` failure: the task is **not** enrolled. The status stays
  `new`. The error message tells the user how to retry once they have
  fixed git state.

### 7.4 `autosk done` / `cancel`

Both call `wtMgr.OnTerminal(ctx, projectRoot, taskID)` **after** the
status update commits. Failure to remove is a warning, not a fatal
error (we don't want a stuck `git worktree remove` to mask a successful
status flip). The warning is printed to stderr and attached as an
`autosk` agent comment on the task so the human can see it via
`comment list`.

### 7.5 `autosk reopen`

No git operations. The branch survives the reopen. The worktree
directory will be re-created by the next `enroll` into an isolated
workflow.

### 7.6 `autosk show`

`render.Task` adds a worktree section when applicable:

```
Worktree: /Users/x/.autosk/worktrees/autosk-7a3b9c2e/as-bea9 (exists)
Branch:   autosk/as-bea9
```

The directory presence is computed at render time via `stat`. `--json`
gains:

```json
{
  ...
  "worktree": {
    "path": "/Users/x/.autosk/worktrees/autosk-7a3b9c2e/as-bea9",
    "branch": "autosk/as-bea9",
    "exists": true
  }
}
```

The block is omitted entirely when the task's workflow has
`isolation: "none"` (or the task has no workflow). This keeps existing
golden tests green for non-worktree paths; new goldens cover the
isolated path.

### 7.7 `autosk worktree` (new subcommand)

Minimal verbs for diagnostic / manual recovery. **None of them mutate
task rows.**

| Verb | Purpose |
|---|---|
| `autosk worktree list` | Show worktrees for every task in this project whose workflow has `isolation: "worktree"`, with task id, path, branch, status, on-disk presence. **One project only** in v0.3 (see deviation below). |
| `autosk worktree path <task-id>` | Print the derived path for a task (does not stat). |
| `autosk worktree rm <task-id>` | Force-remove the worktree directory; branch left intact. Refuses `in_workflow` tasks (a live daemon may be executing inside the dir); accepts `new`, `human_feedback`, `done`, `cancelled` so the documented `worktree_missing` recovery flow in §8.3 can run without first closing the task (see deviation below). |

These are convenience verbs — the engine handles the common cases.

### Deviations from the earlier draft (caught during review)

1. **`--all-projects` on `list` is deferred to a follow-up.** The earlier
   draft of this section spelled it `autosk worktree list
   [--all-projects]` and §10 WT4 promised "`--all-projects` parity on
   list". The shipped CLI omits the flag. The use case (one operator,
   multiple projects all running isolated workflows in parallel) only
   bites once the daemon is the primary entry point; the v0.3 CLI is
   single-process, has no daemon round-trip for `worktree list`, and
   has no reliable cross-project enumeration story without one
   (scanning `~/.autosk/worktrees/*` and reverse-mapping the slug back
   to a canonical project root is best-effort: distinct projects can
   hash to the same `<basename>-<hex8>` slug if their canonical roots
   ever collide). The verb still lists every isolated task **in the
   current project**, including closed ones whose dir survives, which
   covers the day-one diagnostic story.

2. **`rm` refuses only `in_workflow`, not every non-terminal status.** The
   earlier draft said "errors if the task is not in a terminal status".
   That broke §8.3's documented recovery flow for `worktree_missing`:
   the parked task is in `human_feedback`, not `done`/`cancelled`, so
   `autosk worktree rm <id>` errored out and the human had no way to
   clean up stranded on-disk state without first closing the task.
   The shipped verb refuses only `in_workflow` (the only status under
   which the daemon might be live inside the directory); `new` /
   `human_feedback` / `done` / `cancelled` are all accepted. See the
   updated §8.3 below for the full recovery sequence.

---

## 8. Daemon executor changes

### 8.1 `executor.Run`

After resolving `wf` (already done; just lift it out of `advanceTask`-only
scope), branch on `wf.Isolation`:

```go
cwd := e.cfg.ProjectRoot
env := os.Environ() // standard inheritance
if wf.Isolation == workflow.IsolationWorktree {
    p, perr := e.wt.PathFor(e.cfg.ProjectRoot, tk.ID)
    if perr != nil { return e.failTerminal(bg, jobID, nil, fmt.Errorf("worktree path: %w", perr)) }
    if verr := e.wt.Verify(ctx, e.cfg.ProjectRoot, tk.ID); verr != nil {
        return e.failTerminal(bg, jobID, nil, fmt.Errorf("worktree_missing: %w", verr))
    }
    cwd = p
    env = append(env, "AUTOSK_DB="+e.projectDBPath())
}
```

`pi.Opts` and `agentnode.Opts` gain an `Env []string` field (`pi.Opts`
already has one — confirmed in `internal/daemon/pi/runner.go`;
`agentnode.Opts` does not yet — small additive change). Both factories
pass it through to `exec.Cmd.Env`.

`pi.Opts.Cwd` and `agentnode.Opts.Cwd` already exist; we just point
them at the worktree.

`AUTOSK_DB` must be set explicitly even though `pi`/the runtime inherit
the daemon's environment, because the daemon itself ignores `AUTOSK_DB`
(per `docs/daemon.md` § "The daemon ignores its own AUTOSK_DB") and may
not have it set. The exec'd `pi`/Node child does need it: every
`autosk` CLI call inside the worktree would otherwise walk up from the
worktree's cwd, find no `.autosk/db`, and fail (the worktree directory
is outside the project tree).

`projectDBPath()` is a tiny new method on `Executor` that returns the
canonical absolute path to `<projectRoot>/.autosk/db`. The value is
already known to `projectmgr.Project.DBPath`; we wire it through
`executor.Config.DBPath` (currently unset; threaded in
`projectmgr.openProject` next to the existing `execCfg.ProjectRoot = root`
line).

### 8.2 `advanceTask` / terminal transitions

When the agent signals `done` or `cancelled`, the existing
`advanceTask` updates the task. Immediately after the `daemon_runs`
row is marked `done` (in `Executor.Run` after `advanceTask` succeeds),
the executor calls `e.wt.OnTerminal(...)` if the workflow was
isolated. Failures are logged + recorded as an agent comment (same
shape as the CLI's `done`/`cancel` path); the run is **not** marked
failed by a cleanup error — the task is already terminally closed,
the agent did its job.

For `human_feedback` transitions the worktree is preserved (the human
is going to inspect it and may resume the workflow). For sibling-step
transitions ditto (obviously).

### 8.3 Failure modes inside a worktree

| Symptom | What the executor does |
|---|---|
| `Verify` fails: directory missing | **Auto-recovered** (deviation from the original plan — see note below). Executor calls `Ensure("")` on the existing branch and proceeds; logs `executor: re-allocated missing worktree`. Only fails (`error="worktree_missing: re-allocate failed: ..."`, task parked → `human_feedback`) when `Ensure` itself errors (e.g. git binary missing, slot occupied by non-worktree). |
| `Verify` fails: `.git` stranded (e.g. project moved) | Run fails with `error="worktree_stranded"`; task parked → `human_feedback`. Recovery: see below. |
| Worktree exists but agent crashed mid-edit | Standard kickback flow applies; no special handling. The worktree retains the partial state for the next step run to pick up. |
| Cleanup on terminal fails | Warning + agent comment; run still marked `done`. |

**Deviation (2026-05-21 follow-up to WT5).** The original plan parked
the task on a missing-directory `Verify` fail and required a four-step
manual recovery (`worktree rm` → `cancel` → `reopen` → `enroll`). In
practice this hit on every legitimate reopen after a cleanup-on-done
and felt strictly worse than just re-`Ensure`-ing on the branch (which
is the load-bearing piece and is preserved on terminal cleanup by
design). The executor now auto-recovers the missing-dir case and only
the stranded case (project moved / git state broken / slot occupied)
still parks. The dedicated `autosk worktree create <id>` verb
discussed in §13 is therefore not needed for this case.

**Recovery from `worktree_stranded`** (and from the residual
`worktree_missing: re-allocate failed: ...` case when auto-recovery
itself bombs) is either "give up" (one command) or "re-allocate and
retry" (four commands). A plain `autosk resume` does NOT work because
it returns the task to `in_workflow` at the same step without
cleaning the stranded dir, so the next run `Verify`-fails and parks
again. The accurate recovery is:

```bash
# give up:
autosk cancel  <id>             # branch preserved; cleanup is a no-op when dir is already gone

# OR re-allocate and retry:
autosk worktree rm <id>         # optional: reaps any stranded on-disk state
autosk cancel  <id>             # human_feedback → cancelled (reopen doesn't take
                                # human_feedback as a source state)
autosk reopen  <id>             # cancelled → new; current_step_id cleared
autosk enroll  <id> --workflow <iso>   # Ensure re-allocates the worktree on the
                                       # existing branch; daemon picks the task back up
```

A dedicated `autosk worktree create <id>` verb (calls `Ensure` on a
parked task and lets `resume` pick up) would compress this to two
commands but is deferred to a follow-up; see §13.

### 8.4 `parkTaskOnFailure`

Untouched. We **do not** auto-clean the worktree on a parked task —
the human needs the on-disk state to debug. Cleanup only happens at a
terminal status, and parking is `human_feedback`, not terminal.

---

## 9. Store / projectdb deltas

Small additive changes; nothing semantically deep.

1. `store.Store` interface + `doltlite` + (build-tagged stub)
   `doltserver`: add

   ```go
   DeleteTask(ctx context.Context, id string) error
   ```

   Used by `create` rollback in §7.2. Today there is no public delete
   verb; we keep this internal (no CLI command exposes it directly).
   The implementation is a single `DELETE FROM tasks WHERE id=?`; FK
   cascades handle `task_deps` and `comments`. `daemon_runs` rows are
   only created post-`EnterStep` so there are none to worry about at
   the create-rollback boundary.

2. `workflow.Definition` and `workflow.Workflow` gain an `Isolation`
   field; `workflow.Store.Create` writes the column; the scanner reads
   it; `EnsureSingle` always inserts `'none'`.

3. `executor.Config` gains `DBPath string`. `projectmgr.openProject`
   sets `execCfg.DBPath = dbPath` next to the existing `ProjectRoot`
   assignment.

4. `internal/daemon/agentnode/runner.go` gains `Env []string` on
   `Opts` and passes it to `exec.Cmd.Env`. The pi runner already has
   one (`internal/daemon/pi/runner.go::Opts.Env`).

No migration is needed for any of the above beyond migration 005.

---

## 10. Phases

Sized one autosk task per phase. Strictly linear; later phases assume
earlier ones landed.

| ID | Phase | Done when |
|---|---|---|
| **WT0** | **Lock plan** | This document checked in. `(P)` items above are the contract. |
| **WT1** | **Migration 005 + workflow schema** | `005_workflow_isolation.sql`, `workflow.Definition.Isolation`, parser + validator, `Store.Create/Get/List` round-trip preserves it. `EnsureSingle` writes `'none'`. Unit + conformance tests cover the column and the parser. |
| **WT2** | **`internal/worktree` package** | Manager with shell-out git wrappers, deterministic path derivation, sentinel errors, per-task mutex. Unit tests against a real `git init`-ed temp repo (Makefile already gates on a working git environment via `make doctor`-equivalent assumptions). No autosk wiring yet. |
| **WT3** | **CLI wiring (create / enroll / done / cancel / reopen / show)** | `create` allocates on isolated workflow; `enroll --base-ref` lands; `done`/`cancel` cleanup; `show` text+JSON surfaces the worktree block; `--no-worktree`-style escape hatches deliberately absent. Golden + e2e tests using a temp-repo fixture. |
| **WT4** | **`autosk worktree` subcommand** | `list` / `path` / `rm` verbs. Read-only against tasks; mutating `rm` rejects only `in_workflow` (so the §8.3 recovery flow works on parked tasks too). `--all-projects` parity on `list` is deferred to a follow-up — see §7.7 deviations. |
| **WT5** | **Daemon executor** | `executor.Config.DBPath` threading; cwd + AUTOSK_DB injection; `Verify` pre-flight; `OnTerminal` post-`advanceTask` hook; `agentnode.Opts.Env`; `pi.Opts.Env` populated. End-to-end e2e test with a fake pi that emits a `done` signal and asserts the worktree directory is gone afterward. |
| **WT6** | **Concurrency soak** | Spawn 2× tasks of the same isolated workflow against the same project, with a fake-pi factory that fights over the same relative paths. Assert no cross-task interference; assert the daemon's worker pool serialises within a project but not across worktrees. |
| **WT7** | **Docs + AGENTS.md** | `docs/workflows.md` gains an "Isolation" section (linking here); `docs/daemon.md` loses the "no worktree isolation in this iteration" caveat and gains a `--gc-interval`-style note about derived paths; `README.md` roadmap line "worktree isolation per run (re-introduces tasks.git_branch)" is replaced with the v0.3 done bullet. Example workflow `docs/notes/workflow-example.json` gets a sibling `workflow-isolated-example.json`. |

Each phase ends with a runnable artifact and lands as one PR.

---

## 11. Acceptance scenario (WT5 / WT6)

```bash
# 0. Bootstrap.
autosk init                                    # in a git repo
autosk agent install @autosk/developer
autosk agent install @autosk/code-reviewer

# 1. Define an isolated workflow.
cat > .autosk/feature-dev.json <<'EOF'
{
  "name": "feature-dev",
  "description": "Isolated dev+review",
  "first_step":  "dev",
  "isolation":   "worktree",
  "steps": {
    "dev":    { "agent": { "name": "@autosk/developer" },
                "next_steps": [{ "step": "review", "prompt_rule": "..." }] },
    "review": { "agent": { "name": "@autosk/code-reviewer" },
                "next_steps": [{ "task_status": "done",  "prompt_rule": "..." },
                               { "step":        "dev",   "prompt_rule": "..." }] }
  }
}
EOF
autosk workflow create --file .autosk/feature-dev.json

# 2. Two tasks, two worktrees.
a=$(autosk create "Tidy README"        --workflow feature-dev --json | jq -r .id)
b=$(autosk enroll  as-XYZ              --workflow feature-dev --base-ref main)

autosk worktree list
# task        path                                                  branch              dir
# as-bea9     ~/.autosk/worktrees/myproj-7a3b9c2e/as-bea9            autosk/as-bea9      present
# as-XYZ      ~/.autosk/worktrees/myproj-7a3b9c2e/as-XYZ             autosk/as-XYZ       present

# 3. Run the daemon. Both tasks progress concurrently.
autosk daemon serve --workers 2 &

# 4. Each step run for $a happens in its worktree; the same for $b.
#    Their file edits never collide.

# 5. Eventually the reviewer for $a signals `done`. The daemon:
#    - advances task → done
#    - calls `git worktree remove --force <path>`
autosk show "$a"
# Status: done
# (no Worktree: block — directory gone, branch still there)

git branch --list 'autosk/*'
# * autosk/as-bea9
#   autosk/as-XYZ

# 6. Human merges autosk/as-bea9 into main at their leisure.
```

A second scenario for `reopen`:

```bash
autosk reopen "$a"          # done → new; branch survives, no on-disk worktree
autosk enroll  "$a" --workflow feature-dev
# branch autosk/as-bea9 already exists → reused, --base-ref would warn
# new worktree directory allocated, daemon picks the task back up
```

---

## 12. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Stale worktrees clutter `~/.autosk/worktrees` (cancellation paths, daemon crashes mid-cleanup) | `autosk worktree list` makes them discoverable; `autosk worktree rm` reaps them; future iteration may add a `--prune` flag that cross-references `daemon_runs` to find orphans. Not in scope for v0.3. |
| User moves the project directory after creating isolated tasks | Derived paths now point to nonsense. `Verify` fails with `worktree_stranded`; recovery is "move it back, or cancel the task and re-create it". Documented in `docs/workflows.md`. |
| `git worktree remove --force` discards uncommitted edits | Documented contract: the agent must commit before signalling `done`. The kickback messages for `done` transitions mention this. Adding a guard ("refuse `done` if status not clean") is explicitly out of scope — too easy to game with `.gitignore`, and the protocol stays simple. |
| Branch namespace pollution after many tasks | Branches `autosk/<task-id>` are scoped by the random task id; collisions are vanishingly unlikely. `autosk worktree list` can show them; future `autosk worktree prune-branches` is a follow-up. |
| Network-filesystem races on `~/.autosk/worktrees` | Out of scope; documented as unsupported. Same constraint as `.autosk/db` itself. |
| `git worktree add` is slow on huge repos | Single-shot cost at `create`/`enroll`; no impact on step runtime. Same trade-off `git worktree` ships with upstream. |

---

## 13. Out of scope

- Per-step worktrees, container/VM isolation, or any isolation other
  than git worktree at the task level.
- `autosk worktree prune` / GC of orphaned branches or directories.
- A `worktree create <id>` (or `resume --reallocate`) verb that calls
  `Ensure` on a parked task without going through `cancel` → `reopen`
  → `enroll`. Would compress the `worktree_missing` recovery from
  four commands to two; see §8.3. Deferred to a follow-up.
- `autosk worktree list --all-projects`. Deferred per §7.7
  deviation note (no cross-project enumeration story in the
  single-process CLI).
- Auto-merge / auto-rebase of the task's branch back into the base.
- Push/pull, remote sync, multi-machine worktree sharing.
- `tasks.isolation_override` (per-task escape hatch overriding the
  workflow's `isolation` field).
- Hooking `autosk lazy` into worktree visualisation beyond what the
  reused `autosk show` already produces. (The TUI will pick up the
  `worktree` JSON block "for free" wherever it already renders task
  detail.)
- Reflecting worktree state in the daemon's `/v1/healthz` aggregate.

These either have no design pressure today (we have one user asking
for the concurrent-workflows use case) or are obvious follow-ups once
the simple version has bedded in.

---

## 14. Cross-references

- Background motivation: `docs/daemon.md` § Security model — concurrent
  runs in the same project will race on files.
- README roadmap line being closed: `README.md` § Roadmap — "worktree
  isolation per run (re-introduces `tasks.git_branch`)" (the
  `git_branch` parenthetical is now obsolete; this plan keeps no row
  state).
- Previous deferrals: `docs/plans/20260517-Workflows-Plan.md` §§2.1,
  §7 — `tasks.git_branch` was deferred until "worktree-isolated runs
  land"; this plan is that landing, **and** it consciously chooses
  no row state instead of the column the earlier note assumed.
- Daemon UDS open items: `docs/plans/20260518-Daemon-UDS-Plan.md`
  §10 — "Worktree-per-job изоляция" is the line item closed by this
  plan (modulo the per-task vs per-job rename — see locked decisions).
