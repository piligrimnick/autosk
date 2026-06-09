// state/reducer.ts — the slice-composed reducer (redesign plan §6.2).
// `rootReducer` dispatches each action through every slice; each slice owns a
// keyed sub-tree (ui / projects / selection / tasks / jobs / sessions /
// timeline) and returns the (possibly) updated state. Slices are pure functions
// of (state, action). An action may be handled by more than one slice (e.g.
// `task/extrasLoaded` updates both tasks and the normalized job map).

import type { Action, AppState, ProjectSlice, SidebarPanel } from "./types";
import { clampSidebarWidth, emptyExtras, emptyProjectSlice } from "./types";
import { NO_SELECTION } from "./selection";

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
    default:
      return state;
  }
}

/** Project registry + per-project task/meta loads. */
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
        agents: action.agents,
        metaLoaded: true,
      }));
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

/** Normalized job map + the per-project session order (Sessions panel). */
function jobsSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "jobs/upsertMany": {
      const jobs = { ...state.jobs };
      for (const j of action.jobs) {
        jobs[j.job_id] = j;
      }
      return { ...state, jobs };
    }
    case "job/upsert": {
      const jobs = { ...state.jobs, [action.job.job_id]: action.job };
      // A freshly-spawned run shows up at the top of the active project's
      // Sessions panel live (the daemon only pushes job-events for the active
      // project's jobs).
      let sessionOrderByProject = state.sessionOrderByProject;
      const root = state.activeProject;
      if (root) {
        const order = sessionOrderByProject[root] ?? [];
        if (!order.includes(action.job.job_id)) {
          sessionOrderByProject = { ...sessionOrderByProject, [root]: [action.job.job_id, ...order] };
        }
      }
      return { ...state, jobs, sessionOrderByProject };
    }
    case "task/extrasLoaded": {
      // Mirror the task's jobs into the normalized map so timeline lookups and
      // status frames share one source of truth.
      const jobs = { ...state.jobs };
      for (const j of action.extras.jobs) {
        jobs[j.job_id] = j;
      }
      return { ...state, jobs };
    }
    default:
      return state;
  }
}

/** Per-project session order (job.list snapshot) for the Sessions panel. */
function sessionsSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "sessions/loaded": {
      const jobs = { ...state.jobs };
      for (const j of action.jobs) {
        jobs[j.job_id] = j;
      }
      const order = action.jobs
        .slice()
        .sort((a, b) => Date.parse(b.created_at || "") - Date.parse(a.created_at || ""))
        .map((j) => j.job_id);
      return {
        ...state,
        jobs,
        sessionOrderByProject: { ...state.sessionOrderByProject, [action.root]: order },
      };
    }
    default:
      return state;
  }
}

/** Live transcript tail (keyed by job id), deduped by event_id. */
function timelineSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "job/subscribed":
      return { ...state, subscribedJob: action.jobId };
    case "job/messagesReset":
      return {
        ...state,
        messagesByJob: { ...state.messagesByJob, [action.jobId]: action.messages },
        seenEventId: { ...state.seenEventId, [action.jobId]: 0 },
      };
    case "job/messageAppended": {
      const seen = state.seenEventId[action.jobId] ?? 0;
      if (action.eventId !== 0 && action.eventId <= seen) {
        return state; // replayed frame already applied
      }
      const cur = state.messagesByJob[action.jobId] ?? [];
      return {
        ...state,
        messagesByJob: { ...state.messagesByJob, [action.jobId]: [...cur, action.message] },
        seenEventId: { ...state.seenEventId, [action.jobId]: Math.max(seen, action.eventId) },
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
  jobsSlice,
  sessionsSlice,
  timelineSlice,
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
