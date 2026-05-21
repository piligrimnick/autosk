-- autosk-migration: foreign_keys_off
--
-- Rename every legacy `as-XXXX` task id to `ask-00XXXX`.
--
-- FK enforcement is off (leading directive) so we can UPDATE parents
-- and children separately without ON UPDATE CASCADE. PRAGMA
-- foreign_key_check fires inside the transaction before COMMIT.
--
-- The two-zero left-pad on the hex suffix makes the mapping reversible
-- and keeps the migrated namespace disjoint from freshly minted random
-- ids (which use 3 bytes / 6 hex chars).

UPDATE tasks
   SET id = 'ask-00' || substr(id, 4)
 WHERE id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]';

UPDATE task_deps
   SET blocker_id = 'ask-00' || substr(blocker_id, 4)
 WHERE blocker_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]';

UPDATE task_deps
   SET blocked_id = 'ask-00' || substr(blocked_id, 4)
 WHERE blocked_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]';

UPDATE comments
   SET task_id = 'ask-00' || substr(task_id, 4)
 WHERE task_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]';

UPDATE daemon_runs
   SET task_id = 'ask-00' || substr(task_id, 4)
 WHERE task_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]';

UPDATE step_signals
   SET task_id = 'ask-00' || substr(task_id, 4)
 WHERE task_id GLOB 'as-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]';
