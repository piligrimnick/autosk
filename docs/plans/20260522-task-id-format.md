# Task id format migration — `as-XXXX` → `ask-XXXXXX`

## Background

autosk task ids were minted with a 2-byte / 4-hex suffix
(`as-XXXX`, 65 536 keyspace). The keyspace was already uncomfortably
small for projects that go through hundreds of throw-away `human` /
`cancel` tasks during normal use of the workflow engine — the
collision-retry loop in `id.NewUnique` was firing more than it
should, and exhaustion was a single bad month away.

This plan records the **hard cut-over** to a 3-byte / 6-hex suffix
(`ask-XXXXXX`, ~16.7M keyspace). It captures both the change
itself and the explicit out-of-scope decisions so future readers
understand what the migration deliberately did **not** do.

Historical context: this format flip is independent of the v0.1 →
v0.2 schema break (which is non-migratable). Existing v0.2 databases
are migrated in place by migration 007.

## Format

Old: `as-XXXX`    (prefix `as`,  4-hex suffix)
New: `ask-XXXXXX` (prefix `ask`, 6-hex suffix)

Agent ids (`ag-XXXX`) are **not** touched — they keep the 2-byte /
4-hex shape. The regex in `internal/id/id.go` is intentionally left
loose (`^[a-z]+-[0-9a-f]{4}([0-9a-f]{2})*$`) so `ag-XXXX` and any
pre-migration `as-XXXX` strings still round-trip through `id.Valid`.

A new strict shape check `assertTaskIDShape` lives in
`internal/store/doltlite/tasks.go` and rejects caller-supplied
non-canonical task ids on `CreateTask` with
`store.ErrInvalidShape`. The generator path (`id.NewUnique` →
`id.New("ask")`) already produces the canonical 10-char shape.

## Padding rule for migrated rows

Legacy ids are rewritten with a two-zero left-pad on the hex
suffix so the mapping is reversible and never collides with
freshly minted random ids:

    as-XXXX  →  ask-00XXXX

Random new ids only use the full 6-hex space (positions 1-6 can be
anything `0..f`), but no fresh id will ever start with two zero hex
chars by accident — the probability is `1/256`, but the migration
reserves the entire `ask-00....` slice for legacy. Practically the
generator could still mint `ask-00abcd`, which would only collide
with a real migrated id, so the create path runs through the same
unique-id retry loop it always has; the reservation is documentary
rather than enforced.

## FK map (migration 007)

FKs on `tasks(id)` are all `ON DELETE CASCADE` (no `ON UPDATE
CASCADE`), so updating `tasks.id` alone would orphan children.
Migration 007 runs with `PRAGMA foreign_keys = OFF` (via the
existing `autosk-migration: foreign_keys_off` directive in
`internal/migrations/migrations.go`) and rewrites every dependent
column, then `PRAGMA foreign_key_check` before COMMIT:

- `tasks.id`
- `task_deps.blocker_id`
- `task_deps.blocked_id`
- `comments.task_id`
- `daemon_runs.task_id`
- `step_signals.task_id`

`tasks.metadata` (free-form JSON) is left alone — no callers plant
task-id refs in there today.

The migration is idempotent: applying 007 on a database that
contains no `as-` rows is a no-op, and re-applying on a database
already migrated is also a no-op because the `GLOB
'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'` filter no longer matches
anything. The migrations conformance suite pins both invariants.

## Out of scope (decided)

- **No `autosk db backup` CLI verb, no `make backup-db` target.**
  The operator handles backup with `cp -a` before running the
  migration. See the runbook below.
- **No `autosk db migrate-worktrees` and no pre-flight bail when
  `work` / `human` tasks exist.** Old `autosk/as-XXXX` branches and
  `~/.autosk/worktrees/<slug>/as-XXXX/` directories are orphaned;
  the daemon will create fresh `autosk/ask-00XXXX` ones on next
  run when the task next enters a `work` step. Migrated tasks that
  were not active across the migration may end up with two
  worktree branches in their git remote (the dead legacy one and
  the live new one); operators can prune the dead branch by hand.
- **No auto-rewrite or warn for legacy `as-` strings found inside
  `tasks.metadata`.** The column is left untouched; if a user has
  pinned task-id references in metadata they need to update them
  manually.
- **Agent ids (`ag-XXXX`) keep their 2-byte / 4-hex shape.** They
  use a much smaller name space (one row per agent package), the
  4-hex keyspace is plenty, and bumping them would require a
  parallel migration 008. Not worth the churn.

## Operator runbook

1. Stop the daemon:

       autosk daemon stop

   Verify no `autosk daemon serve` process is still running against
   the project DB. `pgrep -af 'autosk daemon serve'` is the easy
   check; daemons keep `~/.autosk/daemon.sock` open while they're
   alive.

2. **Back up the database.** This is mandatory — there is no
   shipped tooling for it because doltlite's on-disk format is a
   single file, so a vanilla `cp -a` is enough:

       cp -a .autosk/db .autosk/db.bak.$(date +%Y%m%d-%H%M%S)

3. Run the migration:

       autosk migrate

   This applies migrations 001..007 in order; 007 is the task-id
   rewrite. The command exits non-zero if the migration fails for
   any reason (FK check, schema drift, write error) — in that case,
   roll back as described below.

4. Restart the daemon:

       autosk daemon serve &

5. Sanity-check from `autosk lazy`:

   - Tasks panel shows `ask-XXXXXX` ids in the 10-cell id column
     with the title column still aligned to the right.
   - Drilling into a migrated task (`Enter` on a Tasks row) shows
     the new id in the Detail header, and the comments / signals /
     jobs blocks still resolve through the rewritten FKs.
   - For isolated workflows whose worktrees survived the migration,
     the `─ recent jobs` and `─ worktree` blocks still show the
     legacy `autosk/as-XXXX` branch + on-disk dir (this is the
     documented limitation — see "Out of scope" above).

## Rollback

If migration 007 corrupts the database or leaves the build red:

    autosk daemon stop
    rm -rf .autosk/db
    mv .autosk/db.bak.<timestamp> .autosk/db
    git revert <head>  # reverts code changes for this task
    autosk daemon start

New ids minted between the migration and the rollback will not
exist in the restored DB; check `daemon_runs` and active jobs
before rolling back.

## Visual width budget (TUI)

The Tasks panel in `autosk lazy` has its id column widened from 7
cells to 10 cells (`internal/lazy/tui/render.go` — `idW = 10`,
matching the literal width of `ask-XXXXXX`). The title column
absorbs the 3-cell loss; no other panel widths change. The detail
pane renderers (`renderTaskDetail`, `renderJobDetail`,
`renderInspectorHeader`) don't pad on id width, so they need no
follow-up changes.

`internal/render/render.go` (used by `autosk show`) relies on
`text/tabwriter`, so its column widths adapt automatically to the
longest id in the table. No constant flip needed there.

## Acceptance criteria

Recorded as part of the task itself; preserved here for the
project's archeological record:

1. Migration applies cleanly on a populated v0.2 database. Every
   `tasks.id` matches `^ask-[0-9a-f]{6}$` after migrate; FK joins
   resolve via the new ids; `PRAGMA foreign_key_check` returns no
   rows after migration.
2. Migration is idempotent on a fresh database and on an already-
   migrated database.
3. `id.New("ask")` and `id.NewUnique("ask", …)` produce 10-char
   `ask-` + 6 lowercase hex.
4. `store.CreateTask` rejects caller-supplied legacy ids
   (`as-XXXX`) with `store.ErrInvalidShape`.
5. `make test` / `make vet` / `make lint` pass.
6. Lazy TUI renders the wider id column without title-column
   overflow; selected-row highlight still covers the whole row.
7. `autosk show <id>` output uses the new format in both text and
   `--json` modes. The worktree block for migrated tasks still
   shows the legacy `autosk/as-XXXX` branch + path (documented
   limitation).
8. TypeScript SDK examples mention the new format; `pi --mode rpc`
   smoke test (`extension/test/smoke.ts`) executes with the
   updated fixture id.
9. Every example in `README.md`, `docs/lazy.md`, `docs/workflows.md`,
   `cmd/autosk/enroll.go` long-help, and `extension/src/*.ts`
   shows `ask-XXXXXX`. Historical `docs/plans/*` entries are left
   untouched on purpose.
10. A `.autosk/db.bak.<timestamp>` snapshot sits next to
    `.autosk/db` before the migration is merged.
