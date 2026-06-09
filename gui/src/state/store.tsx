// state/store.tsx — the React store: useReducer + an effects layer (async IPC
// calls that dispatch results) + the event router (redesign plan §6.3). The
// event router (subscribeJobEvents / subscribeTaskChanged / subscribeProjectChanged)
// maps daemon pushes into reducer actions and refreshes. Selection is the
// unified entity model (task | session | workflow | none); the live transcript
// tail follows either the selected session's job or, when a task is selected,
// the task's newest running job (one subscription at a time in v1).

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
  subscribeJobEvents,
  subscribeProjectChanged,
  subscribeTaskChanged,
} from "@/services/events";
import type { Job } from "@/types";
import { rootReducer } from "./reducer";
import { type Action, type AppState, type ModalKind, type SidebarPanel, initialState } from "./types";
import { NO_SELECTION, selectedSessionJobId, selectedTaskId } from "./selection";

interface Effects {
  bootstrap(): Promise<void>;
  refreshProjects(): Promise<void>;
  selectProject(root: string | null): Promise<void>;
  refreshTasks(root?: string): Promise<void>;
  refreshSessions(root?: string): Promise<void>;
  refreshMeta(root?: string): Promise<void>;
  selectTask(id: string | null): Promise<void>;
  selectSession(jobId: string | null): Promise<void>;
  selectWorkflow(name: string | null): void;
  clearSelection(): void;
  refreshTask(taskId: string, forceJobId?: string): Promise<void>;
  setSidebarPanel(panel: SidebarPanel): void;
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
  const [state, dispatch] = useReducer(rootReducer, undefined, initialState);

  // Keep a ref to the latest state so the (stable) event handlers can read it
  // without re-subscribing on every render.
  const stateRef = useRef(state);
  stateRef.current = state;

  // The job currently subscribed for a live tail, tracked outside React so the
  // cleanup path can unsubscribe the right job.
  const subRef = useRef<{ cwd: string; jobId: string } | null>(null);

  const effects = useMemo<Effects>(() => {
    const cwdOf = () => stateRef.current.activeProject ?? "";

    async function teardownLiveTail() {
      const prev = subRef.current;
      if (!prev) return;
      try {
        await ipc.jobUnsubscribe(prev.cwd, prev.jobId);
      } catch {
        /* ignore */
      }
      subRef.current = null;
      dispatch({ type: "job/subscribed", jobId: null });
    }

    // Reset a job's transcript and replay-then-tail it via subscribe. Tears
    // down any previous subscription first (one live tail at a time in v1).
    async function subscribeToJob(cwd: string, jobId: string) {
      if (subRef.current?.jobId === jobId) return;
      await teardownLiveTail();
      dispatch({ type: "job/messagesReset", jobId, messages: [] });
      try {
        await ipc.jobSubscribe(cwd, jobId, { attach: true, full: true });
        subRef.current = { cwd, jobId };
        dispatch({ type: "job/subscribed", jobId });
      } catch {
        /* a not-ready runner just means no live frames yet */
      }
    }

    // Load a terminal job's transcript once (immutable snapshot).
    async function snapshotJob(cwd: string, jobId: string) {
      if (stateRef.current.messagesByJob[jobId] !== undefined) return;
      try {
        const msgs = await ipc.jobMessages(cwd, jobId, true, 0);
        dispatch({ type: "job/messagesReset", jobId, messages: msgs });
      } catch {
        /* best-effort; an unreadable transcript is non-fatal */
      }
    }

    async function loadTaskMessages(cwd: string, jobs: Job[], forceJobId?: string) {
      // Terminal jobs: snapshot via job.messages. Running job: live tail below.
      // Terminal-job transcripts are immutable, so load each at most once and
      // skip it on subsequent task-changed refreshes. `forceJobId` reloads
      // exactly the job that just finished (the `done` job-event).
      const have = stateRef.current.messagesByJob;
      for (const job of jobs) {
        if (job.status === "running" || job.status === "queued") continue;
        if (job.job_id !== forceJobId && have[job.job_id] !== undefined) continue;
        try {
          const msgs = await ipc.jobMessages(cwd, job.job_id, true, 0);
          dispatch({ type: "job/messagesReset", jobId: job.job_id, messages: msgs });
        } catch {
          /* best-effort */
        }
      }
    }

    // When a TASK is selected, tail its newest running/queued job (if any).
    async function subscribeTaskLive(cwd: string, jobs: Job[]) {
      const liveJobs = jobs
        .filter((j) => j.status === "running" || j.status === "queued")
        .slice()
        .sort((a, b) => Date.parse(a.created_at || "") - Date.parse(b.created_at || ""));
      const live = liveJobs.length > 0 ? liveJobs[liveJobs.length - 1] : null;
      if (!live) {
        await teardownLiveTail();
        return;
      }
      await subscribeToJob(cwd, live.job_id);
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
        // load its tasks + sessions + meta.
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
        // sessions + meta (covers explicit selection AND the reducer's
        // auto-select on launch / after a registry change, with no double-load).
        await teardownLiveTail();
        dispatch({ type: "project/select", root });
      },

      async refreshTasks(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        dispatch({ type: "project/tasksLoading", root: cwd });
        try {
          const tasks = await ipc.taskList(cwd, { statuses: ["new", "work", "human", "done", "cancel"] });
          dispatch({ type: "project/tasksLoaded", root: cwd, tasks });
          // Load the project's live jobs so the tasks panel can show
          // running/streaming indicators per task.
          try {
            const live = await ipc.jobList(cwd, { statuses: ["running", "queued"] });
            dispatch({ type: "jobs/upsertMany", jobs: live });
          } catch {
            /* indicators are best-effort */
          }
        } catch (err) {
          dispatch({ type: "project/error", root: cwd, error: String((err as Error).message ?? err) });
        }
      },

      async refreshSessions(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        try {
          const jobs = await ipc.jobList(cwd, {});
          dispatch({ type: "sessions/loaded", root: cwd, jobs });
        } catch {
          /* sessions list is best-effort */
        }
      },

      async refreshMeta(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        try {
          const [workflows, agents] = await Promise.all([
            ipc.workflowList(cwd, false),
            ipc.agentList(cwd),
          ]);
          dispatch({ type: "project/metaLoaded", root: cwd, workflows, agents });
        } catch {
          /* meta is non-fatal */
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

      async selectSession(jobId) {
        dispatch({ type: "selection/set", selection: jobId ? { kind: "session", jobId } : NO_SELECTION });
        if (!jobId) {
          await teardownLiveTail();
          return;
        }
        const cwd = cwdOf();
        if (!cwd) return;
        const job = stateRef.current.jobs[jobId];
        if (job && (job.status === "running" || job.status === "queued")) {
          await subscribeToJob(cwd, jobId);
        } else {
          await teardownLiveTail();
          await snapshotJob(cwd, jobId);
        }
      },

      selectWorkflow(name) {
        // Selecting a workflow leaves any live session tail running is wrong —
        // drop it so the (one-at-a-time) subscription isn't orphaned.
        void teardownLiveTail();
        dispatch({ type: "selection/set", selection: name ? { kind: "workflow", name } : NO_SELECTION });
      },

      clearSelection() {
        void teardownLiveTail();
        dispatch({ type: "selection/set", selection: NO_SELECTION });
      },

      async refreshTask(taskId, forceJobId) {
        const cwd = cwdOf();
        if (!cwd) return;
        try {
          const [task, jobs, comments, signals] = await Promise.all([
            ipc.taskGet(cwd, taskId),
            ipc.jobList(cwd, { task_id: taskId }),
            ipc.commentList(cwd, taskId),
            ipc.signalsForTask(cwd, taskId),
          ]);
          dispatch({ type: "task/upserted", root: cwd, task });
          dispatch({ type: "task/extrasLoaded", taskId, extras: { jobs, comments, signals } });
          await loadTaskMessages(cwd, jobs, forceJobId);
          // Only drive the task's live tail while a task is the active
          // selection (a selected session owns the subscription otherwise).
          if (selectedTaskId(stateRef.current.selection) === taskId) {
            await subscribeTaskLive(cwd, jobs);
          }
        } catch (err) {
          dispatch({ type: "notice/set", notice: { kind: "error", text: String((err as Error).message ?? err) } });
        }
      },

      setSidebarPanel(panel) {
        dispatch({ type: "ui/sidebarPanel", panel });
      },

      openModal(modal) {
        dispatch({ type: "ui/modal", modal });
      },

      setNotice(notice) {
        dispatch({ type: "notice/set", notice });
      },

      resetLiveTail() {
        // `job.subscribe` is per-connection daemon state; on a connection-
        // generation change the marker is stale. Clear it so the next refresh
        // re-subscribes on the NEW connection.
        subRef.current = null;
        dispatch({ type: "job/subscribed", jobId: null });
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
              await eff.refreshTasks(cur.activeProject);
              await eff.refreshSessions(cur.activeProject);
              await eff.refreshMeta(cur.activeProject);
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
      const jobId = selectedSessionJobId(cur.selection);
      if (jobId) {
        await eff.selectSession(jobId);
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
        // drop). `job.subscribe` is per-connection state, so any live tail is
        // bound to a dead connection: reset the marker so the next refresh
        // re-subscribes on the new connection.
        effects.resetLiveTail();
        if (status.connected) {
          const cur = stateRef.current;
          if (cur.activeProject) {
            void effects.refreshTasks(cur.activeProject);
            void effects.refreshSessions(cur.activeProject);
            void effects.refreshMeta(cur.activeProject);
          }
          const taskId = selectedTaskId(cur.selection);
          const jobId = selectedSessionJobId(cur.selection);
          if (taskId) void effects.refreshTask(taskId);
          else if (jobId) void effects.selectSession(jobId);
        }
      }),

      subscribeProjectChanged(() => {
        void effects.refreshProjects();
        const cwd = stateRef.current.activeProject;
        if (cwd) void effects.refreshMeta(cwd);
      }),

      subscribeTaskChanged((evt) => {
        const cur = stateRef.current;
        if (evt.root && evt.root === cur.activeProject) {
          void effects.refreshTasks(cur.activeProject);
          void effects.refreshSessions(cur.activeProject);
          const taskId = selectedTaskId(cur.selection);
          if (taskId) void effects.refreshTask(taskId);
        }
      }),

      subscribeJobEvents((evt) => {
        switch (evt.kind) {
          case "message":
            if (evt.event) {
              dispatch({
                type: "job/messageAppended",
                jobId: evt.job_id,
                eventId: evt.event_id ?? 0,
                message: evt.event,
              });
            }
            break;
          case "status":
          case "done":
            if (evt.job) {
              dispatch({ type: "job/upsert", job: evt.job });
            }
            if (evt.kind === "done") {
              const cur = stateRef.current;
              const taskId = selectedTaskId(cur.selection);
              // A finished run may have advanced the workflow — refresh the
              // selected task (forcing one final reload of the just-finished
              // job's now-immutable transcript). If the finished job is the
              // selected session, re-snapshot it directly.
              if (taskId) {
                void effects.refreshTask(taskId, evt.job_id);
              } else if (selectedSessionJobId(cur.selection) === evt.job_id) {
                void effects.selectSession(evt.job_id);
              }
            }
            break;
          case "error":
            dispatch({ type: "notice/set", notice: { kind: "error", text: evt.error ?? "job error" } });
            break;
        }
      }),
    ];
    return () => {
      for (const u of unsubs) u();
    };
  }, [effects]);

  // Load a project's tasks + sessions + meta whenever it becomes the active
  // project. Covers BOTH an explicit user selection and the reducer's
  // auto-select of the first project on launch (and after a registry change).
  useEffect(() => {
    const root = state.activeProject;
    if (!root) return;
    void effects.refreshTasks(root);
    void effects.refreshSessions(root);
    void effects.refreshMeta(root);
  }, [state.activeProject, effects]);

  // Bootstrap once on mount. `effects` is stable (useMemo []), so the empty
  // dep array is intentional — bootstrap runs exactly once.
  useEffect(() => {
    void effects.bootstrap();
  }, [effects]);

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
