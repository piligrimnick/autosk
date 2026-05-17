-- 002_daemon_runs.sql — v0.2 daemon state.
--
-- See docs/plans/20260517-Workflows-Plan.md §4.1 for the canonical spec.
--
-- One row per STEP execution. step_id is always set: even single-agent
-- runs go through the synthetic `single:<agent>` workflow created by
-- internal/workflow.EnsureSingle.
--
-- Everything that used to be derivable per-spawn (prompt, model, thinking,
-- cwd, auto_claim, pre_blocked_by, closure_kind, agent_id) has been
-- removed. On success the engine records which transition the agent took
-- via transition_id; that is the closure signal.

CREATE TABLE daemon_runs (
  job_id           TEXT PRIMARY KEY,
  task_id          TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  step_id          TEXT NOT NULL REFERENCES steps(id) ON DELETE RESTRICT,
  status           TEXT NOT NULL
                   CHECK (status IN ('queued','running','done','failed','cancelled')),
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
-- read by the executor at end-of-turn. PK on run_id enforces "exactly one
-- signal per run"; a second call returns step_next_already_emitted.
CREATE TABLE step_signals (
  run_id        TEXT PRIMARY KEY REFERENCES daemon_runs(job_id) ON DELETE CASCADE,
  task_id       TEXT NOT NULL,
  transition_id INTEGER NOT NULL REFERENCES step_transitions(id),
  created_at    INTEGER NOT NULL
);
