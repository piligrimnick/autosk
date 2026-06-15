/**
 * Workflow definitions (plan §3.3) and pluggable isolation providers
 * (plan §3.5). Workflows are *code* registered by extensions; the engine only
 * drives the task status machine and calls `onTransit` on every transition.
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
 * The outcome of a session-free {@link IsolationProvider.reap}. Lets the caller
 * distinguish "removed the env" from "left it in place because it was dirty and
 * `force` was not set" so it can warn the operator instead of silently dropping
 * uncommitted work.
 */
export interface IsolationReapResult {
  /** Whether an isolation env existed for the identity and was removed. */
  removed: boolean;
  /** True when removal was SKIPPED because the env had uncommitted changes (and `force` was false). */
  dirty: boolean;
  /** Optional human-readable detail (e.g. `"3 uncommitted file(s)"`). */
  detail?: string;
}

/**
 * A pluggable isolation provider attached to a workflow (sandcastle pattern,
 * plan §3.5). Isolation is scoped to a task's ACTIVE RUN (the contiguous time it
 * spends in `work`) and driven by the task's status machine, not by step-session
 * boundaries. The environment moves through a small state machine:
 *
 * ```text
 *  ABSENT ──acquire(create)──▶ RUNNING ──release(stop)──▶ DORMANT
 *                                ▲   │                        │
 *                                │   └──acquire(reuse)         │
 *                                └────acquire(resume/start)────┘
 *                             (any) ──reap(force)──▶ GONE
 * ```
 *
 * The three methods map to three distinct roles:
 *  - **`acquire` = ensure-ready** (create | start | reuse). Mandatory, called on
 *    entering `work` (enroll / resume) and re-entered per step; MUST be
 *    idempotent and recovery-safe.
 *  - **`release` = quiesce-on-exit**. Optional; called only when the task LEAVES
 *    `work` (`human` park, or `done`/`cancel`). Stops a live env but keeps it
 *    cheaply resumable. NEVER fires on step→step.
 *  - **`reap` = destroy-on-terminal**. Optional; called only on a TERMINAL
 *    transition (`done`/`cancel`), keyed by identity so it works with no live
 *    handle. `force:false` refuses to discard uncommitted changes, `force:true`
 *    removes regardless (branches created by the env are always preserved).
 */
export interface IsolationProvider {
  /** `"worktree"` | `"none"` | future: `"docker"`, … */
  tag: string;
  /**
   * Ensure the environment for `(projectRoot, taskId)` exists AND is ready to
   * run, returning the handle the session runs in. MUST be idempotent and
   * recovery-safe: create when ABSENT, resume when DORMANT, re-use when RUNNING.
   * Called on entering `work` (enroll / resume) and re-entered per step.
   */
  acquire(ctx: { projectRoot: string; taskId: string }): Promise<IsolationHandle>;
  /**
   * Quiesce a LIVE environment when the task LEAVES `work` (park or terminal):
   * stop it but keep it cheaply resumable by a later `acquire`. NO destruction
   * happens here (that is `reap`), so there is no `terminal`/`force`. NEVER
   * called on step→step. Optional — a provider with nothing to stop (e.g. the
   * worktree provider, where keeping the dir IS the absence of teardown) omits
   * it.
   */
  release?(handle: IsolationHandle): Promise<void>;
  /**
   * Destroy the durable artifacts for `(projectRoot, taskId)` on a TERMINAL
   * transition (`done`/`cancel`), keyed by the deterministic identity rather
   * than a live handle so it works with no in-memory handle (the engine path on
   * a workflow-driven terminal, a manual `task.done`/`task.cancel` after a park,
   * or crash recovery). With `force:false` a dirty env is left in place and
   * reported via {@link IsolationReapResult.dirty}; with `force:true` it is
   * removed regardless (branches are always preserved). Optional: providers with
   * no out-of-band identity may omit it (the caller then skips reaping).
   */
  reap?(ctx: { projectRoot: string; taskId: string }, opts: { force: boolean }): Promise<IsolationReapResult>;
}
