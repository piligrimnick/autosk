/**
 * Pure transition helpers shared by every transition path (plan §3.3).
 *
 * `workflow.onTransit` is called by the engine for EVERY transition — enroll →
 * firstStep, step → step, step → done/cancel/human, resume --to — so the
 * target-validation, position-computation, and `TransitContext` construction
 * live here, used identically by {@link Engine.enroll} / {@link Engine.resume}
 * (no session) and {@link SessionRuntime.transit} (a live session).
 */

import { isStatusStep, type StepTarget, type TaskStatus, type TransitContext, type WorkflowDefinition } from "@autosk/sdk";

import type { Store } from "../store/store.ts";
import { getStepVisits } from "../store/metadata.ts";
import { buildTasksApi } from "./context.ts";

/** The engine-owned position triple a transition commits. */
export interface Position {
  status: TaskStatus;
  workflow: string;
  step: string;
}

/**
 * Validates a transition target against a workflow. A `{ step }` target must
 * name a declared step; a `{ status }` target must be one of the three
 * terminal/park statuses. Throwing here (like `onTransit` throwing) is
 * retryable by the agent — it never consumes the session's single transit.
 */
export function validateTarget(wf: WorkflowDefinition, to: StepTarget): void {
  if ("step" in to) {
    if (typeof to.step !== "string" || !(to.step in wf.steps)) {
      throw new Error(`transit: unknown step ${JSON.stringify(to.step)} in workflow ${wf.name}`);
    }
    return;
  }
  if ("status" in to) {
    if (to.status !== "done" && to.status !== "cancel" && to.status !== "human") {
      throw new Error(`transit: invalid status target ${JSON.stringify(to.status)}`);
    }
    return;
  }
  throw new Error("transit: target must be { step } or { status }");
}

/**
 * The position a target commits to (plan §3.3, step 7).
 *
 *  - `{ step }` into an agent step → `work` at that step (the scheduler picks it
 *    up); into a `statusStep` → that step's status at that step (the engine
 *    moves the task: `human` parks it — never scheduled, `resume` re-enters —
 *    while `done`/`cancel` close it on that step). This is what "transit into a
 *    statusStep moves the task to its status" means.
 *  - `{ status }` → flips status but KEEPS the workflow + the step the task was at
 *    (so a parked `human` task can resume the same step, and a closed task shows
 *    where it ended). `currentStep` is the step being left.
 */
export function positionFor(wf: WorkflowDefinition, currentStep: string, to: StepTarget): Position {
  if ("step" in to) {
    const step = wf.steps[to.step];
    const status: TaskStatus = step && isStatusStep(step) ? step.status : "work";
    return { status, workflow: wf.name, step: to.step };
  }
  return { status: to.status, workflow: wf.name, step: currentStep };
}

/** Inputs for {@link buildTransitContext}. */
export interface TransitContextInput {
  store: Store;
  taskId: string;
  /** The workflow governing the transition. */
  workflow: string;
  /** The step being left (`""` on enroll, where there is no current step). */
  leavingStep: string;
  /** Author for `ctx.comment` (the engine records workflow-driven comments as `autosk`). */
  author: string;
}

/**
 * Builds the {@link TransitContext} handed to `workflow.onTransit`. `visits(s)`
 * reads the engine-maintained `metadata.step_visits[s]` counter (plan §5) — the
 * persistent, human-resettable answer to the common `max_visits` guard
 * (plan §3.3, §3.6). The counter is bumped INSIDE the position write of a
 * transition into a named step (`setPosition(..., { countVisit })`), which
 * happens AFTER `onTransit` is consulted; so at the moment of a transition INTO
 * step `s`, `visits(s)` still reflects PRIOR entries (a self-loop's current
 * occupancy was counted when it was entered, exactly as before).
 */
export function buildTransitContext(input: TransitContextInput): TransitContext {
  const { store, taskId, workflow, leavingStep, author } = input;
  return {
    taskId,
    workflow,
    step: leavingStep,
    visits: (s) => getStepVisits(store.peekMetadata(taskId))[s] ?? 0,
    tasks: buildTasksApi(store, taskId),
    comment: (text) => store.addComment(taskId, { author, text }).then(() => {}),
  };
}
