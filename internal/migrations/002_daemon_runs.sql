-- 002_daemon_runs.sql — daemon / pi orchestrator run state.
--
-- See docs/plans/20260517-Daemon-Plan.md §4.1 for the canonical spec.
--
-- Notes:
--   * status enum mirrors plan §6 ({queued, running, done, failed, cancelled}).
--   * thinking enum allows "" for "pi default".
--   * closure_kind is null until terminal success ({done, cancelled, decomposed}).
--   * pre_blocked_by is a comma-separated snapshot of incoming blockers at
--     run start; used to detect new blockers ("decomposed") during
--     verifyClosure.
--   * task_id is nullable so ad-hoc prompts (no autosk task) are supported.
--   * Foreign key uses ON DELETE SET NULL so deleting a task does not
--     destroy historical run rows.

CREATE TABLE daemon_runs (
  job_id           TEXT PRIMARY KEY,
  task_id          TEXT,
  prompt           TEXT NOT NULL,
  model            TEXT NOT NULL DEFAULT '',
  thinking         TEXT NOT NULL DEFAULT ''
                   CHECK (thinking IN ('','off','minimal','low','medium','high','xhigh')),
  cwd              TEXT NOT NULL,
  status           TEXT NOT NULL
                   CHECK (status IN ('queued','running','done','failed','cancelled')),
  exit_code        INTEGER,
  pid              INTEGER,
  pi_session_id    TEXT,
  session_path     TEXT,
  error            TEXT,
  auto_claim       INTEGER NOT NULL DEFAULT 1,
  max_corrections  INTEGER NOT NULL DEFAULT 3,
  corrections_used INTEGER NOT NULL DEFAULT 0,
  closure_kind     TEXT
                   CHECK (closure_kind IS NULL OR closure_kind IN ('done','cancelled','decomposed')),
  pre_blocked_by   TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  started_at       INTEGER,
  finished_at      INTEGER,
  FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL
);

CREATE INDEX idx_daemon_runs_task_id ON daemon_runs(task_id);
CREATE INDEX idx_daemon_runs_status  ON daemon_runs(status, created_at);
CREATE INDEX idx_daemon_runs_created ON daemon_runs(created_at);
