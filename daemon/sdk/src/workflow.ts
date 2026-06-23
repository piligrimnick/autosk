/**
 * Workflow definitions (plan §3.3). Workflows are *code* registered by
 * extensions; the engine only drives the task status machine and calls
 * `onTransit` on every transition.
 *
 * Isolation is no longer an engine/SDK concern: it left the SDK with the
 * abolition of the `IsolationProvider` abstraction. Agents own the isolation
 * they need via the userspace `@autosk/sandbox` library (a `worktreeSandbox()` /
 * `dockerSandbox()` they wrap their harness with) plus a cleanup workflow step
 * (`sandboxCleanupStep()`). The only related SDK surface is
 * {@link AgentRunContext.newMCPServer} (per-session host-side MCP serving — not
 * isolation).
 */

import type { AgentDefinition, TasksAPI } from "./agent.ts";

/**
 * The target of a transition: either a sibling step within the same workflow,
 * or a terminal/park status (plan §3.3).
 */
export type StepTarget = { step: string } | { status: "done" | "cancel" | "human" };

/**
 * A terminal/park step. Entering a `statusStep` does not schedule an agent; the
 * engine moves the task to that status — `human` parks it (resumable via
 * `task.resume`), `done`/`cancel` close it. Build one with the {@link statusStep}
 * helper.
 */
export interface StatusStep {
  status: "done" | "cancel" | "human";
}

/**
 * One step of a workflow (plan §3.3). Either an inline {@link AgentDefinition}
 * (the step key is the agent name; discriminated by `onRun`) or a
 * {@link StatusStep} (discriminated by `status`).
 */
export type StepDef = AgentDefinition | StatusStep;

/** Narrows a {@link StepDef} to a {@link StatusStep} (a terminal/park step). */
export function isStatusStep(step: StepDef): step is StatusStep {
  return typeof (step as StatusStep).status === "string";
}

/** Narrows a {@link StepDef} to an {@link AgentDefinition} (a runnable step). */
export function isAgentStep(step: StepDef): step is AgentDefinition {
  return typeof (step as AgentDefinition).onRun === "function";
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
}
