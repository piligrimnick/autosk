// state/store.tsx — the React store: useReducer + an effects layer (async IPC
// calls that dispatch results) + the event router (redesign plan §6.3). The
// event router (subscribeSessionEvents / subscribeTaskChanged /
// subscribeProjectChanged) maps daemon pushes into reducer actions and
// refreshes. Selection is the unified entity model (task | session | workflow |
// none); the live transcript tail follows either the selected session or, when
// a task is selected, the task's newest running session (one subscription at a
// time).
//
// Proto-v2 note: `task.subscribe` now REQUIRES `{cwd}` and OPENS that project,
// so the Rust connect-time auto-subscribe was dropped. The front end issues
// `task.subscribe` when a project becomes active (and re-issues it after a
// reconnect, since it is per-connection state); `project.subscribe` stays
// global and is still auto-issued by the Rust backend on connect.

import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  type ReactNode,
} from "react";
import * as ipc from "@/services/ipc";
import {
  subscribeDaemonStatus,
  subscribeProjectChanged,
  subscribeSessionEvents,
  subscribeTaskChanged,
} from "@/services/events";
import type { SessionMeta } from "@/types";
import { rootReducer } from "./reducer";
import {
  type Action,
  type AppState,
  type ModalKind,
  type SidebarPanel,
  clampSidebarWidth,
  initialState,
} from "./types";
import { NO_SELECTION, selectedSessionId, selectedTaskId } from "./selection";
import { loadUiScale, saveUiScale } from "@/features/layout/utils/uiScale";

// localStorage keys for the sidebar geometry (layout-only UI state; not part of
// the daemon's domain, so it lives in the browser, not the project DB).
const LS_SIDEBAR_COLLAPSED = "autosk.sidebarCollapsed";
const LS_SIDEBAR_WIDTH = "autosk.sidebarWidth";

// Seed the reducer with the persisted sidebar geometry so the first paint is
// already correct (no width/collapse flash). Pure in non-browser test runs
// (`window` is undefined under vitest's node environment), where it returns the
// plain `initialState()`.
function hydratedInitialState(): AppState {
  const base = initialState();
  if (typeof window === "undefined") return base;
  try {
    const collapsed = window.localStorage.getItem(LS_SIDEBAR_COLLAPSED) === "1";
    const widthRaw = window.localStorage.getItem(LS_SIDEBAR_WIDTH);
    const sidebarWidth = widthRaw ? clampSidebarWidth(Number(widthRaw)) : base.ui.sidebarWidth;
    return {
      ...base,
      ui: { ...base.ui, sidebarCollapsed: collapsed, sidebarWidth, uiScale: loadUiScale() },
    };
  } catch {
    return base;
  }
}

interface Effects {
  bootstrap(): Promise<void>;
  refreshProjects(): Promise<void>;
  selectProject(root: string | null): Promise<void>;
  refreshTasks(root?: string): Promise<void>;
  refreshSessions(root?: string): Promise<void>;
  refreshMeta(root?: string): Promise<void>;
  refreshDiagnostics(root?: string): Promise<void>;
  selectTask(id: string | null): Promise<void>;
  selectSession(sessionId: string | null): Promise<void>;
  selectWorkflow(name: string | null): void;
  clearSelection(): void;
  refreshTask(taskId: string): Promise<void>;
  setSidebarPanel(panel: SidebarPanel): void;
  toggleSidebar(): void;
  setSidebarWidth(width: number): void;
  setUiScale(scale: number): void;
  openModal(modal: ModalKind): void;
  setNotice(notice: AppState["notice"]): void;
  resetLiveTail(): void;
  reconnect(): Promise<void>;
}

interface StoreValue {
  state: AppState;
  dispatch: React.Dispatch<Action>;
  effects: Effects;
  /** Active project root == RPC `cwd`. "" when no project selected. */
  cwd: string;
}

const StoreContext = createContext<StoreValue | null>(null);

export function StoreProvider({ children }: { children: ReactNode }) {
  const [state, dispatch] = useReducer(rootReducer, undefined, hydratedInitialState);

  // Keep a ref to the latest state so the (stable) event handlers can read it
  // without re-subscribing on every render.
  const stateRef = useRef(state);
  stateRef.current = state;

  // The session currently subscribed for a live tail, tracked outside React so
  // the cleanup path can unsubscribe the right session.
  const subRef = useRef<{ cwd: string; sessionId: string } | null>(null);

  const effects = useMemo<Effects>(() => {
    const cwdOf = () => stateRef.current.activeProject ?? "";

    async function teardownLiveTail() {
      const prev = subRef.current;
      if (!prev) return;
      try {
        await ipc.sessionUnsubscribe(prev.cwd, prev.sessionId);
      } catch {
        /* ignore */
      }
      subRef.current = null;
      dispatch({ type: "session/subscribed", sessionId: null });
    }

    // Reset a session's transcript and replay-then-tail it via subscribe. Tears
    // down any previous subscription first (one live tail at a time). The
    // daemon replays from line 1 as `session-event` message frames, then tails.
    async function subscribeToSession(cwd: string, sessionId: string) {
      if (subRef.current?.sessionId === sessionId) return;
      await teardownLiveTail();
      dispatch({ type: "session/transcriptReset", sessionId, entries: [], nextLine: 1 });
      try {
        await ipc.sessionSubscribe(cwd, sessionId, 1);
        subRef.current = { cwd, sessionId };
        dispatch({ type: "session/subscribed", sessionId });
      } catch {
        /* a not-ready session just means no live frames yet */
      }
    }

    // Load a terminal session's transcript once (immutable snapshot).
    async function snapshotSession(cwd: string, sessionId: string) {
      if (stateRef.current.transcriptBySession[sessionId] !== undefined) return;
      try {
        const { entries, next_line } = await ipc.sessionTranscript(cwd, sessionId, 1);
        dispatch({ type: "session/transcriptReset", sessionId, entries, nextLine: next_line });
      } catch {
        /* best-effort; an unreadable transcript is non-fatal */
      }
    }

    async function loadTaskTranscripts(cwd: string, sessions: SessionMeta[]) {
      // Terminal sessions: snapshot via session.transcript (immutable, so load
      // each at most once and skip on later refreshes). The running session is
      // live-tailed below; its tail is already complete by the time it goes
      // terminal — the daemon flushes the terminal entries (autosk:transit /
      // autosk:session_end) as numbered `message` frames BEFORE the `done`
      // frame — so a just-finished session needs no reload here.
      const have = stateRef.current.transcriptBySession;
      for (const session of sessions) {
        if (session.status === "running" || session.status === "queued") continue;
        if (have[session.id] !== undefined) continue;
        await snapshotSession(cwd, session.id);
      }
    }

    // When a TASK is selected, tail its newest running/queued session (if any).
    async function subscribeTaskLive(cwd: string, sessions: SessionMeta[]) {
      // A queued session has started_at=null; treat null as newest (just
      // spawned) and break ties by id so the tailed session is deterministic
      // (a raw Date.parse("") comparator yields NaN and an unstable order).
      const startMs = (m: SessionMeta) => {
        const t = m.started_at ? Date.parse(m.started_at) : NaN;
        return Number.isNaN(t) ? Number.POSITIVE_INFINITY : t;
      };
      const liveSessions = sessions
        .filter((m) => m.status === "running" || m.status === "queued")
        .slice()
        .sort((a, b) => startMs(a) - startMs(b) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
      const live = liveSessions.length > 0 ? liveSessions[liveSessions.length - 1] : null;
      if (!live) {
        await teardownLiveTail();
        return;
      }
      await subscribeToSession(cwd, live.id);
    }

    const eff: Effects = {
      async bootstrap() {
        try {
          const settings = await ipc.getAppSettings();
          dispatch({ type: "settings/loaded", settings });
        } catch {
          /* defaults */
        }
        try {
          const status = await ipc.getDaemonStatus();
          dispatch({ type: "daemon/status", status });
        } catch {
          /* not connected yet */
        }
        // Just load the registry. The reducer auto-selects the first project
        // (projects/loaded), which the `activeProject` effect below picks up to
        // load its tasks + sessions + meta + diagnostics.
        await eff.refreshProjects();
      },

      async refreshProjects() {
        try {
          const projects = await ipc.projectList();
          dispatch({ type: "projects/loaded", projects });
        } catch (err) {
          dispatch({ type: "notice/set", notice: { kind: "error", text: String((err as Error).message ?? err) } });
        }
      },

      async selectProject(root) {
        // Record the selection only; the `activeProject` effect loads tasks +
        // sessions + meta + diagnostics (covers explicit selection AND the
        // reducer's auto-select on launch / after a registry change, with no
        // double-load) and manages this project's task.subscribe.
        await teardownLiveTail();
        dispatch({ type: "project/select", root });
      },

      async refreshTasks(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        dispatch({ type: "project/tasksLoading", root: cwd });
        try {
          const tasks = await ipc.taskList(cwd, {});
          dispatch({ type: "project/tasksLoaded", root: cwd, tasks });
        } catch (err) {
          dispatch({ type: "project/error", root: cwd, error: String((err as Error).message ?? err) });
        }
      },

      async refreshSessions(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        try {
          const sessions = await ipc.sessionList(cwd);
          dispatch({ type: "sessions/loaded", root: cwd, sessions });
        } catch {
          /* sessions list is best-effort */
        }
      },

      async refreshMeta(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        try {
          const [workflows, agents] = await Promise.all([ipc.workflowList(cwd), ipc.agentList(cwd)]);
          dispatch({ type: "project/metaLoaded", root: cwd, workflows, agents });
        } catch {
          /* meta is non-fatal */
        }
      },

      async refreshDiagnostics(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        try {
          const diagnostics = await ipc.projectDiagnostics(cwd);
          dispatch({ type: "project/diagnosticsLoaded", root: cwd, diagnostics });
        } catch {
          /* diagnostics are non-fatal */
        }
      },

      async selectTask(id) {
        dispatch({ type: "selection/set", selection: id ? { kind: "task", taskId: id } : NO_SELECTION });
        if (id) {
          await eff.refreshTask(id);
        } else {
          await teardownLiveTail();
        }
      },

      async selectSession(sessionId) {
        dispatch({ type: "selection/set", selection: sessionId ? { kind: "session", sessionId } : NO_SELECTION });
        if (!sessionId) {
          await teardownLiveTail();
          return;
        }
        const cwd = cwdOf();
        if (!cwd) return;
        const session = stateRef.current.sessions[sessionId];
        if (session && (session.status === "running" || session.status === "queued")) {
          await subscribeToSession(cwd, sessionId);
        } else {
          await teardownLiveTail();
          await snapshotSession(cwd, sessionId);
        }
      },

      selectWorkflow(name) {
        // Selecting a workflow must not leave a live session tail orphaned —
        // drop it so the (one-at-a-time) subscription isn't dangling.
        void teardownLiveTail();
        dispatch({ type: "selection/set", selection: name ? { kind: "workflow", name } : NO_SELECTION });
      },

      clearSelection() {
        void teardownLiveTail();
        dispatch({ type: "selection/set", selection: NO_SELECTION });
      },

      async refreshTask(taskId) {
        const cwd = cwdOf();
        if (!cwd) return;
        try {
          const [task, sessions, comments] = await Promise.all([
            ipc.taskGet(cwd, taskId),
            ipc.sessionList(cwd, taskId),
            ipc.commentList(cwd, taskId),
          ]);
          dispatch({ type: "task/upserted", root: cwd, task });
          dispatch({ type: "task/extrasLoaded", taskId, extras: { sessions, comments } });
          await loadTaskTranscripts(cwd, sessions);
          // Only drive the task's live tail while a task is the active
          // selection (a selected session owns the subscription otherwise).
          if (selectedTaskId(stateRef.current.selection) === taskId) {
            await subscribeTaskLive(cwd, sessions);
          }
        } catch (err) {
          dispatch({ type: "notice/set", notice: { kind: "error", text: String((err as Error).message ?? err) } });
        }
      },

      setSidebarPanel(panel) {
        dispatch({ type: "ui/sidebarPanel", panel });
      },

      toggleSidebar() {
        dispatch({ type: "ui/sidebarToggle" });
      },

      setSidebarWidth(width) {
        dispatch({ type: "ui/sidebarWidth", width });
      },

      setUiScale(scale) {
        dispatch({ type: "ui/uiScale", scale });
      },

      openModal(modal) {
        dispatch({ type: "ui/modal", modal });
      },

      setNotice(notice) {
        dispatch({ type: "notice/set", notice });
      },

      resetLiveTail() {
        // `session.subscribe` is per-connection daemon state; on a connection-
        // generation change the marker is stale. Clear it so the next refresh
        // re-subscribes on the NEW connection.
        subRef.current = null;
        dispatch({ type: "session/subscribed", sessionId: null });
      },

      async reconnect() {
        // We KNOW the connection generation changed, so reset the live tail and
        // re-sync the active view DETERMINISTICALLY here rather than depending
        // on daemon-status event ordering.
        eff.resetLiveTail();
        try {
          const status = await ipc.reconnectDaemon();
          dispatch({ type: "daemon/status", status });
          if (status.connected) {
            await eff.refreshProjects();
            const cur = stateRef.current;
            if (cur.activeProject) {
              // task.subscribe is per-connection state and the Rust connect no
              // longer auto-issues it — re-subscribe the active project here.
              void ipc.taskSubscribe(cur.activeProject).catch(() => {});
              await eff.refreshTasks(cur.activeProject);
              await eff.refreshSessions(cur.activeProject);
              await eff.refreshMeta(cur.activeProject);
              await eff.refreshDiagnostics(cur.activeProject);
            }
            await resyncSelection(cur);
          }
        } catch (err) {
          dispatch({ type: "notice/set", notice: { kind: "error", text: String((err as Error).message ?? err) } });
        }
      },
    };

    // Re-sync whatever entity is selected after a (re)connect.
    async function resyncSelection(cur: AppState) {
      const taskId = selectedTaskId(cur.selection);
      if (taskId) {
        await eff.refreshTask(taskId);
        return;
      }
      const sessionId = selectedSessionId(cur.selection);
      if (sessionId) {
        await eff.selectSession(sessionId);
      }
    }

    return eff;
  }, []);

  // ---- event router: daemon pushes -> reducer actions / refreshes ----------
  useEffect(() => {
    const unsubs = [
      subscribeDaemonStatus((status) => {
        const wasConnected = stateRef.current.daemon.connected;
        dispatch({ type: "daemon/status", status });
        if (wasConnected === status.connected) return;
        // The connection generation changed (spontaneous daemon restart / socket
        // drop). `session.subscribe` is per-connection state, so any live tail is
        // bound to a dead connection: reset the marker so the next refresh
        // re-subscribes on the new connection.
        effects.resetLiveTail();
        if (status.connected) {
          const cur = stateRef.current;
          if (cur.activeProject) {
            // Re-open this project's task-change push on the NEW connection
            // (per-connection; the Rust connect no longer auto-subscribes).
            void ipc.taskSubscribe(cur.activeProject).catch(() => {});
            void effects.refreshTasks(cur.activeProject);
            void effects.refreshSessions(cur.activeProject);
            void effects.refreshMeta(cur.activeProject);
            void effects.refreshDiagnostics(cur.activeProject);
          }
          const taskId = selectedTaskId(cur.selection);
          const sessionId = selectedSessionId(cur.selection);
          if (taskId) void effects.refreshTask(taskId);
          else if (sessionId) void effects.selectSession(sessionId);
        }
      }),

      subscribeProjectChanged(() => {
        void effects.refreshProjects();
        const cwd = stateRef.current.activeProject;
        if (cwd) {
          void effects.refreshMeta(cwd);
          void effects.refreshDiagnostics(cwd);
        }
      }),

      subscribeTaskChanged((evt) => {
        const cur = stateRef.current;
        if (!evt.root || evt.root !== cur.activeProject) return;
        // The push carries the full TaskView, so upsert it directly instead of
        // refetching the whole list.
        dispatch({ type: "task/upserted", root: evt.root, task: evt.task });
        // A task change may reflect a new/advanced session — refresh the panel.
        void effects.refreshSessions(cur.activeProject);
        const taskId = selectedTaskId(cur.selection);
        if (taskId && taskId === evt.task.id) void effects.refreshTask(taskId);
      }),

      subscribeSessionEvents((evt) => {
        switch (evt.kind) {
          case "message":
            if (evt.event) {
              dispatch({
                type: "session/transcriptAppended",
                sessionId: evt.session_id,
                line: evt.line ?? 0,
                entry: evt.event,
              });
            }
            break;
          case "status":
          case "done":
            if (evt.session) {
              dispatch({ type: "session/upsert", session: evt.session });
            }
            if (evt.kind === "done") {
              // A finished run may have advanced the workflow — refresh the
              // selected task. The live tail already holds the just-finished
              // session's complete (immutable) transcript, so no transcript
              // reload is needed (the `done` frame trails the terminal entries).
              // When the finished session is itself the selection, its now-dead
              // live tail is left as-is; it self-heals on the next selection
              // (teardownLiveTail runs at the head of the next subscribe, and
              // selectSession's terminal branch tears it down on re-select).
              const taskId = selectedTaskId(stateRef.current.selection);
              if (taskId) void effects.refreshTask(taskId);
            }
            break;
          case "error":
            dispatch({ type: "notice/set", notice: { kind: "error", text: evt.error ?? "session error" } });
            break;
        }
      }),
    ];
    return () => {
      for (const u of unsubs) u();
    };
  }, [effects]);

  // Load a project's tasks + sessions + meta + diagnostics whenever it becomes
  // the active project, and open/close its task-change subscription. Covers BOTH
  // an explicit user selection and the reducer's auto-select of the first
  // project on launch (and after a registry change).
  useEffect(() => {
    const root = state.activeProject;
    if (!root) return;
    // Front-end-issued task.subscribe (v2 requires {cwd} + opens the project).
    void ipc.taskSubscribe(root).catch(() => {});
    void effects.refreshTasks(root);
    void effects.refreshSessions(root);
    void effects.refreshMeta(root);
    void effects.refreshDiagnostics(root);
    return () => {
      void ipc.taskUnsubscribe(root).catch(() => {});
    };
  }, [state.activeProject, effects]);

  // Bootstrap once on mount. `effects` is stable (useMemo []), so the empty
  // dep array is intentional — bootstrap runs exactly once.
  useEffect(() => {
    void effects.bootstrap();
  }, [effects]);

  // Persist the sidebar geometry (collapse + width) across restarts.
  useEffect(() => {
    if (typeof window === "undefined") return;
    try {
      window.localStorage.setItem(LS_SIDEBAR_COLLAPSED, state.ui.sidebarCollapsed ? "1" : "0");
      window.localStorage.setItem(LS_SIDEBAR_WIDTH, String(state.ui.sidebarWidth));
    } catch {
      /* private mode / quota — non-fatal */
    }
  }, [state.ui.sidebarCollapsed, state.ui.sidebarWidth]);

  // Persist the UI zoom factor across restarts (layout-only UI state).
  useEffect(() => {
    saveUiScale(state.ui.uiScale);
  }, [state.ui.uiScale]);

  const value = useMemo<StoreValue>(
    () => ({ state, dispatch, effects, cwd: state.activeProject ?? "" }),
    [state, effects],
  );

  return <StoreContext.Provider value={value}>{children}</StoreContext.Provider>;
}

export function useStore(): StoreValue {
  const ctx = useContext(StoreContext);
  if (!ctx) {
    throw new Error("useStore must be used within <StoreProvider>");
  }
  return ctx;
}
