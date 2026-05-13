-- 001_init.sql — initial schema for autosk v0.1.
--
-- See docs/plans/20260513-Init-Plan.md §5 for the canonical spec.

CREATE TABLE tasks (
  id          TEXT PRIMARY KEY,
  title       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'new'
              CHECK (status IN ('new','claimed','done','cancelled')),
  priority    INTEGER NOT NULL DEFAULT 2
              CHECK (priority BETWEEN 0 AND 3),
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE INDEX idx_tasks_status_prio ON tasks(status, priority, created_at);

CREATE TABLE task_deps (
  blocker_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  blocked_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL DEFAULT 'blocks' CHECK (kind = 'blocks'),
  PRIMARY KEY (blocker_id, blocked_id, kind),
  CHECK (blocker_id <> blocked_id)
);

CREATE INDEX idx_deps_blocked ON task_deps(blocked_id);
