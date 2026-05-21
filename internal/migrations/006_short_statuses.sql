-- autosk-migration: foreign_keys_off
--
-- 006_short_statuses.sql -- rename every persisted status enum to a value
-- that is <= 7 characters. Mapping:
--
--   tasks.status:            in_workflow    -> work
--                            human_feedback -> human
--                            cancelled      -> cancel
--   step_transitions.task_status:
--                            human_feedback -> human
--                            cancelled      -> cancel
--   daemon_runs.status:      cancelled      -> cancel
--
-- This is a hard break: legacy spellings are rejected by the new CHECK
-- constraints and by the Go enum guards (see store.Status / runstore.RunStatus).
--
-- SQLite cannot ALTER a CHECK constraint in place, so each affected table
-- is rebuilt via the canonical 12-step procedure
-- (https://www.sqlite.org/lang_altertable.html). Because task_deps,
-- comments, daemon_runs and step_signals declare ON DELETE CASCADE FKs
-- against the tables we rebuild, the migration runner disables FK
-- enforcement for the duration of this migration (see the leading
-- directive). PRAGMA foreign_key_check fires inside the transaction
-- before COMMIT so any broken reference aborts the rebuild.

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
  -- The closest SQL can express to the section 5.1 invariant: status='work'
  -- iff a step is set. Other invariants live in app code.
  CHECK (
    (status =  'work' AND current_step_id IS NOT NULL)
    OR
    (status <> 'work' AND (current_step_id IS NULL OR status = 'human'))
  )
);

INSERT INTO tasks_new(id, title, description, status, priority,
                      author_id, workflow_id, current_step_id, metadata,
                      created_at, updated_at)
SELECT id, title, description,
       CASE status
         WHEN 'in_workflow'    THEN 'work'
         WHEN 'human_feedback' THEN 'human'
         WHEN 'cancelled'      THEN 'cancel'
         ELSE status
       END,
       priority, author_id, workflow_id, current_step_id, metadata,
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
-- step_transitions
------------------------------------------------------------------------
--
-- step_transitions.id is INTEGER PRIMARY KEY AUTOINCREMENT. Inserting
-- rows with explicit ids preserves their values; SQLite updates
-- sqlite_sequence to MAX(id) automatically as the insert proceeds, and
-- ALTER TABLE ... RENAME migrates the sqlite_sequence row to the new
-- table name, so the next AUTOINCREMENT id continues where the original
-- left off.

CREATE TABLE step_transitions_new (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id      TEXT NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
  next_step_id TEXT REFERENCES steps(id) ON DELETE CASCADE,
  task_status  TEXT,
  prompt_rule  TEXT NOT NULL,
  CHECK ((next_step_id IS NULL) <> (task_status IS NULL)),
  CHECK (task_status IS NULL OR task_status IN ('human','done','cancel'))
)
;

INSERT INTO step_transitions_new(id, step_id, next_step_id, task_status, prompt_rule)
SELECT id, step_id, next_step_id,
       CASE task_status
         WHEN 'human_feedback' THEN 'human'
         WHEN 'cancelled'      THEN 'cancel'
         ELSE task_status
       END,
       prompt_rule
  FROM step_transitions
;

DROP TABLE step_transitions
;

ALTER TABLE step_transitions_new RENAME TO step_transitions
;

CREATE INDEX idx_step_transitions_step ON step_transitions(step_id)
;

------------------------------------------------------------------------
-- daemon_runs
------------------------------------------------------------------------
--
-- step_signals carries an ON DELETE CASCADE FK to daemon_runs(job_id);
-- with FK enforcement disabled by the migration directive, dropping the
-- old daemon_runs does NOT cascade-delete the signal rows. The signals
-- continue to reference the same job_id values, which we preserve on
-- the copy, so once FK enforcement is re-enabled the invariant holds.

CREATE TABLE daemon_runs_new (
  job_id           TEXT PRIMARY KEY,
  task_id          TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  step_id          TEXT NOT NULL REFERENCES steps(id) ON DELETE RESTRICT,
  status           TEXT NOT NULL
                   CHECK (status IN ('queued','running','done','failed','cancel')),
  transition_id    INTEGER REFERENCES step_transitions(id),
  exit_code        INTEGER,
  pid              INTEGER,
  pi_session_id    TEXT,
  session_path     TEXT,
  error            TEXT,
  corrections_used INTEGER NOT NULL DEFAULT 0,
  max_corrections  INTEGER NOT NULL DEFAULT 3,
  created_at       INTEGER NOT NULL,
  started_at       INTEGER,
  finished_at      INTEGER
)
;

INSERT INTO daemon_runs_new(job_id, task_id, step_id, status, transition_id,
                            exit_code, pid, pi_session_id, session_path, error,
                            corrections_used, max_corrections,
                            created_at, started_at, finished_at)
SELECT job_id, task_id, step_id,
       CASE status WHEN 'cancelled' THEN 'cancel' ELSE status END,
       transition_id, exit_code, pid, pi_session_id, session_path, error,
       corrections_used, max_corrections,
       created_at, started_at, finished_at
  FROM daemon_runs
;

DROP TABLE daemon_runs
;

ALTER TABLE daemon_runs_new RENAME TO daemon_runs
;

CREATE INDEX idx_runs_task   ON daemon_runs(task_id, created_at)
;
CREATE INDEX idx_runs_status ON daemon_runs(status, created_at)
;
