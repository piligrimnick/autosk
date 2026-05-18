-- 003_step_agent_params.sql
--
-- Per-step agent parameter overrides. The column stores a JSON blob
-- mirroring internal/workflow.AgentParams; NULL means "no overrides,
-- use the agent package's defaults verbatim". See docs/workflows.md
-- for merge semantics.

ALTER TABLE steps ADD COLUMN agent_params TEXT;
