// state/store.tsx — the React store: useReducer + an effects layer (async IPC
// calls that dispatch results) + the event router (plan §6 "State engine"). The
// event router (subscribeJobEvents / subscribeTaskChanged / subscribeProjectChanged)
// maps daemon pushes into reducer actions and refreshes, so every lazy write
// verb is reflected without a manual refresh.

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
import { type Action, type AppState, type MainView, initialState } from "./types";

interface Effects {
  bootstrap(): Promise<void>;
  refreshProjects(): Promise<void>;
  selectProject(root: string | null): Promise<void>;
  refreshTasks(root?: string): Promise<void>;
  refreshMeta(root?: string): Promise<void>;
  selectTask(id: string | null): Promise<void>;
  refreshTask(taskId: string, forceJobId?: string): Promise<void>;
  setView(view: MainView): void;
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

    async function loadTaskMessages(cwd: string, jobs: Job[], forceJobId?: string) {
      // Terminal jobs: snapshot via job.messages. Running job: live tail below.
      // Terminal-job transcripts are immutable, so load each at most once and
      // skip it on subsequent task-changed refreshes (avoids O(jobs) disk reads
      // per change). `forceJobId` reloads exactly the job that just finished
      // (the `done` job-event), giving its final transcript a single refresh.
      const have = stateRef.current.messagesByJob;
      for (const job of jobs) {
        if (job.status === "running" || job.status === "queued") continue;
        if (job.job_id !== forceJobId && have[job.job_id] !== undefined) continue;
        try {
          const msgs = await ipc.jobMessages(cwd, job.job_id, true, 0);
          dispatch({ type: "job/messagesReset", jobId: job.job_id, messages: msgs });
        } catch {
          /* best-effort; an unreadable transcript is non-fatal */
        }
      }
    }

    async function subscribeLive(cwd: string, jobs: Job[]) {
      // Pick the newest non-terminal job from the freshly-fetched list (not from
      // stateRef, which may not have re-rendered with the new jobs yet).
      const liveJobs = jobs
        .filter((j) => j.status === "running" || j.status === "queued")
        .slice()
        .sort((a, b) => Date.parse(a.created_at || "") - Date.parse(b.created_at || ""));
      const live = liveJobs.length > 0 ? liveJobs[liveJobs.length - 1] : null;
      // Tear down any previous subscription targeting a different job.
      const prev = subRef.current;
      if (prev && (!live || prev.jobId !== live.job_id)) {
        try {
          await ipc.jobUnsubscribe(prev.cwd, prev.jobId);
        } catch {
          /* ignore */
        }
        subRef.current = null;
        dispatch({ type: "job/subscribed", jobId: null });
      }
      if (!live) return;
      if (subRef.current?.jobId === live.job_id) return;
      // Reset the running job's transcript, then replay-then-tail via subscribe.
      dispatch({ type: "job/messagesReset", jobId: live.job_id, messages: [] });
      try {
        await ipc.jobSubscribe(cwd, live.job_id, { attach: true, full: true });
        subRef.current = { cwd, jobId: live.job_id };
        dispatch({ type: "job/subscribed", jobId: live.job_id });
      } catch {
        /* a not-ready runner just means no live frames yet */
      }
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
        // load its tasks+meta — avoiding a stale dispatch→read in one tick.
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
        // Record the selection only; the `activeProject` effect loads tasks+meta
        // (covers explicit selection AND the reducer's auto-select on launch /
        // after a registry change, with no double-load).
        dispatch({ type: "project/select", root });
      },

      async refreshTasks(root) {
        const cwd = root ?? cwdOf();
        if (!cwd) return;
        dispatch({ type: "project/tasksLoading", root: cwd });
        try {
          const tasks = await ipc.taskList(cwd, { statuses: ["new", "work", "human", "done", "cancel"] });
          dispatch({ type: "project/tasksLoaded", root: cwd, tasks });
          // Fuse Jobs into the Tasks view: load the project's live jobs so the
          // sidebar can show running/streaming indicators per task (plan §6).
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
          /* meta is non-fatal for the tasks view */
        }
      },

      async selectTask(id) {
        dispatch({ type: "task/select", id });
        if (id) {
          await eff.refreshTask(id);
        } else {
          // Leaving the task view: drop any live subscription.
          const prev = subRef.current;
          if (prev) {
            try {
              await ipc.jobUnsubscribe(prev.cwd, prev.jobId);
            } catch {
              /* ignore */
            }
            subRef.current = null;
            dispatch({ type: "job/subscribed", jobId: null });
          }
        }
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
          await subscribeLive(cwd, jobs);
        } catch (err) {
          dispatch({ type: "notice/set", notice: { kind: "error", text: String((err as Error).message ?? err) } });
        }
      },

      setView(view) {
        dispatch({ type: "view/set", view });
      },

      setNotice(notice) {
        dispatch({ type: "notice/set", notice });
      },

      resetLiveTail() {
        // `job.subscribe` is per-connection daemon state; on a connection-
        // generation change the marker is stale. Clear it so the next
        // refreshTask re-subscribes on the NEW connection (subscribeLive's
        // guard keys off subRef).
        subRef.current = null;
        dispatch({ type: "job/subscribed", jobId: null });
      },

      async reconnect() {
        // We KNOW the connection generation changed, so reset the live tail and
        // re-sync the active view DETERMINISTICALLY here rather than depending
        // on daemon-status event ordering (a superseded connection's late EOF
        // can otherwise reorder relative to the new connection's `true`).
        eff.resetLiveTail();
        try {
          const status = await ipc.reconnectDaemon();
          dispatch({ type: "daemon/status", status });
          if (status.connected) {
            await eff.refreshProjects();
            const cur = stateRef.current;
            if (cur.activeProject) {
              await eff.refreshTasks(cur.activeProject);
              await eff.refreshMeta(cur.activeProject);
            }
            if (cur.activeTaskId) await eff.refreshTask(cur.activeTaskId);
          }
        } catch (err) {
          dispatch({ type: "notice/set", notice: { kind: "error", text: String((err as Error).message ?? err) } });
        }
      },
    };
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
        // drop — the manual reconnect/mode-switch paths reset deterministically
        // in effects.reconnect). `job.subscribe` is per-connection daemon state,
        // so any live tail is now bound to a dead connection: reset the marker
        // so the next refresh re-subscribes on the new connection.
        effects.resetLiveTail();
        // On (re)connect, proactively re-sync + re-subscribe the active view so a
        // running job keeps streaming and any workflow/agent that changed while
        // disconnected (refreshMeta) is picked up without a manual refresh.
        if (status.connected) {
          const cur = stateRef.current;
          if (cur.activeProject) {
            void effects.refreshTasks(cur.activeProject);
            void effects.refreshMeta(cur.activeProject);
          }
          if (cur.activeTaskId) void effects.refreshTask(cur.activeTaskId);
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
          if (cur.activeTaskId) void effects.refreshTask(cur.activeTaskId);
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
              // A finished run may have advanced the workflow — refresh the task,
              // forcing a single final reload of the just-finished job's
              // (now-immutable) transcript.
              const cur = stateRef.current;
              if (cur.activeTaskId) void effects.refreshTask(cur.activeTaskId, evt.job_id);
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

  // Load a project's tasks + meta whenever it becomes the active project. This
  // covers BOTH an explicit user selection and the reducer's auto-select of the
  // first project on launch (and after a registry change). Reading from
  // committed React state (not stateRef) is what fixes the first-launch race
  // where tasks/workflows/agents never loaded for the auto-selected project.
  useEffect(() => {
    const root = state.activeProject;
    if (!root) return;
    void effects.refreshTasks(root);
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
