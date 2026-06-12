/**
 * Builders for the live-access slices of an {@link AgentRunContext} (plan §3.4).
 *
 * `tasks` and `workflows` are deliberately *live* — `tasks.current()` re-reads
 * the store rather than handing back a frozen snapshot, and `workflows.get/list`
 * render the in-memory registry — so an agent always sees the current state.
 */

import type { TasksAPI, WorkflowsAPI } from "@autosk/sdk";
import type { StepTarget, WorkflowDefinition } from "@autosk/sdk";

import type { ExtensionRegistry } from "../extensions/registry.ts";
import type { Store } from "../store/store.ts";

/**
 * The declared target set for a step (plan §4): every step in the workflow (the
 * self-loop included, since re-running the same agent is a valid retry) plus the
 * three terminal/park statuses. The exact runtime edges are decided by
 * `onTransit`, so this is the conservative superset — identical to
 * `renderWorkflowInfo`'s per-step `targets`, kept in one shape for both surfaces.
 */
export function declaredTargets(wf: WorkflowDefinition): StepTarget[] {
  const steps: StepTarget[] = Object.keys(wf.steps)
    .sort()
    .map((s) => ({ step: s }));
  return [...steps, { status: "done" }, { status: "cancel" }, { status: "human" }];
}

/** A {@link TasksAPI} bound to one current task id (live reads through the store). */
export function buildTasksApi(store: Store, taskId: string): TasksAPI {
  return {
    currentId: taskId,
    current: () => store.taskView(taskId),
    get: (id) => store.taskView(id),
    list: (filter) => store.listTaskViews(filter),
    comments: (id) => store.listComments(id ?? taskId),
  };
}

/** A {@link WorkflowsAPI} pinned to the session's current position. */
export function buildWorkflowsApi(
  registry: ExtensionRegistry,
  wf: WorkflowDefinition,
  step: string,
): WorkflowsAPI {
  return {
    current: { workflow: wf.name, step, targets: declaredTargets(wf) },
    get: (name) => registry.getWorkflowInfo(name),
    list: () => registry.listWorkflows(),
  };
}
