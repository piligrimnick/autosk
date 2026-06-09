// state/selection.ts — the unified entity-selection model (redesign plan §3).
// Replaces the old top-level `view` + standalone `activeTaskId`. A "session" is
// currently backed by a Job; the kind is named generically so a future
// task-less interactive session slots in without another state rewrite.

export type Selection =
  | { kind: "none" }
  | { kind: "task"; taskId: string }
  | { kind: "session"; jobId: string }
  | { kind: "workflow"; name: string };

export const NO_SELECTION: Selection = { kind: "none" };

export function selectedTaskId(sel: Selection): string | null {
  return sel.kind === "task" ? sel.taskId : null;
}

export function selectedSessionJobId(sel: Selection): string | null {
  return sel.kind === "session" ? sel.jobId : null;
}

export function selectedWorkflowName(sel: Selection): string | null {
  return sel.kind === "workflow" ? sel.name : null;
}
