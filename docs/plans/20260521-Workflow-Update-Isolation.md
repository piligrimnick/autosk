# `autosk workflow update` + lazy isolation surface — plan

**Status:** plan (not yet started).
**Date:** 2026-05-21.
**Owners:** autosk core.
**Predecessor:** [`20260521-Worktree-Isolation.md`](20260521-Worktree-Isolation.md).
**Related code:**
`cmd/autosk/workflow.go`,
`cmd/autosk/sql.go` (existing escape hatch we are replacing),
`internal/workflow/store.go`,
`internal/worktree/worktree.go`,
`internal/lazy/datasource/datasource.go`,
`internal/lazy/tui/{state,keys,popup,refresh,render}.go`,
`internal/render/render.go`.

---

## 1. Motivation

Worktree isolation (plan `20260521-Worktree-Isolation.md`) shipped with
one rough edge: the `isolation` field on a workflow is **immutable
after `workflow create`**. The only way to flip an existing workflow
today is `autosk sql --write "UPDATE workflows SET isolation=…"`,
which:

- bypasses every safety check the engine has,
- says nothing about the existing in-flight tasks that would suddenly
  expect (or stop expecting) a worktree on disk at the next step run,
- and isn't discoverable from `autosk --help` or `autosk lazy`.

This plan adds:

1. A real `autosk workflow update <name> --isolation <none|worktree>`
   CLI verb with explicit safety semantics around non-terminal tasks
   in the workflow.
2. Surfacing of `isolation` in `autosk lazy`'s Workflows panel + a
   keybinding that drives the same update through the TUI.

Out of scope: editing any other workflow field (`description`,
`first_step`, step graph). The verb's `--isolation` flag is the only
mutator in v0.4; growing it into a generic `workflow update` is a
trivial additive change later but is **not** what this plan ships.

---

## 2. Locked decisions

These are the contract — do not relitigate when implementing.

| Topic | Choice |
|---|---|
| Verb shape | `autosk workflow update <name> --isolation <none\|worktree>` plus `--force` and `--dry-run`. No positional alternate spellings. |
| Other mutators | None in v0.4. Flag set is exactly `{--isolation, --force, --dry-run, --json}`. |
| Default safety | Refuse the update when the workflow has **any** non-terminal task referencing it (statuses `new`+`workflow_id`, `in_workflow`, `human_feedback`). Exit non-zero, print the offending task ids, suggest `--force`. |
| `--force` semantics | Bypass the safety guard with mode-specific extra work (see §4.2). Always atomic across the workflow row update + per-task worktree side-effects. |
| `--dry-run` | Prints exactly what `--force` (or the plain run) would do, including the per-task worktree side-effects, without mutating anything. Exit 0 on a no-op, non-zero on a guard violation, mirroring the real run's exit shape. |
| Synthetic workflows | Always rejected, regardless of `--force`. `single:<agent>` workflows are pinned to `none` by construction (per the worktree-isolation plan §2). |
| No-op updates | Setting the same value the workflow already has is a no-op; exit 0, no commit, no warning. `--force` does not change this. |
| Daemon concurrency | Document the race ("stop the daemon or accept that runs already in flight finish on the old cwd; the next step run picks up the new mode"). No new locks introduced. |
| Lazy entry point | Single keybinding `i` on the Workflows panel (free in `internal/lazy/tui/keys.go`). Opens a two-option `popupMenu`; the destructive branch chains into a `popupConfirm`. Synthetic rows respond with a transient status-bar message instead of opening the menu. |
| Lazy ↔ CLI parity | The TUI calls the same `workflow.Store.UpdateIsolation(...)` method the CLI uses. No second code path. The `datasource.DataSource` exposes `UpdateWorkflowIsolation(ctx, name, mode, force) (UpdateIsolationReport, error)`. |

---

## 3. Design at a glance

```
              ┌──────────────────────────────────────────────────┐
              │                  shared store layer              │
              │  workflow.Store.UpdateIsolation(name, mode, opt) │
              │   - SELECTs current isolation                    │
              │   - lists non-terminal tasks                     │
              │   - none→worktree + force:                       │
              │       per task: wt.Ensure(...)                   │
              │       atomic rollback on any Ensure failure      │
              │   - worktree→none + force:                       │
              │       no on-disk cleanup; collects paths for     │
              │       the report so callers can surface them     │
              │   - UPDATE workflows SET isolation=?             │
              │   - returns UpdateIsolationReport                │
              └────────────────┬─────────────────────────────────┘
                               │
            ┌──────────────────┴───────────────────┐
            ▼                                      ▼
  ┌─────────────────────────┐         ┌─────────────────────────────┐
  │ cmd/autosk/workflow.go  │         │ internal/lazy/datasource/   │
  │  workflow update        │         │  UpdateWorkflowIsolation    │
  │  - flag parsing         │         │  - typed Report mirror      │
  │  - --dry-run short-circ.│         └──────────────┬──────────────┘
  │  - --json report        │                        │
  └─────────────────────────┘                        ▼
                                          ┌───────────────────────┐
                                          │ internal/lazy/tui/    │
                                          │  popupMenu  (kind=    │
                                          │   popupIsolation)     │
                                          │  popupConfirm (force) │
                                          │  refresh.go reads     │
                                          │   datasource.Workflow │
                                          │   .Isolation          │
                                          │  render.go shows '[wt]│
                                          │   ' badge             │
                                          └───────────────────────┘
```

The store-level method is the single chokepoint. CLI and lazy are
thin adapters.

---

## 4. Requirements

### 4.1 Functional

| ID | Requirement |
|---|---|
| FR1 | `autosk workflow update <name> --isolation <mode>` returns exit 0 on a successful flip (incl. no-op) and non-zero on any other outcome. |
| FR2 | `--isolation` accepts exactly the two values `none` and `worktree`. Any other value fails parse with `invalid --isolation: %q (want none\|worktree)`. |
| FR3 | The verb refuses synthetic workflows (`name` starts with `single:`) with `cannot update synthetic workflow %q` and exit non-zero, regardless of `--force`. |
| FR4 | The verb refuses when **any** task references the workflow in a non-terminal state (statuses ∈ `{new-with-workflow_id, in_workflow, human_feedback}`) unless `--force` is set. The error lists the offending task ids, one per line, prefixed with `non-terminal task in workflow:`. |
| FR5 | Setting the mode to the workflow's current isolation is a no-op: no commit, no side-effects, exit 0. `--json` still emits a well-formed report with `noop=true`. |
| FR6 | `none → worktree` with `--force`: for every non-terminal task, the implementation calls `worktree.Manager.Ensure(projectRoot, taskID, "" /* HEAD */)`. If any single `Ensure` fails, every successfully created worktree for this update is rolled back via `worktree.Manager.OnTerminal(projectRoot, taskID)` and the workflows row is left unchanged. The error explains which task failed and which were rolled back. |
| FR7 | `worktree → none` with `--force`: the implementation **does not** remove existing worktree directories (that would discard uncommitted state). The report carries the list of leftover `(taskID, worktreePath)` pairs for the caller to surface; the column flip happens after the report is materialised. |
| FR8 | `--dry-run` runs the same guard logic and the same Ensure-planning logic, but performs no git operations and no DB writes. It returns the same `UpdateIsolationReport` shape; the report's `dry_run` flag is true. |
| FR9 | The verb commits the doltlite write with message `workflow update <name> isolation=<old>→<new>` (or `(noop)` when no change). |
| FR10 | `autosk workflow update --json` prints the `UpdateIsolationReport` to stdout. Text mode prints a human summary that mirrors the report fields. |
| FR11 | The new `workflow.Store.UpdateIsolation` API is the only mutator of `workflows.isolation`. The existing `autosk sql --write` escape hatch keeps working (it's generic SQL), but the verb documentation discourages it. |

### 4.2 Behavioural matrix (FR4–FR7 in one table)

| Current → Target | No non-terminal tasks | Has non-terminal tasks (no `--force`) | Has non-terminal tasks (with `--force`) |
|---|---|---|---|
| `none` → `none` | no-op | no-op | no-op |
| `worktree` → `worktree` | no-op | no-op | no-op |
| `none` → `worktree` | flip column + commit | refuse, list tasks | per task: `wt.Ensure(root, id, HEAD)`; atomic rollback; flip column + commit |
| `worktree` → `none` | flip column + commit | refuse, list tasks | flip column + commit; report carries leftover worktree paths for human cleanup |

### 4.3 Non-functional

| ID | Requirement |
|---|---|
| NF1 | The verb completes in under one second for workflows with ≤ 50 affected tasks on a warm doltlite (`Ensure` itself dominates wall-clock when it runs). |
| NF2 | The implementation must not introduce a project-wide lock. The known race window (daemon picks up a task at the same instant we Ensure its worktree) is documented in `docs/workflows.md` and surfaced in the verb's `--help` text. |
| NF3 | The lazy popup chain (menu → confirm → action) is keyboard-only; mouse paths are not added in this plan. Mouse-already-bound entries on the Workflows panel keep their current behaviour. |
| NF4 | The implementation does not regress any existing golden test in `internal/lazy/tui/render_test.go` for non-isolated workflows. |

---

## 5. Implementation

### 5.1 Store API — `workflow.Store.UpdateIsolation`

New method in `internal/workflow/store.go`:

```go
// UpdateIsolation flips the workflows.isolation column on the named
// workflow. The caller passes a Manager so the method can compute
// worktree side-effects atomically alongside the column update.
//
// Returns an UpdateIsolationReport even when Err is non-nil, populated
// up to the point of failure — callers use it to surface partial
// rollbacks in error output.
func (s *Store) UpdateIsolation(
    ctx context.Context,
    name string,
    target IsolationMode,
    opts UpdateIsolationOpts,
) (UpdateIsolationReport, error)

type UpdateIsolationOpts struct {
    Force       bool
    DryRun      bool
    ProjectRoot string             // required when Force && target shift involves worktree
    Worktrees   worktree.Manager   // ditto
}

type UpdateIsolationReport struct {
    Workflow         string         `json:"workflow"`
    From             IsolationMode  `json:"from"`
    To               IsolationMode  `json:"to"`
    Noop             bool           `json:"noop"`
    DryRun           bool           `json:"dry_run"`
    NonTerminalTasks []string       `json:"non_terminal_tasks,omitempty"`
    EnsuredTasks     []EnsureRecord `json:"ensured_tasks,omitempty"`
    LeftoverWorktrees []LeftoverWorktree `json:"leftover_worktrees,omitempty"`
}

type EnsureRecord struct {
    TaskID string `json:"task_id"`
    Path   string `json:"path"`
    Branch string `json:"branch"`
    Existing bool `json:"existing"`
}

type LeftoverWorktree struct {
    TaskID string `json:"task_id"`
    Path   string `json:"path"`
}
```

Algorithm (mirrors §4.2):

1. `SELECT id, isolation, is_synthetic FROM workflows WHERE name=?`.
2. Reject synthetic (`is_synthetic=1`) regardless of `Force`.
3. If `current == target`: return `Noop=true`, no commit, exit.
4. List non-terminal tasks: `SELECT id FROM tasks WHERE workflow_id=? AND status IN ('new','in_workflow','human_feedback')` (with the additional CHECK that `status='new'` rows are kept only when `workflow_id IS NOT NULL`).
5. If non-empty and `!opts.Force`: return error wrapping a sentinel `ErrNonTerminalTasks`, with the list in the report.
6. Compute per-task side-effects:
    - `none → worktree`: plan `Ensure(root, id, "")` for each non-terminal task.
    - `worktree → none`: plan no side-effects; gather the deterministic path of each non-terminal task for `LeftoverWorktrees`.
7. If `opts.DryRun`: populate the report's plan, return.
8. Execute:
    - Open a transaction on `s.db`.
    - For `none → worktree`: call `Ensure` for each task; on first failure, run `OnTerminal` for every previously-Ensured task (best-effort, log failures); rollback tx; return error wrapping `ErrEnsureFailed`.
    - `UPDATE workflows SET isolation=? WHERE id=?`.
    - Commit tx.
9. Return the report.

The store method does **not** know about the doltlite commit message; the caller (CLI / TUI) handles that side. The doltlite write commit message is appended at the CLI / lazy layer with `dl.DoltCommit(...)`.

### 5.2 CLI verb — `cmd/autosk/workflow.go`

Add `newWorkflowUpdateCmd()` to the `workflow` group:

```go
func newWorkflowUpdateCmd() *cobra.Command {
    var (
        isolationFlag string
        force         bool
        dryRun        bool
    )
    cmd := &cobra.Command{
        Use:   "update <name>",
        Short: "Update mutable fields on a workflow (v0.4: only --isolation)",
        Long: `Flip a workflow's isolation mode.

The only mutable field today is --isolation. Other workflow fields
(description, first_step, step graph) are immutable; delete+recreate
the workflow instead.

By default the verb refuses to run if any task currently references
the workflow in a non-terminal state (new+workflow_id, in_workflow,
human_feedback). --force overrides the refusal:

  none → worktree   each non-terminal task gets a fresh worktree
                    allocated from HEAD. The update is atomic: if any
                    one Ensure fails, every prior Ensure for this run
                    is rolled back.

  worktree → none   leftover worktree directories are NOT removed
                    (that would discard uncommitted state). Their
                    paths are printed; clean them with
                    'autosk worktree rm <task-id>' once you have
                    salvaged anything you need.

Use --dry-run to preview the side-effects without committing.

Examples:
  autosk workflow update feature-dev --isolation worktree
  autosk workflow update feature-dev --isolation worktree --force --dry-run
  autosk workflow update feature-dev --isolation none --force
`,
        Args: cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            // ... parse, resolve store + manager + project root, call
            // wf.UpdateIsolation, commit on success, render report.
        },
    }
    cmd.Flags().StringVar(&isolationFlag, "isolation", "", "target isolation mode (none|worktree)")
    cmd.MarkFlagRequired("isolation")
    cmd.Flags().BoolVar(&force, "force", false, "bypass the non-terminal-tasks guard with mode-specific side-effects")
    cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen without committing")
    return cmd
}
```

Wiring:

- `workflowStoreFromCmd` already gives a `*workflow.Store` + `*doltlite.Store`. We additionally need `worktree.Manager` (one is constructed in `cmd/autosk/worktree.go`'s helpers — lift it into a small package-level factory shared by `create`, `enroll`, `done`, `cancel`, `update`).
- `projectRootFromCwd()` (existing helper in `cmd/autosk/storeio.go`/`show.go`) supplies the canonical project root.
- On success, `dl.DoltCommit(ctx, "workflow update "+name+" isolation="+from+"→"+to)`.

### 5.3 Lazy datasource — `internal/lazy/datasource/datasource.go`

Two additions:

1. `Workflow` struct gains an `Isolation string` field (mirrors the workflow.IsolationMode string).
2. `DataSource` interface gains:

   ```go
   UpdateWorkflowIsolation(
       ctx context.Context,
       name string,
       mode string,
       force bool,
   ) (UpdateIsolationReport, error)
   ```

   The implementation in `datasource.go` instantiates the same
   `worktree.Manager` the CLI uses and calls `workflow.Store.UpdateIsolation`.
   `UpdateIsolationReport` lives next to the existing `Health`,
   `Comment`, etc. types in the datasource package — a typed copy of
   the workflow-package report, with `time.Time` fields formatted as
   strings on the JSON boundary for the TUI's convenience.

   `--dry-run` is not exposed at the datasource layer; the TUI always
   makes the real call after the user confirms (the confirmation
   popup *is* the dry-run preview — see §5.4).

### 5.4 Lazy TUI

The Workflows panel grows one keybinding and two popups.

#### 5.4.1 Render — `internal/lazy/tui/render.go`

Workflow rows currently look like:

```
feature-dev          dev → review → validator     3 tasks
```

After this plan:

```
feature-dev [wt]     dev → review → validator     3 tasks
feature-dev-generic  dev → review                 0 tasks
```

The `[wt]` token is theme-colourable (added under `theme.Tokyonight`'s
existing key set; no new theme keys for now — reuse the colour used
for workflow names and dim it). Synthetic rows never carry the
marker.

#### 5.4.2 Inspector

When a workflow row is focused (Enter), the existing inspector pane
gains a top-of-section line:

```
Isolation: worktree   (n non-terminal task(s) currently use this)
```

The "n non-terminal" count is computed at refresh time and lives on
the new `datasource.Workflow.NonTerminalTaskCount` field (additive,
same path as the existing `TaskCount`).

#### 5.4.3 Keybinding — `internal/lazy/tui/keys.go`

Add:

```go
{winWorkflows, 'i', gocui.ModNone, gu.workflowIsolation},
```

`workflowIsolation` (new handler) opens `popupMenu` with two entries:

```
isolation: none         ← current
isolation: worktree
```

The currently-active mode is marked with a leading `← current`. Enter
on the current value closes the popup with no further action. Enter
on the other value chains into a `popupConfirm` whose body depends on
whether `selectedWorkflow.NonTerminalTaskCount > 0`:

- `== 0`: confirm body is

  ```
  Flip isolation: none → worktree
  No tasks are currently affected.
  ```

  Confirm with `y` runs `ds.UpdateWorkflowIsolation(name, target, false)`.

- `> 0`: confirm body is

  ```
  Flip isolation: none → worktree
  3 non-terminal task(s) will get a fresh worktree allocated from HEAD:
    - as-bea9 (status=in_workflow, current step=dev)
    - as-7321 (status=human_feedback, current step=review)
    - as-9c01 (status=in_workflow, current step=validator)
  This is the --force path. Continue?
  ```

  Or, for `worktree → none`:

  ```
  Flip isolation: worktree → none
  3 non-terminal task(s) keep their worktree directories on disk:
    - as-bea9   ~/.autosk/worktrees/.../as-bea9
    - ...
  Remove them later with `autosk worktree rm <task-id>` once
  you've salvaged anything you need. Continue?
  ```

  Confirm with `y` runs `ds.UpdateWorkflowIsolation(name, target, true)`.

The Tasks panel is **not** modified. The worktree block in the
Inspector that the predecessor plan added is already rendering the
right data; nothing new for tasks here.

Synthetic workflows: `i` on a `single:*` row drops a one-line
status-bar message (`isolation is locked to 'none' on synthetic
workflows`) and does not open the menu.

#### 5.4.4 popup state — `internal/lazy/tui/state.go`

`popupKind` enum gains one entry:

```go
popupIsolation popupKind = iota+5  // after popupTaskCompose
```

Reusing `popupMenu` directly would also work; a dedicated kind makes
the test pin (`popup_isolation_test.go`) trivially scoped. We do not
add a new test file — extend the existing `popup_test.go` keymap pin
to cover the `i`-on-Workflows binding.

### 5.5 Documentation

- `docs/workflows.md` § "Isolation": replace the "v0.3 only flips at
  create" paragraph with a § Updating isolation that documents the
  new verb, the safety matrix, and the daemon race.
- `docs/lazy.md`: add the `i` binding to the Workflows-panel
  keymap section; reference §5.4 of this plan for the popup chain.
- `cmd/autosk/workflow.go`'s long help text on `workflow update`
  (already drafted in §5.2) is the primary reference for CLI users.

---

## 6. Acceptance criteria

The plan is complete when every box below is green.

### 6.1 CLI

- [ ] `autosk workflow update feature-dev --isolation worktree` on a
      workflow with no non-terminal tasks flips the column, commits,
      exits 0, prints a one-line summary.
- [ ] Same call with `--json` emits a `UpdateIsolationReport` object;
      `noop=false`, `non_terminal_tasks=[]`, `from`/`to` populated.
- [ ] `autosk workflow update feature-dev --isolation worktree` on a
      workflow with non-terminal tasks exits non-zero, prints each
      task id on its own line under `non-terminal task in workflow:`,
      makes **no** DB write (verify via `autosk workflow show --json`
      isolation unchanged), makes **no** git side-effects.
- [ ] Same call with `--force` allocates worktrees for every
      non-terminal task and flips the column atomically. A simulated
      `Ensure` failure on task N rolls back every worktree created
      earlier in the same run and leaves the column unchanged.
- [ ] `autosk workflow update feature-dev --isolation none --force`
      on a workflow with non-terminal isolated tasks flips the column
      and prints each leftover worktree path under
      `leftover worktree (not removed):`.
- [ ] `--dry-run` on every above scenario performs no DB writes and
      no git operations; the printed report matches what the real run
      would produce except for `dry_run=true`.
- [ ] Setting the same value the workflow already has is a no-op:
      exit 0, no doltlite commit (verify with `git -C .autosk log`
      against the doltlite checkout), `noop=true` in `--json`.
- [ ] `autosk workflow update single:@autosk/developer --isolation
      worktree` fails with `cannot update synthetic workflow ...`
      regardless of `--force`.
- [ ] Invalid `--isolation` values are caught at flag parse time.

### 6.2 Daemon

- [ ] After `none → worktree --force`, a daemon-driven step run on
      one of the affected tasks executes with cwd at the new
      worktree path and `AUTOSK_DB` env populated (e2e test mirroring
      `internal/daemon/executor/worktree_test.go`).
- [ ] After `worktree → none --force`, a subsequent daemon-driven
      step run on a task that previously had a worktree executes with
      cwd at the project root (existing on-disk worktree is left
      untouched).

### 6.3 Lazy

- [ ] Workflows panel renders the `[wt]` marker exactly on rows whose
      `Isolation == "worktree"`; synthetic rows never carry it.
- [ ] Workflow inspector shows `Isolation: <mode>` with the
      non-terminal task count when applicable.
- [ ] `i` on a non-synthetic Workflows row opens a `popupMenu` with
      two entries; the currently-active mode is marked.
- [ ] Confirming the current value closes the popup with no action
      (no datasource call observed).
- [ ] Confirming a different value with no non-terminal tasks chains
      into a single-line `popupConfirm`; `y` invokes
      `UpdateWorkflowIsolation(..., force=false)`; the panel
      refreshes; the marker is added/removed.
- [ ] Confirming a different value with non-terminal tasks chains
      into a `popupConfirm` whose body enumerates every affected
      task; `y` invokes `UpdateWorkflowIsolation(..., force=true)`;
      the panel refreshes; the report's `leftover_worktrees` (when
      applicable) is rendered as a transient status-bar message
      ("3 worktree(s) left on disk; use `autosk worktree rm`").
- [ ] `i` on a synthetic row drops a status-bar message and does NOT
      open the menu.
- [ ] Existing golden tests in `internal/lazy/tui/render_test.go`
      for non-isolated workflows are unchanged.

### 6.4 Documentation

- [ ] `docs/workflows.md` has a § "Updating isolation" with the
      safety matrix and the daemon race note.
- [ ] `docs/lazy.md` lists the `i` binding under the Workflows
      panel section.
- [ ] `autosk workflow update --help` mentions `--isolation`,
      `--force`, `--dry-run`, and the dangerous semantics of
      `worktree → none --force`.

---

## 7. Phases

Sized one autosk task per phase. Strictly linear.

| ID | Phase | Done when |
|---|---|---|
| **WU1** | **Store API** | `workflow.Store.UpdateIsolation` + report types + sentinel errors (`ErrNonTerminalTasks`, `ErrEnsureFailed`, `ErrSyntheticImmutable`). Unit tests cover every cell of the matrix in §4.2 with a real doltlite temp DB and a fake `worktree.Manager`. |
| **WU2** | **CLI verb** | `autosk workflow update <name> --isolation/--force/--dry-run/--json` lands. Text + JSON output paths. Golden tests cover the happy paths and every error path; e2e test against a real git temp repo covers the atomic-rollback path. |
| **WU3** | **Lazy datasource + render** | `datasource.Workflow.Isolation` + `NonTerminalTaskCount`. Workflow row render shows `[wt]`. Workflow inspector shows the isolation line. Golden tests updated. |
| **WU4** | **Lazy popup chain** | `popupIsolation` kind, the `i` binding on `winWorkflows`, the menu → confirm chain, synthetic-row status-bar message. `popup_test.go` pins the new binding; `popup_compose_test.go`-style state tests cover both confirm bodies. |
| **WU5** | **Docs** | `docs/workflows.md`, `docs/lazy.md`, `README.md` roadmap. The verb's `--help` long text matches §5.2. |

WU1 → WU2 → WU3 → WU4 → WU5. WU2 and WU3 can be parallelised after
WU1 if the implementer wants, but the PR order is the table order.

---

## 8. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Daemon picks up a task at the exact moment we Ensure its worktree. | Documented in `docs/workflows.md` and in the verb's `--help`. Real damage is limited to "one run finishes in the old cwd". A future iteration can add an advisory project lock if this turns out to bite. |
| `none → worktree --force` Ensure rollback misses a directory (e.g. process killed between Ensure and rollback). | The rollback path is best-effort logged; the next `autosk worktree list` surfaces orphans; `autosk worktree rm` reaps them. Same recovery story as a daemon crash mid-worktree-create. |
| User confuses `--dry-run` with a guard-bypass and thinks it's a free preview of `--force` semantics. | The `--help` text explicitly states `--dry-run` *requires* `--force` to enable the `--force` planning path (otherwise dry-run just shows the refusal). The report's `dry_run` flag is also surfaced in text mode. |
| Lazy `popupConfirm` body grows past the popup's max height when many non-terminal tasks exist. | Cap the rendered task list at 10 entries with a `... and N more` suffix; the full list is always available via `autosk workflow show <name>` and `autosk list --workflow <name>`. |
| Lazy keybinding `i` collides with a future binding. | Pin via `popup_test.go` (already the convention). No other Workflows-panel binding uses `i` today (`n`, `D`, `Enter`, `j`/`k`). |

---

## 9. Out of scope

- Editing any workflow field other than `isolation`.
- Renaming a workflow.
- A workflow-level `default_max_visits` knob co-edited from the same
  verb.
- A lazy keybinding for `autosk worktree rm` (the leftover worktrees
  in the `worktree → none --force` path are surfaced as a hint, not
  as an actionable list item — the user runs the CLI verb).
- Project-wide locking against the daemon race in §8.
- Multi-workflow batch update (`autosk workflow update --all
  --isolation worktree`).

These either have no design pressure today or are obvious follow-ups.

---

## 10. Cross-references

- Predecessor: `docs/plans/20260521-Worktree-Isolation.md` —
  established `workflows.isolation`, the deterministic path scheme,
  and the `worktree.Manager` interface.
- Schema column lives in `internal/migrations/005_workflow_isolation.sql`.
- Lazy TUI conventions: `docs/lazy.md` § Keymap; popup machinery in
  `internal/lazy/tui/popup.go`.
