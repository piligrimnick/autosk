-- 001_init.sql — autosk v0.1 release schema.
--
-- This is the consolidated schema for autosk's first tagged release.
-- During pre-release development the schema evolved through a chain of
-- seven migrations (workflow plumbing, daemon state, step parameters,
-- step visit limits, workflow isolation, short status enums, ask-XXXXXX
-- task id format). The autosk repository itself was the only consumer
-- of the intermediate shapes, so the chain was squashed into this
-- single file before tagging. Any future schema change ships as a new
-- numbered migration (002, 003, …) on top of this base.
--
-- The `human` row in the `agents` table is seeded just after this
-- migration runs — see migrations.go SeedHumanAgent. The chicken-and-
-- egg dance for the schema_migrations tracking table itself lives in
-- migrations.EnsureTrackingTable.

------------------------------------------------------------------------
-- Agents
------------------------------------------------------------------------
--
-- The agents row identity is a small id (`ag-XXXX`); `name` is the
-- user-visible handle (and what workflows reference). `is_human=1`
-- marks the canonical `human` agent that owns hand-off tasks.

CREATE TABLE agents (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  is_human   INTEGER NOT NULL DEFAULT 0
             CHECK (is_human IN (0,1)),
  created_at INTEGER NOT NULL
);

------------------------------------------------------------------------
-- Workflows + steps + transitions
------------------------------------------------------------------------
--
-- `isolation` controls how the daemon spawns step runs:
--   'none'     — cwd = projectRoot (default, current behaviour).
--   'worktree' — the CLI allocates a git worktree per task at enroll
--                time and the daemon spawns step runs inside it.
-- Future modes (e.g. 'container') will ALTER the CHECK, not introduce
-- a parallel column.

CREATE TABLE workflows (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  description   TEXT NOT NULL DEFAULT '',
  first_step_id TEXT NOT NULL,
  is_synthetic  INTEGER NOT NULL DEFAULT 0
                CHECK (is_synthetic IN (0,1)),
  isolation     TEXT NOT NULL DEFAULT 'none'
                CHECK (isolation IN ('none','worktree')),
  created_at    INTEGER NOT NULL
);

-- `agent_params` is a JSON blob mirroring internal/workflow.AgentParams;
-- NULL means "no overrides, use the agent package's defaults verbatim".
-- See docs/workflows.md for merge semantics.
--
-- `max_visits` caps how many times a single task may enter this step.
-- 0 (the default) means "unlimited".

CREATE TABLE steps (
  id           TEXT PRIMARY KEY,
  workflow_id  TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  agent_id     TEXT NOT NULL REFERENCES agents(id),
  seq          INTEGER NOT NULL,        -- source-file order within the workflow
  agent_params TEXT,
  max_visits   INTEGER NOT NULL DEFAULT 0
               CHECK (max_visits >= 0),
  UNIQUE (workflow_id, name)
);

-- Transitions are write-only from the engine's point of view; users
-- never reference the integer id directly. Each row is either a
-- sibling-step transition (next_step_id set, task_status NULL) or a
-- closure transition (task_status set, next_step_id NULL).

CREATE TABLE step_transitions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  step_id      TEXT NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
  next_step_id TEXT REFERENCES steps(id) ON DELETE CASCADE,
  task_status  TEXT,
  prompt_rule  TEXT NOT NULL,
  CHECK ((next_step_id IS NULL) <> (task_status IS NULL)),
  CHECK (task_status IS NULL OR task_status IN ('human','done','cancel'))
);
CREATE INDEX idx_step_transitions_step ON step_transitions(step_id);

------------------------------------------------------------------------
-- Tasks
------------------------------------------------------------------------
--
-- Task ids are minted as `ask-XXXXXX` (6 hex chars). `metadata` is a
-- free-form JSON object stored as TEXT (nullable); NULL is equivalent
-- to `{}` for callers. The engine reserves the top-level key
-- `step_visits` for per-step visit counters keyed by step_id.

CREATE TABLE tasks (
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
  -- The closest SQL can express to the workflow §5.1 invariant:
  -- status='work' iff a step is set; 'human' may also carry a step
  -- pointer (a step that paused for human feedback). Other invariants
  -- live in app code.
  CHECK (
    (status =  'work' AND current_step_id IS NOT NULL)
    OR
    (status <> 'work' AND (current_step_id IS NULL OR status = 'human'))
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

------------------------------------------------------------------------
-- Daemon state
------------------------------------------------------------------------
--
-- One row per STEP execution. step_id is always set: even single-agent
-- runs go through the synthetic `single:<agent>` workflow created by
-- internal/workflow.EnsureSingle. On success the engine records which
-- transition the agent took via transition_id; that is the closure
-- signal.

CREATE TABLE daemon_runs (
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
);

CREATE INDEX idx_runs_task   ON daemon_runs(task_id, created_at);
CREATE INDEX idx_runs_status ON daemon_runs(status, created_at);

-- Staging table. Written by `autosk step next` (the pi-extension tool),
-- read by the executor at end-of-turn. PK on run_id enforces "exactly
-- one signal per run"; a second call returns step_next_already_emitted.
CREATE TABLE step_signals (
  run_id        TEXT PRIMARY KEY REFERENCES daemon_runs(job_id) ON DELETE CASCADE,
  task_id       TEXT NOT NULL,
  transition_id INTEGER NOT NULL REFERENCES step_transitions(id),
  created_at    INTEGER NOT NULL
);
