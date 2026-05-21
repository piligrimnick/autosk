-- 004_step_max_visits_and_task_metadata.sql
--
-- Per-step `max_visits` cap and per-task `metadata` JSON blob.
--
-- 0 (the column default) means "unlimited" — preserving prior
-- behaviour for every existing step row. The default also means
-- this migration is safe to apply on an existing database without
-- backfilling: every prior step keeps its uncapped behaviour.
--
-- `metadata` is a free-form JSON object stored as TEXT (nullable).
-- NULL is equivalent to `{}` for callers; the engine reserves the
-- top-level key `step_visits` for per-step counters keyed by step_id.

ALTER TABLE steps ADD COLUMN max_visits INTEGER NOT NULL DEFAULT 0
       CHECK (max_visits >= 0);

ALTER TABLE tasks ADD COLUMN metadata TEXT;
