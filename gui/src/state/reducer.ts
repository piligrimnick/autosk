// state/reducer.ts — the slice-composed reducer (plan §6). `rootReducer`
// dispatches each action through every slice; each slice owns a keyed sub-tree
// (projects / tasks / jobs / timeline / ui) and returns the (possibly) updated
// state. Slices are pure functions of (state, action).

import type { Action, AppState, ProjectSlice } from "./types";
import { emptyExtras, emptyProjectSlice } from "./types";

function patchProject(
  state: AppState,
  root: string,
  patch: (slice: ProjectSlice) => ProjectSlice,
): AppState {
  const cur = state.byProject[root] ?? emptyProjectSlice();
  return { ...state, byProject: { ...state.byProject, [root]: patch(cur) } };
}

/** Connection / settings / notice. */
function uiSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "settings/loaded":
      return { ...state, settings: action.settings };
    case "daemon/status":
      return { ...state, daemon: action.status };
    case "notice/set":
      return { ...state, notice: action.notice };
    case "view/set":
      return { ...state, view: action.view };
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
      return { ...state, projects: action.projects, projectsLoaded: true, byProject, activeProject };
    }
    case "project/select":
      return { ...state, activeProject: action.root, activeTaskId: null };
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

/** Task selection + single-task upserts + per-task extras. */
function tasksSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "task/select":
      return { ...state, activeTaskId: action.id };
    case "task/upserted": {
      const next = patchProject(state, action.root, (s) => {
        const exists = s.tasks[action.task.id] !== undefined;
        return {
          ...s,
          tasks: { ...s.tasks, [action.task.id]: action.task },
          taskOrder: exists ? s.taskOrder : [action.task.id, ...s.taskOrder],
        };
      });
      return next;
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

/** Normalized job map. */
function jobsSlice(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "jobs/upsertMany": {
      const jobs = { ...state.jobs };
      for (const j of action.jobs) {
        jobs[j.job_id] = j;
      }
      return { ...state, jobs };
    }
    case "job/upsert":
      return { ...state, jobs: { ...state.jobs, [action.job.job_id]: action.job } };
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

const SLICES = [uiSlice, projectsSlice, tasksSlice, jobsSlice, timelineSlice];

/** Compose every slice: each may transform the state for a given action. */
export function rootReducer(state: AppState, action: Action): AppState {
  return SLICES.reduce((acc, slice) => slice(acc, action), state);
}

/** Convenience: the extras for a task, defaulted. */
export function extrasFor(state: AppState, taskId: string | null) {
  if (!taskId) return emptyExtras();
  return state.extrasByTask[taskId] ?? emptyExtras();
}
