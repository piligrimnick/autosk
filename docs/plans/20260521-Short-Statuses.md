# Short statuses — plan

**Status:** shipped (migration 006).
**Date:** 2026-05-21.
**Owners:** autosk core.
**Related code:**
`internal/migrations/006_short_statuses.sql`,
`internal/migrations/migrations.go`,
`internal/store/`,
`internal/store/conformance/status_lengths.go`,
`internal/workflow/`,
`internal/daemon/runstore/`,
`internal/daemon/executor/`,
`cmd/autosk/`,
`internal/lazy/`,
`extension/src/`,
`extension/sdk/src/`,
`agents/`.

---

## 1. Motivation

Every status spelling in autosk is on a hot path:

- Persisted on every task / step transition / daemon run row.
- Repeated in CLI help text, flag value lists, error messages,
  workflow YAML grammar and validation messages, the TS SDK schema,
  reference agent prompts, golden test fixtures, and the lazy TUI's
  status column.
- Rendered as a column in `autosk list`, `autosk lazy`'s tasks panel,
  every JSON renderer, and any `--json` consumer that pivots on
  status.

The pre-rename vocabulary mixed short tokens (`new`, `done`) with
quite long ones (`in_workflow` = 11 chars, `human_feedback` = 14
chars). The TUI compensated with abbreviation logic
(`renderTaskStatusCell` collapsed long tokens to `in_wor…` /
`human_…` / `cancel…`); the CLI burned vertical space; agent prompts
quoted the long forms verbatim and the model had to spell them
exactly right for `autosk step next --to …` to land.

Cap every persisted status at ≤ 7 characters and the truncation
disappears, the columns stay tight, and the prompts get shorter.

## 2. Final mapping

| Column / enum                       | Old value          | New value | Notes              |
|-------------------------------------|--------------------|-----------|--------------------|
| `tasks.status`                      | `in_workflow`      | `work`    |                    |
| `tasks.status`                      | `human_feedback`   | `human`   |                    |
| `tasks.status`                      | `cancelled`        | `cancel`  |                    |
| `tasks.status`                      | `new`, `done`      | unchanged | already ≤ 7        |
| `step_transitions.task_status`      | `human_feedback`   | `human`   |                    |
| `step_transitions.task_status`      | `cancelled`        | `cancel`  |                    |
| `step_transitions.task_status`      | `done`             | unchanged |                    |
| `daemon_runs.status`                | `cancelled`        | `cancel`  |                    |
| `daemon_runs.status`                | `running`          | unchanged | 7 chars, fits cap  |
| `daemon_runs.status`                | `queued`/`done`/`failed` | unchanged |              |

Go identifiers were renamed in lockstep:

- `store.StatusInWorkflow` → `store.StatusWork`
- `store.StatusHumanFeedback` → `store.StatusHuman`
- `store.StatusCancelled` → `store.StatusCancel`
- `runstore.StatusCancelled` → `runstore.StatusCancel`

The `autosk cancel` CLI verb keeps its name — the verb spelling and
the new wire value happen to match; no collision, no rename.

## 3. Compatibility stance

Hard break. No legacy spellings are accepted on any input surface:

- CLI flags (`--status`, `--to`),
- workflow YAML / JSON (`task_status`),
- the `autosk_task` / `autosk_step` JSON-RPC SDK (Typebox literals),
- error message hints (CLI prints the new vocabulary).

Existing on-disk rows are rewritten in place by migration 006.
Workflow YAML authored against the old vocabulary fails validation at
`workflow create/update` with a clear error pointing at the rename.

## 4. Migration mechanism

The SQLite recipe for changing a CHECK constraint is a full table
rebuild (`CREATE _new` → `INSERT … SELECT` with `CASE` → `DROP` →
`RENAME`). With CASCADE FKs declared on `task_deps`, `comments`,
`daemon_runs`, `step_signals`, a naive `DROP` of the parent table
would wipe dependent rows. SQLite mandates
`PRAGMA foreign_keys = OFF` **outside** the surrounding transaction
for this pattern, but the original migration runner wraps every
migration body in a single `BeginTx`.

The implementation picked **option A** (directive in the SQL file)
over option B (a per-version Go hook):

- Migration files may start with `-- autosk-migration: foreign_keys_off`.
- `migrations.applyOneFKOff` is the dedicated path that pins a
  `*sql.Conn`, runs `PRAGMA foreign_keys = OFF` outside a tx,
  applies the migration body inside a tx, runs `PRAGMA
  foreign_key_check` before COMMIT (aborts on any dangling FK),
  records `schema_migrations`, COMMITs, then `PRAGMA foreign_keys =
  ON` (deferred — fires on panic too).
- All migration SQL stays in one self-contained file; future
  migrations that need the same dance just add the directive and
  inherit the path. No new dispatcher entry per migration.

`006_short_statuses.sql` then performs three rebuilds in order:

1. `tasks` — new CHECK enum (`'new','work','human','done','cancel'`)
   and the matching invariant CHECK (`status='work' ⇔
   current_step_id IS NOT NULL`). Rewrites use
   `CASE status WHEN 'in_workflow' THEN 'work' …`. Recreates
   `idx_tasks_status_prio`, `idx_tasks_step`.
2. `step_transitions` — new CHECK
   (`task_status IS NULL OR task_status IN ('human','done','cancel')`).
   Recreates `idx_step_transitions_step`. Copies `id` and
   `sqlite_sequence` explicitly so AUTOINCREMENT survives.
3. `daemon_runs` — new CHECK
   (`status IN ('queued','running','done','failed','cancel')`).
   Recreates `idx_runs_task`, `idx_runs_status`.
   `step_signals` has `FK → daemon_runs(job_id) ON DELETE CASCADE`,
   which is the reason the FK-off recipe is mandatory in the first
   place.

Each rebuild's pre-COMMIT `foreign_key_check` is the canonical guard
against a botched copy: if any dependent row dangles, the
transaction aborts and the user sees a precise SQLite error instead
of silent data loss.

## 5. Static guard against regression

`internal/store/conformance/status_lengths.go` iterates
`store.AllStatuses()` and `runstore.AllStatuses()` and asserts
`len(s) <= 7` (`MaxStatusLen = 7`). Wired into `RunConformance` so
every backend test sees the guard. A future addition of e.g.
`StatusPaused` (or any other 8+ char value) trips this static check
in CI before it can land on disk.

## 6. Rollout & rollback

- Single migration version bump (5 → 6). Existing dev / test DBs
  migrate automatically on next CLI invocation. No `--force` flag,
  no opt-out.
- Daemon hot upgrade: stop daemon → `autosk migrate` → restart.
- Dirty cache rollback: `rm -rf .autosk/db` and re-init.

## 7. Acceptance criteria

1. `SELECT DISTINCT status FROM tasks UNION SELECT DISTINCT
   task_status FROM step_transitions UNION SELECT DISTINCT status
   FROM daemon_runs` on a migrated DB returns only the new
   vocabulary; every value has `length(value) <= 7`.
2. CHECK constraints reject every legacy spelling (`in_workflow`,
   `human_feedback`, `cancelled`).
3. `autosk step next --to human_feedback` fails with a CLI-side
   error pointing at the new spelling (no SQL-layer constraint
   violation reaches the user).
4. Workflow YAML containing `task_status: cancelled` fails
   `workflow create/update` validation with a message naming the new
   vocabulary.
5. `autosk_task`, `autosk_step` JSON-RPC schemas reject legacy
   status strings (Typebox validation error).
6. Lazy TUI renders the new strings without ellipsis truncation in
   default terminal widths.
7. All reference agents (developer, task-validator) emit only new
   transition tokens in their canned prompts.
8. Every doc page under `docs/` that documents statuses is
   consistent with the new vocabulary (`docs/plans/` historical
   files excluded — they are point-in-time records).

## 8. Out of scope

- Migrating the `pi-rpc` `cancelled:true` extension-UI reply
  semantics (different domain — that field is a dialog
  cancellation, not a task status).
- Trimming the `running` `daemon_runs.status` token (already 7
  chars; fits the cap).
- Re-spelling historical plans under `docs/plans/`.

## Pointers

- Migration runner directive + integration test:
  `internal/migrations/migrations.go`,
  `internal/migrations/migrations_test.go::TestApply006_ShortStatuses_RewritesAndPreservesDeps`.
- Migration body: `internal/migrations/006_short_statuses.sql`.
- Static guard: `internal/store/conformance/status_lengths.go`.
- Live status enum: see [`docs/workflows.md` § Task](../workflows.md#task)
  and the root README.
