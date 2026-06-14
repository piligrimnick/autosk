// state/reducer.ts — the slice-composed reducer (redesign plan §6.2).
// `rootReducer` dispatches each action through every slice; each slice owns a
// keyed sub-tree (ui / projects / selection / tasks / sessions / transcript)
// and returns the (possibly) updated state. Slices are pure functions of
// (state, action). An action may be handled by more than one slice (e.g.
// `task/extrasLoaded` updates both tasks and the normalized session map).

import type { Action, AppState, ProjectSlice, SidebarPanel } from "./types";
import { clampSidebarWidth, emptyExtras, emptyProjectSlice } from "./types";
import { NO_SELECTION } from "./selection";
import { clampUiScale } from "@/features/layout/utils/uiScale";

function patchProject(
  state: AppState,
  root: string,
  patch: (slice: ProjectSlice) => ProjectSlice,
): AppState {
  const cur = state.byProject[root] ?? emptyProjectSlice();
  return { ...state, byProject: { ...state.byProject, [root]: patch(cur) } };
}

/** Connection / settings / notice / modal. */
function uiSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "settings/loaded":
      return { ...state, settings: action.settings };
    case "daemon/status":
      return { ...state, daemon: action.status };
    case "notice/set":
      return { ...state, notice: action.notice };
    case "ui/modal":
      return { ...state, ui: { ...state.ui, modal: action.modal } };
    case "ui/sidebarPanel":
      return { ...state, ui: { ...state.ui, sidebarPanel: action.panel } };
    case "ui/sidebarToggle":
      return { ...state, ui: { ...state.ui, sidebarCollapsed: !state.ui.sidebarCollapsed } };
    case "ui/sidebarSetCollapsed":
      return { ...state, ui: { ...state.ui, sidebarCollapsed: action.collapsed } };
    case "ui/sidebarWidth":
      return { ...state, ui: { ...state.ui, sidebarWidth: clampSidebarWidth(action.width) } };
    case "ui/uiScale":
      return { ...state, ui: { ...state.ui, uiScale: clampUiScale(action.scale) } };
    default:
      return state;
  }
}

/** Project registry + per-project task/meta/diagnostics loads. */
function projectsSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "projects/loaded": {
      // Drop slices for projects no longer in the registry; keep the rest.
      const roots = new Set(action.projects.map((p) => p.root));
      const byProject: AppState["byProject"] = {};
      for (const [root, slice] of Object.entries(state.byProject)) {
        if (roots.has(root)) {
          byProject[root] = slice;
        }
      }
      let activeProject = state.activeProject;
      if (activeProject && !roots.has(activeProject)) {
        activeProject = null;
      }
      if (!activeProject && action.projects.length > 0) {
        activeProject = action.projects[0].root;
      }
      // The selection is scoped to the active project; drop it if the active
      // project changed (removed, or first auto-select).
      const selection = activeProject === state.activeProject ? state.selection : NO_SELECTION;
      return { ...state, projects: action.projects, projectsLoaded: true, byProject, activeProject, selection };
    }
    case "project/select":
      return { ...state, activeProject: action.root, selection: NO_SELECTION };
    case "project/tasksLoading":
      return patchProject(state, action.root, (s) => ({ ...s, loading: true, error: null }));
    case "project/tasksLoaded": {
      const tasks: ProjectSlice["tasks"] = {};
      const taskOrder: string[] = [];
      for (const t of action.tasks) {
        tasks[t.id] = t;
        taskOrder.push(t.id);
      }
      return patchProject(state, action.root, (s) => ({
        ...s,
        tasks,
        taskOrder,
        tasksLoaded: true,
        loading: false,
        error: null,
      }));
    }
    case "project/metaLoaded":
      return patchProject(state, action.root, (s) => ({
        ...s,
        workflows: action.workflows,
        metaLoaded: true,
      }));
    case "project/diagnosticsLoaded":
      return patchProject(state, action.root, (s) => ({ ...s, diagnostics: action.diagnostics }));
    case "project/error":
      return patchProject(state, action.root, (s) => ({ ...s, loading: false, error: action.error }));
    default:
      return state;
  }
}

/** Unified entity selection. Selecting an entity also auto-expands the matching
 * sidebar accordion panel (task→tasks, session→sessions, workflow→workflows);
 * an empty selection leaves the active panel as-is. */
function selectionSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "selection/set": {
      const sel = action.selection;
      const panel: SidebarPanel | null =
        sel.kind === "task"
          ? "tasks"
          : sel.kind === "session"
            ? "sessions"
            : sel.kind === "workflow"
              ? "workflows"
              : null;
      const ui = panel ? { ...state.ui, sidebarPanel: panel } : state.ui;
      return { ...state, selection: sel, ui };
    }
    default:
      return state;
  }
}

/** Single-task upserts + per-task extras. */
function tasksSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "task/upserted": {
      return patchProject(state, action.root, (s) => {
        const exists = s.tasks[action.task.id] !== undefined;
        return {
          ...s,
          tasks: { ...s.tasks, [action.task.id]: action.task },
          taskOrder: exists ? s.taskOrder : [action.task.id, ...s.taskOrder],
        };
      });
    }
    case "task/extrasLoaded":
      return {
        ...state,
        extrasByTask: {
          ...state.extrasByTask,
          [action.taskId]: { ...action.extras, loaded: true },
        },
      };
    default:
      return state;
  }
}

/** Normalized session map + the per-project session order (Sessions panel). */
function sessionsSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "session/upsert": {
      const sessions = { ...state.sessions, [action.session.id]: action.session };
      // A freshly-spawned run shows up at the top of the active project's
      // Sessions panel live (the daemon only pushes session-events for the
      // active project's sessions).
      let sessionOrderByProject = state.sessionOrderByProject;
      const root = state.activeProject;
      if (root) {
        const order = sessionOrderByProject[root] ?? [];
        if (!order.includes(action.session.id)) {
          sessionOrderByProject = { ...sessionOrderByProject, [root]: [action.session.id, ...order] };
        }
      }
      return { ...state, sessions, sessionOrderByProject };
    }
    case "sessions/loaded": {
      const sessions = { ...state.sessions };
      for (const m of action.sessions) {
        sessions[m.id] = m;
      }
      // Newest first by start time; a queued session (no started_at) just
      // spawned, so float it to the top, and break ties by id (descending) so
      // the order is deterministic (a NaN comparator would be unstable).
      const startMs = (m: { started_at: string | null }) => {
        const t = m.started_at ? Date.parse(m.started_at) : NaN;
        return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
      };
      const order = action.sessions
        .slice()
        .sort((a, b) => startMs(b) - startMs(a) || (a.id < b.id ? 1 : a.id > b.id ? -1 : 0))
        .map((m) => m.id);
      return {
        ...state,
        sessions,
        sessionOrderByProject: { ...state.sessionOrderByProject, [action.root]: order },
      };
    }
    case "task/extrasLoaded": {
      // Mirror the task's sessions into the normalized map so transcript
      // lookups and status frames share one source of truth.
      const sessions = { ...state.sessions };
      for (const m of action.extras.sessions) {
        sessions[m.id] = m;
      }
      return { ...state, sessions };
    }
    default:
      return state;
  }
}

/** Live transcript tail (keyed by session id), deduped by transcript line number. */
function transcriptSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "session/subscribed":
      return { ...state, subscribedSession: action.sessionId };
    case "session/transcriptReset":
      return {
        ...state,
        transcriptBySession: { ...state.transcriptBySession, [action.sessionId]: action.entries },
        // The snapshot covers up to `nextLine - 1`; tail from there.
        seenLineBySession: { ...state.seenLineBySession, [action.sessionId]: Math.max(0, action.nextLine - 1) },
      };
    case "session/transcriptAppended": {
      const seen = state.seenLineBySession[action.sessionId] ?? 0;
      if (action.line !== 0 && action.line <= seen) {
        return state; // replayed line already applied
      }
      const cur = state.transcriptBySession[action.sessionId] ?? [];
      return {
        ...state,
        transcriptBySession: { ...state.transcriptBySession, [action.sessionId]: [...cur, action.entry] },
        seenLineBySession: { ...state.seenLineBySession, [action.sessionId]: Math.max(seen, action.line) },
      };
    }
    default:
      return state;
  }
}

const SLICES = [
  uiSlice,
  projectsSlice,
  selectionSlice,
  tasksSlice,
  sessionsSlice,
  transcriptSlice,
];

/** Compose every slice: each may transform the state for a given action. */
export function rootReducer(state: AppState, action: Action): AppState {
  return SLICES.reduce((acc, slice) => slice(acc, action), state);
}

/** Convenience: the extras for a task, defaulted. */
export function extrasFor(state: AppState, taskId: string | null) {
  if (!taskId) return emptyExtras();
  return state.extrasByTask[taskId] ?? emptyExtras();
}
