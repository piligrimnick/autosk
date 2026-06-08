// state/types.ts — the normalized store shape + the action union (plan §6
// "State engine"). The reducer is slice-composed (reducer.ts): each slice owns
// a keyed sub-tree (projects / tasks / jobs / timeline / ui). The event router
// (store.tsx) maps `job-event` / `task-changed` / `project-changed` into these
// actions; render flows from this state.

import type {
  Agent,
  AppSettings,
  Comment,
  DaemonStatus,
  Job,
  MessageEvent,
  ProjectInfo,
  Signal,
  TaskView,
  Workflow,
} from "@/types";

/** Per-project slice. Keyed by project root (which is also the RPC `cwd`). */
export interface ProjectSlice {
  /** Normalized task map keyed by task id. */
  tasks: Record<string, TaskView>;
  /** Task id order (creation/update order as returned by task.list). */
  taskOrder: string[];
  workflows: Workflow[];
  agents: Agent[];
  tasksLoaded: boolean;
  metaLoaded: boolean;
  loading: boolean;
  error: string | null;
}

/** Per-task auxiliary data (jobs/comments/signals), keyed by task id. */
export interface TaskExtras {
  jobs: Job[];
  comments: Comment[];
  signals: Signal[];
  loaded: boolean;
}

export type MainView = "tasks" | "workflows" | "agents" | "settings";

/** The whole app state. */
export interface AppState {
  projects: ProjectInfo[];
  projectsLoaded: boolean;
  activeProject: string | null; // project root
  byProject: Record<string, ProjectSlice>;

  activeTaskId: string | null;
  extrasByTask: Record<string, TaskExtras>;

  /** Normalized job map (so status/done frames update one place). */
  jobs: Record<string, Job>;
  /** Live transcript per job (ordered, deduped by event_id). */
  messagesByJob: Record<string, MessageEvent[]>;
  /** Highest event_id seen per job, for replay-then-tail dedup. */
  seenEventId: Record<string, number>;
  /** The job currently subscribed for a live tail (one at a time in v1). */
  subscribedJob: string | null;

  view: MainView;
  daemon: DaemonStatus;
  settings: AppSettings | null;
  /** A transient banner message (errors / confirmations). */
  notice: { kind: "error" | "info"; text: string } | null;
}

export function emptyProjectSlice(): ProjectSlice {
  return {
    tasks: {},
    taskOrder: [],
    workflows: [],
    agents: [],
    tasksLoaded: false,
    metaLoaded: false,
    loading: false,
    error: null,
  };
}

export function emptyExtras(): TaskExtras {
  return { jobs: [], comments: [], signals: [], loaded: false };
}

export function initialState(): AppState {
  return {
    projects: [],
    projectsLoaded: false,
    activeProject: null,
    byProject: {},
    activeTaskId: null,
    extrasByTask: {},
    jobs: {},
    messagesByJob: {},
    seenEventId: {},
    subscribedJob: null,
    view: "tasks",
    daemon: { connected: false, mode: "local" },
    settings: null,
    notice: null,
  };
}

// ---- actions --------------------------------------------------------------

export type Action =
  // bootstrap / connection
  | { type: "settings/loaded"; settings: AppSettings }
  | { type: "daemon/status"; status: DaemonStatus }
  | { type: "notice/set"; notice: AppState["notice"] }
  // projects
  | { type: "projects/loaded"; projects: ProjectInfo[] }
  | { type: "project/select"; root: string | null }
  | { type: "project/tasksLoading"; root: string }
  | { type: "project/tasksLoaded"; root: string; tasks: TaskView[] }
  | { type: "project/metaLoaded"; root: string; workflows: Workflow[]; agents: Agent[] }
  | { type: "project/error"; root: string; error: string }
  // tasks
  | { type: "task/select"; id: string | null }
  | { type: "task/upserted"; root: string; task: TaskView }
  | { type: "task/extrasLoaded"; taskId: string; extras: Omit<TaskExtras, "loaded"> }
  // jobs
  | { type: "jobs/upsertMany"; jobs: Job[] }
  | { type: "job/upsert"; job: Job }
  // timeline / streaming
  | { type: "job/subscribed"; jobId: string | null }
  | { type: "job/messagesReset"; jobId: string; messages: MessageEvent[] }
  | { type: "job/messageAppended"; jobId: string; eventId: number; message: MessageEvent }
  // view
  | { type: "view/set"; view: MainView };
