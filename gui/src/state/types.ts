// state/types.ts — the normalized store shape + the action union (redesign plan
// §3, §6). The reducer is slice-composed (reducer.ts): each slice owns a keyed
// sub-tree (projects / selection / tasks / sessions / transcript / ui). The
// event router (store.tsx) maps `session-event` / `task-changed` /
// `project-changed` into these actions; render flows from this state.

import type {
  AppSettings,
  Comment,
  DaemonStatus,
  ProjectDiagnostics,
  ProjectInfo,
  SessionMeta,
  TaskView,
  TranscriptLine,
  WorkflowInfo,
} from "@/types";
import type { Selection } from "./selection";
import { NO_SELECTION } from "./selection";
import { UI_SCALE_DEFAULT } from "@/features/layout/utils/uiScale";

/** Per-project slice. Keyed by project root (which is also the RPC `cwd`). */
export interface ProjectSlice {
  /** Normalized task map keyed by task id. */
  tasks: Record<string, TaskView>;
  /** Task id order (creation/update order as returned by task.list). */
  taskOrder: string[];
  workflows: WorkflowInfo[];
  /** Extension load errors for this project (project.diagnostics). */
  diagnostics: ProjectDiagnostics | null;
  tasksLoaded: boolean;
  metaLoaded: boolean;
  loading: boolean;
  error: string | null;
}

/** Per-task auxiliary data (sessions/comments), keyed by task id. */
export interface TaskExtras {
  sessions: SessionMeta[];
  comments: Comment[];
  loaded: boolean;
}

/** Which overlay modal is open (Settings), if any. */
export type ModalKind = "settings" | null;

/** Which sidebar accordion panel is expanded/active (lazygit-style stack). */
export type SidebarPanel = "tasks" | "sessions" | "workflows";

/** Sidebar resize bounds (px). The default matches the `--sidebar-width` token. */
export const SIDEBAR_MIN_WIDTH = 220;
export const SIDEBAR_MAX_WIDTH = 480;
export const SIDEBAR_DEFAULT_WIDTH = 300;

/** Clamp + round a candidate sidebar width to the allowed range. */
export function clampSidebarWidth(width: number): number {
  if (!Number.isFinite(width)) return SIDEBAR_DEFAULT_WIDTH;
  return Math.min(SIDEBAR_MAX_WIDTH, Math.max(SIDEBAR_MIN_WIDTH, Math.round(width)));
}

/** The whole app state. */
export interface AppState {
  projects: ProjectInfo[];
  projectsLoaded: boolean;
  activeProject: string | null; // project root
  byProject: Record<string, ProjectSlice>;

  /** Unified entity selection (replaces `view` + `activeTaskId`). */
  selection: Selection;

  extrasByTask: Record<string, TaskExtras>;

  /** Normalized session map (so status/done frames update one place). */
  sessions: Record<string, SessionMeta>;
  /** Per-project session order (session ids, newest first) for the Sessions panel. */
  sessionOrderByProject: Record<string, string[]>;
  /** Live transcript per session (ordered pi-format lines). */
  transcriptBySession: Record<string, TranscriptLine[]>;
  /** Highest transcript line number applied per session, for replay-then-tail dedup. */
  seenLineBySession: Record<string, number>;
  /** The session currently subscribed for a live tail (one at a time). */
  subscribedSession: string | null;

  /** Overlay modal, the expanded accordion panel, and the sidebar geometry. */
  ui: {
    modal: ModalKind;
    sidebarPanel: SidebarPanel;
    /** Whether the whole left sidebar is hidden (titlebar toggle). */
    sidebarCollapsed: boolean;
    /** Sidebar column width in px (drag-to-resize; clamped to the bounds). */
    sidebarWidth: number;
    /** Whole-UI zoom factor (webview setZoom; Cmd/Ctrl +/-/0 + settings slider). */
    uiScale: number;
  };
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
    diagnostics: null,
    tasksLoaded: false,
    metaLoaded: false,
    loading: false,
    error: null,
  };
}

export function emptyExtras(): TaskExtras {
  return { sessions: [], comments: [], loaded: false };
}

export function initialState(): AppState {
  return {
    projects: [],
    projectsLoaded: false,
    activeProject: null,
    byProject: {},
    selection: NO_SELECTION,
    extrasByTask: {},
    sessions: {},
    sessionOrderByProject: {},
    transcriptBySession: {},
    seenLineBySession: {},
    subscribedSession: null,
    ui: {
      modal: null,
      sidebarPanel: "tasks",
      sidebarCollapsed: false,
      sidebarWidth: SIDEBAR_DEFAULT_WIDTH,
      uiScale: UI_SCALE_DEFAULT,
    },
    daemon: { connected: false, mode: "local" },
    settings: null,
    notice: null,
  };
}

// ---- actions --------------------------------------------------------------

export type Action =
  // bootstrap / connection / ui
  | { type: "settings/loaded"; settings: AppSettings }
  | { type: "daemon/status"; status: DaemonStatus }
  | { type: "notice/set"; notice: AppState["notice"] }
  | { type: "ui/modal"; modal: ModalKind }
  | { type: "ui/sidebarPanel"; panel: SidebarPanel }
  | { type: "ui/sidebarToggle" }
  | { type: "ui/sidebarSetCollapsed"; collapsed: boolean }
  | { type: "ui/sidebarWidth"; width: number }
  | { type: "ui/uiScale"; scale: number }
  // projects
  | { type: "projects/loaded"; projects: ProjectInfo[] }
  | { type: "project/select"; root: string | null }
  | { type: "project/tasksLoading"; root: string }
  | { type: "project/tasksLoaded"; root: string; tasks: TaskView[] }
  | { type: "project/metaLoaded"; root: string; workflows: WorkflowInfo[] }
  | { type: "project/diagnosticsLoaded"; root: string; diagnostics: ProjectDiagnostics }
  | { type: "project/error"; root: string; error: string }
  // selection
  | { type: "selection/set"; selection: Selection }
  // tasks
  | { type: "task/upserted"; root: string; task: TaskView }
  | { type: "task/extrasLoaded"; taskId: string; extras: Omit<TaskExtras, "loaded"> }
  // sessions
  | { type: "sessions/loaded"; root: string; sessions: SessionMeta[] }
  | { type: "session/upsert"; session: SessionMeta }
  // transcript / live tail
  | { type: "session/subscribed"; sessionId: string | null }
  | { type: "session/transcriptReset"; sessionId: string; entries: TranscriptLine[]; nextLine: number }
  | { type: "session/transcriptAppended"; sessionId: string; line: number; entry: TranscriptLine };
