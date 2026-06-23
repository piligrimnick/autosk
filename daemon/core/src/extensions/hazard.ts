/**
 * The live-code hazard guard (plan §3.6, step 5).
 *
 * Workflows and agents are now *code*. If an operator edits an extension so a
 * workflow (or one of its steps) disappears while a task is mid-flight, the
 * task's stored `(workflow, step)` can point at code that no longer exists.
 * There are no frozen copies and no versioning — the registry at (re)load time
 * is the truth.
 *
 * So on every project (re)open, after the registry is built, validate every
 * `work`/`human` task against it: an unknown workflow or an unknown step ⇒ park
 * the task to `human` with `error="workflow_missing: …"`, written via the P2
 * store (status flip + an `autosk`-authored comment recording the reason).
 *
 * Only `work` tasks are actively re-parked. A task already at `human` is not
 * scheduled, so re-parking it would only spam a duplicate comment on every
 * reload; it is left untouched. A valid task is never touched.
 *
 * A `work` task with a null `workflow` or `step` is also parked: an enrolled
 * in-flight task should always carry both, so a null pair is a structural
 * inconsistency (typically an external hand-edit that flipped `status` to `work`
 * without enrolling). The scheduler can never pick it up (no step ⇒ nothing to
 * run), so without this it would stall in invisible limbo — parking it surfaces
 * the bad edit to the operator.
 */

import type { Store } from "../store/store.ts";
import type { ExtensionRegistry } from "./registry.ts";

/** The comment author recorded for an engine-driven park. */
export const HAZARD_COMMENT_AUTHOR = "autosk";

/** One task parked by the hazard guard. */
export interface ParkedTask {
  taskId: string;
  /** The workflow the task pointed at when parked (`null` if it had none). */
  workflow: string | null;
  /** The step the task pointed at when parked (`null` if it had none). */
  step: string | null;
  /** The `workflow_missing: …` reason. */
  error: string;
}

/**
 * Validates every in-flight (`work`/`human`) task in `store` against `registry`
 * and parks the invalid `work` ones to `human`. Returns the tasks it parked.
 *
 * The status flip keeps the task's `workflow`/`step` so the operator can see
 * what it pointed at; the reason is recorded as a comment.
 */
export async function validateInFlightTasks(
  store: Store,
  registry: ExtensionRegistry,
  opts: { author?: string } = {},
): Promise<ParkedTask[]> {
  const author = opts.author ?? HAZARD_COMMENT_AUTHOR;
  const parked: ParkedTask[] = [];

  const views = await store.listTaskViews();
  for (const view of views) {
    if (view.status !== "work" && view.status !== "human") continue;

    // Compute the park reason, if any. A null workflow/step on an in-flight task
    // is itself a (structural) inconsistency; otherwise validate against the
    // live registry.
    let error: string | null;
    if (view.workflow === null || view.step === null) {
      error = "workflow_missing: enrolled task has no workflow/step";
    } else {
      const result = registry.validatePosition(view.workflow, view.step);
      error = result.ok ? null : (result.error ?? `workflow_missing: ${view.workflow}`);
    }
    if (error === null) continue;

    // Only park what the scheduler could actually pick up. A `human` task is
    // already parked; re-commenting on every reload would be noise.
    if (view.status !== "work") continue;

    await store.setPosition(view.id, {
      status: "human",
      workflow: view.workflow,
      step: view.step,
    });
    await store.addComment(view.id, { author, text: error });
    parked.push({ taskId: view.id, workflow: view.workflow, step: view.step, error });
  }

  return parked;
}
