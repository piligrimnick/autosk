/**
 * Workflow definitions (plan §3.3) and pluggable isolation providers
 * (plan §3.5). Workflows are *code* registered by extensions; the engine only
 * drives the task status machine and calls `onTransit` on every transition.
 */

import type { TasksAPI } from "./agent.ts";

/**
 * The target of a transition: either a sibling step within the same workflow,
 * or a terminal/park status (plan §3.3).
 */
export type StepTarget = { step: string } | { status: "done" | "cancel" | "human" };

/** One step of a workflow (plan §3.3). */
export interface StepDef {
  /** Agent name from the registry. Absent on a `human` step. */
  agent?: string;
  /** Human-owned step: the engine parks the task and never schedules an agent. */
  human?: boolean;
}

/**
 * Context passed to `WorkflowDefinition.onTransit`. The engine knows nothing
 * about graphs, visit caps, or guards — a workflow implements those here,
 * counting in its own state or in comments (plan §3.3). `visits(step)` is the
 * convenience the engine offers for the common `max_visits` pattern shown in
 * plan §3.6.
 */
export interface TransitContext {
  taskId: string;
  /** The workflow name this transition belongs to. */
  workflow: string;
  /** The step the task is leaving (the current step before the transition). */
  step: string;
  /** How many times the task has entered `step` so far (this run included). */
  visits(step: string): number;
  /** Live task access (re-reads from the store). */
  tasks: TasksAPI;
  /** Shorthand to comment on the transitioning task. */
  comment(text: string): Promise<void>;
}

/**
 * A workflow definition (plan §3.3). `onTransit` is called by the engine for
 * EVERY transition (enroll → firstStep, step → step, step → terminal, resume).
 * Throwing or returning a rejected promise rejects the transition. A default
 * (absent) `onTransit` allows everything.
 */
export interface WorkflowDefinition {
  /** Unique within a project's registry. */
  name: string;
  description?: string;
  firstStep: string;
  /** Step name → definition. */
  steps: Record<string, StepDef>;
  onTransit?(ctx: TransitContext, to: StepTarget): void | Promise<void>;
  /** Optional pluggable isolation module (plan §3.5). */
  isolation?: IsolationProvider;
}

/** A handle to an acquired isolation environment (plan §3.5). */
export interface IsolationHandle {
  /** The working directory the session runs in (passed as `ctx.cwd`). */
  cwd: string;
  /** Provider-internal bookkeeping (e.g. branch name for worktrees). */
  meta?: Record<string, unknown>;
}

/**
 * A pluggable isolation provider attached to a workflow (sandcastle pattern,
 * plan §3.5). The engine calls `acquire` before scheduling a session and
 * `release` on transitions (`terminal=true` on done/cancel).
 */
export interface IsolationProvider {
  /** `"worktree"` | `"none"` | future: `"docker"`, … */
  tag: string;
  acquire(ctx: { projectRoot: string; taskId: string }): Promise<IsolationHandle>;
  release(handle: IsolationHandle, opts: { terminal: boolean }): Promise<void>;
}
