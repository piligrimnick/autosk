-- 005_workflow_isolation.sql
--
-- Per-workflow isolation mode. 'none' (the default) preserves current
-- behaviour: steps spawn with cwd=projectRoot. 'worktree' means the
-- CLI allocates a git worktree per task at enroll time and the daemon
-- spawns step runs inside that worktree.
--
-- The set is intentionally small. Future modes (e.g. 'container') will
-- ALTER the CHECK, not introduce a parallel column.

ALTER TABLE workflows ADD COLUMN isolation TEXT NOT NULL DEFAULT 'none'
       CHECK (isolation IN ('none','worktree'));
