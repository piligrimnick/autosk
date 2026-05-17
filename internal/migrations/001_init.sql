-- 001_init.sql — v0.2 schema for autosk (workflows + agents).
--
-- See docs/plans/20260517-Workflows-Plan.md §4.1 for the canonical spec.
-- This migration drops anything v0.1 left behind. migrations.go refuses to
-- run on a v0.1 database (see CheckV1).

-- Agents. `human` is seeded just after this migration runs (see
-- migrations.go SeedHumanAgent). The agents row identity is a small id;
-- name is the user-visible handle (and what workflows reference).
CREATE TABLE agents (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  is_human   INTEGER NOT NULL DEFAULT 0
             CHECK (is_human IN (0,1)),
  created_at INTEGER NOT NULL
);

CREATE TABLE workflows (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  description   TEXT NOT NULL DEFAULT '',
  first_step_id TEXT NOT NULL,
  is_synthetic  INTEGER NOT NULL DEFAULT 0
                CHECK (is_synthetic IN (0,1)),
  created_at    INTEGER NOT NULL
);

CREATE TABLE steps (
  id          TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  agent_id    TEXT NOT NULL REFERENCES agents(id),
  seq         INTEGER NOT NULL,         -- source-file order within the workflow
  UNIQUE (workflow_id, name)
);

-- Transitions are write-only from the engine's point of view; users never
-- reference the integer id directly.
CREATE TABLE step_transitions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id      TEXT NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
  next_step_id TEXT REFERENCES steps(id) ON DELETE CASCADE,
  task_status  TEXT,
  prompt_rule  TEXT NOT NULL,
  CHECK ((next_step_id IS NULL) <> (task_status IS NULL)),
  CHECK (task_status IS NULL OR task_status IN ('human_feedback','done','cancelled'))
);
CREATE INDEX idx_step_transitions_step ON step_transitions(step_id);

CREATE TABLE tasks (
  id              TEXT PRIMARY KEY,
  title           TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'new'
                  CHECK (status IN ('new','in_workflow','human_feedback','done','cancelled')),
  priority        INTEGER NOT NULL DEFAULT 2
                  CHECK (priority BETWEEN 0 AND 3),
  author_id       TEXT REFERENCES agents(id),
  workflow_id     TEXT REFERENCES workflows(id),
  current_step_id TEXT REFERENCES steps(id),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  -- The closest SQL can express to the §5.1 invariant: status='in_workflow'
  -- iff a step is set. Other invariants live in app code.
  CHECK (
    (status =  'in_workflow' AND current_step_id IS NOT NULL)
    OR
    (status <> 'in_workflow' AND (current_step_id IS NULL OR status = 'human_feedback'))
  )
);

CREATE INDEX idx_tasks_status_prio ON tasks(status, priority, created_at);
CREATE INDEX idx_tasks_step        ON tasks(current_step_id);

CREATE TABLE task_deps (
  blocker_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  blocked_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL DEFAULT 'blocks' CHECK (kind = 'blocks'),
  PRIMARY KEY (blocker_id, blocked_id, kind),
  CHECK (blocker_id <> blocked_id)
);

CREATE INDEX idx_deps_blocked ON task_deps(blocked_id);

CREATE TABLE comments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  author_id  TEXT NOT NULL REFERENCES agents(id),
  text       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX idx_comments_task ON comments(task_id, created_at);
