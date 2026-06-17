// state/selectors.ts — derived views over the normalized store. The session
// transcript itself is rendered straight from `state.transcriptBySession`
// (pi-format lines from session.transcript / session-event); these selectors
// cover the sidebar lists, the selected entity, and the entity-driven composer
// mode.

import type { SessionMeta, TaskView, WorkflowInfo } from "@/types";
import type { AppState, ProjectSlice } from "./types";
import { emptyProjectSlice } from "./types";
import { selectedSessionId, selectedTaskId, selectedWorkflowName } from "./selection";

export function activeSlice(state: AppState): ProjectSlice {
  if (!state.activeProject) return emptyProjectSlice();
  return state.byProject[state.activeProject] ?? emptyProjectSlice();
}

export function activeTasks(state: AppState): TaskView[] {
  const slice = activeSlice(state);
  return slice.taskOrder.map((id) => slice.tasks[id]).filter(Boolean);
}

/** The selected task id, or null when the selection is not a task. */
export function activeTaskId(state: AppState): string | null {
  return selectedTaskId(state.selection);
}

export function activeTask(state: AppState): TaskView | null {
  const id = activeTaskId(state);
  if (!id) return null;
  const slice = activeSlice(state);
  return slice.tasks[id] ?? null;
}

/** The session backing the selected session, or null. */
export function selectedSession(state: AppState): SessionMeta | null {
  const id = selectedSessionId(state.selection);
  if (!id) return null;
  return state.sessions[id] ?? null;
}

/** The selected workflow (looked up by name in the active project), or null. */
export function selectedWorkflow(state: AppState): WorkflowInfo | null {
  const name = selectedWorkflowName(state.selection);
  if (!name) return null;
  return activeSlice(state).workflows.find((w) => w.name === name) ?? null;
}

/** All sessions of the active project, newest first (Sessions panel). */
export function sessionsForProject(state: AppState): SessionMeta[] {
  const root = state.activeProject;
  if (!root) return [];
  const order = state.sessionOrderByProject[root] ?? [];
  return order.map((id) => state.sessions[id]).filter(Boolean);
}

/**
 * Tasks sorted by most-recently-updated first (the Tasks panel renders one flat,
 * ungrouped list). `updated_at` is the primary key; `created_at` then `id` break
 * ties so the order is stable across renders.
 */
export function tasksByRecency(tasks: TaskView[]): TaskView[] {
  return tasks.slice().sort((a, b) => {
    const byUpdated = tsMillis(b.updated_at) - tsMillis(a.updated_at);
    if (byUpdated !== 0) return byUpdated;
    const byCreated = tsMillis(b.created_at) - tsMillis(a.created_at);
    if (byCreated !== 0) return byCreated;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

function tsMillis(ts: string | null | undefined, fallback = 0): number {
  if (!ts) return fallback;
  const n = Date.parse(ts);
  return Number.isNaN(n) ? fallback : n;
}

/**
 * The unified composer mode, driven by the SELECTED ENTITY:
 *   - interactive session selected + running/queued → "chat" (a chat composer;
 *     each message is a followup turn, ended via the End button in the header)
 *   - workflow session selected + running/queued     → "steer" (steer + abort;
 *     abort lives in the session header, the composer is just the input)
 *   - session selected + terminal session            → "none" (read-only transcript)
 *   - task selected (any status)                     → "comment" (a single comment box;
 *     enroll/resume/reopen moved to the Enroll button in the task header)
 *   - nothing (or workflow) selected                 → "none"
 */
export type ComposerMode = "chat" | "steer" | "comment" | "none";

export function composerMode(state: AppState): ComposerMode {
  const sel = state.selection;
  if (sel.kind === "session") {
    const session = state.sessions[sel.sessionId];
    if (session && (session.status === "running" || session.status === "queued")) {
      return session.kind === "interactive" ? "chat" : "steer";
    }
    return "none";
  }
  if (sel.kind === "task") {
    return activeSlice(state).tasks[sel.taskId] ? "comment" : "none";
  }
  return "none";
}
