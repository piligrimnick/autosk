-- autosk-migration: foreign_keys_off
--
-- 007_task_id_format.sql — rename every legacy `as-XXXX` task id to
-- `ask-00XXXX`. The two-zero left-pad on the hex suffix makes the
-- mapping reversible and keeps the migrated namespace disjoint from
-- freshly minted random ids (which use 3 bytes / 6 hex chars).
--
-- # Why this is a rebuild, not a flat UPDATE
--
-- doltlite (the Dolt SQL engine that backs autosk's store) does NOT
-- honor UPDATE against PRIMARY KEY columns: the statement reports
-- rows-affected but the rows are inserted-as-duplicates rather than
-- renamed in place. The original draft of this migration tried a
-- straight UPDATE on `tasks.id` and `task_deps.{blocker_id,blocked_id}`
-- and the post-statement state was:
--
--     tasks still `as-*`  : 56   (old rows lingered)
--     tasks now   `ask-*` : 112  (new rows piled on top)
--     foreign_key_check   : 28 violations  (children orphaned)
--
-- So any table whose primary key carries a task id has to go through
-- the SQLite-canonical rebuild dance — CREATE *_new, INSERT…SELECT
-- with a CASE-rewrite on the id columns, DROP old, ALTER … RENAME.
-- This is the same pattern migration 006_short_statuses.sql uses to
-- rewrite the status enums on `tasks` / `step_transitions` /
-- `daemon_runs`.
--
-- Tables rebuilt:
--   - tasks       (id is PK)
--   - task_deps   (blocker_id, blocked_id are part of the composite PK)
--
-- Tables touched with plain UPDATE (task_id is just an FK column, not
-- part of the PK — doltlite handles those normally):
--   - comments
--   - daemon_runs
--   - step_signals
--
-- FK enforcement is off for the whole migration via the leading
-- directive. PRAGMA foreign_key_check fires inside the transaction
-- before COMMIT so any dangling reference still aborts the rebuild.

------------------------------------------------------------------------
-- tasks
------------------------------------------------------------------------

CREATE TABLE tasks_new (
  id              TEXT PRIMARY KEY,
  title           TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'new'
                  CHECK (status IN ('new','work','human','done','cancel')),
  priority        INTEGER NOT NULL DEFAULT 2
                  CHECK (priority BETWEEN 0 AND 3),
  author_id       TEXT REFERENCES agents(id),
  workflow_id     TEXT REFERENCES workflows(id),
  current_step_id TEXT REFERENCES steps(id),
  metadata        TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  CHECK (
    (status =  'work' AND current_step_id IS NOT NULL)
    OR
    (status <> 'work' AND (current_step_id IS NULL OR status = 'human'))
  )
)
;

INSERT INTO tasks_new(id, title, description, status, priority,
                      author_id, workflow_id, current_step_id, metadata,
                      created_at, updated_at)
SELECT CASE WHEN id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
            THEN 'ask-00' || substr(id, 4)
            ELSE id
       END,
       title, description, status, priority,
       author_id, workflow_id, current_step_id, metadata,
       created_at, updated_at
  FROM tasks
;

DROP TABLE tasks
;

ALTER TABLE tasks_new RENAME TO tasks
;

CREATE INDEX idx_tasks_status_prio ON tasks(status, priority, created_at)
;
CREATE INDEX idx_tasks_step        ON tasks(current_step_id)
;

------------------------------------------------------------------------
-- task_deps
------------------------------------------------------------------------

CREATE TABLE task_deps_new (
  blocker_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  blocked_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL DEFAULT 'blocks' CHECK (kind = 'blocks'),
  PRIMARY KEY (blocker_id, blocked_id, kind),
  CHECK (blocker_id <> blocked_id)
)
;

INSERT INTO task_deps_new(blocker_id, blocked_id, kind)
SELECT CASE WHEN blocker_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
            THEN 'ask-00' || substr(blocker_id, 4)
            ELSE blocker_id
       END,
       CASE WHEN blocked_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
            THEN 'ask-00' || substr(blocked_id, 4)
            ELSE blocked_id
       END,
       kind
  FROM task_deps
;

DROP TABLE task_deps
;

ALTER TABLE task_deps_new RENAME TO task_deps
;

CREATE INDEX idx_deps_blocked ON task_deps(blocked_id)
;

------------------------------------------------------------------------
-- comments  —  task_id is an ordinary FK column, plain UPDATE works.
------------------------------------------------------------------------

UPDATE comments
   SET task_id = 'ask-00' || substr(task_id, 4)
 WHERE task_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
;

------------------------------------------------------------------------
-- daemon_runs  —  task_id is an ordinary FK column, plain UPDATE works.
------------------------------------------------------------------------

UPDATE daemon_runs
   SET task_id = 'ask-00' || substr(task_id, 4)
 WHERE task_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
;

------------------------------------------------------------------------
-- step_signals  —  task_id is an ordinary column (run_id is PK).
------------------------------------------------------------------------

UPDATE step_signals
   SET task_id = 'ask-00' || substr(task_id, 4)
 WHERE task_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
;
