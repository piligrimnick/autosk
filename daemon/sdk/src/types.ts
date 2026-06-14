/**
 * Rendered view types for tasks, comments, sessions, and the registry
 * (plan §3.1, §3.2, §4).
 *
 * These are the shapes clients (CLI / lazy / GUI) and extensions read. They
 * are deliberately flat and derived server-side so a client never has to join
 * by hand. All timestamps are RFC3339 UTC strings on the wire.
 */

import type { StepTarget } from "./workflow.ts";

/** The five-status task enum, unchanged from v1 (plan §3.1). */
export type TaskStatus = "new" | "work" | "human" | "done" | "cancel";

/** A lightweight reference to a related task (dependency edges). */
export interface TaskRef {
  id: string;
  status: TaskStatus;
}

/**
 * The enriched task view (plan §3.1). v2 drops `priority`, `author_id`, and
 * `metadata`. `blocked` and `blocks` are derived server-side and never stored.
 * `workflow` / `step` are `null` until the task is enrolled.
 */
export interface TaskView {
  id: string;
  title: string;
  description: string;
  status: TaskStatus;
  workflow: string | null;
  step: string | null;
  /** Derived: an open blocker exists. */
  blocked: boolean;
  blocked_by: TaskRef[];
  /** Derived: tasks this one blocks. */
  blocks: TaskRef[];
  comment_count: number;
  created_at: string;
  updated_at: string;
}

/**
 * One comment on a task (plan §3.1). Comments are a flat list, editable and
 * deletable (not strictly append-only); the daemon is the sole writer in the
 * normal path.
 */
export interface Comment {
  id: string;
  author: string;
  text: string;
  created_at: string;
  updated_at: string;
}

/** Filter for `tasks.list` / `task.list` (plan §3.4, §4). */
export interface TaskFilter {
  status?: TaskStatus | TaskStatus[];
  workflow?: string;
  step?: string;
  blocked?: boolean;
}

/** Session lifecycle status (plan §3.2). */
export type SessionStatus = "queued" | "running" | "done" | "failed" | "aborted";

/**
 * Session metadata (`./.autosk/sessions/<id>.json`, plan §3.2). Listing a
 * task's sessions = filtering metas by `task_id`.
 */
export interface SessionMeta {
  id: string;
  task_id: string;
  workflow: string;
  step: string;
  agent: string;
  status: SessionStatus;
  error?: string;
  started_at: string | null;
  ended_at: string | null;
}

/** One step of a workflow as rendered from code for `registry.workflow.*`. */
export interface WorkflowStepInfo {
  name: string;
  /**
   * The terminal/park status for a `statusStep` (`done`/`cancel`/`human`), or
   * `null` for an agent step. An agent step's `name` is the agent name.
   */
  status: "done" | "cancel" | "human" | null;
  /**
   * Targets the engine can reach from this step. Because the actual graph is
   * decided at runtime by `onTransit`, this is the conservative declared set (a
   * superset): every step in the workflow — including the step itself, since a
   * self-loop is a valid retry target — plus the terminal/park statuses.
   */
  targets: StepTarget[];
}

/**
 * A workflow rendered from code (plan §4 `registry.workflow.get`). Workflows
 * are code, not data, so this is a projection — not the source of truth.
 */
export interface WorkflowInfo {
  name: string;
  description?: string;
  /** snake_case on the wire (the `WorkflowDefinition.firstStep` projection). */
  first_step: string;
  steps: WorkflowStepInfo[];
  /** Isolation provider tag; `"none"` when the workflow has no provider. */
  isolation: string;
}
