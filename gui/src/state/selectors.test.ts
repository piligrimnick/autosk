// Unit tests for the derived selectors (pure; no browser, no daemon). These
// lock composer-mode precedence, the sessions list + selected session, and the
// recency sort — several of which back the reconnect / first-load fixes.

import { describe, it, expect } from "vitest";
import {
  activeTask,
  composerMode,
  selectedSession,
  selectedWorkflow,
  sessionBadgeStatus,
  sessionsForProject,
  tasksByRecency,
} from "./selectors";
import { emptyProjectSlice, initialState, type AppState } from "./types";
import type { SessionMeta, TaskView, WorkflowInfo } from "@/types";

function mkWorkflow(name: string): WorkflowInfo {
  return {
    name,
    description: "",
    first_step: "dev",
    steps: [{ name: "dev", status: null, targets: [{ status: "done" }] }],
  };
}

function mkSession(p: Partial<SessionMeta> & Pick<SessionMeta, "id" | "task_id">): SessionMeta {
  return {
    kind: "task",
    workflow: "wf",
    step: "dev",
    agent: "a",
    status: "running",
    started_at: "2024-01-01T00:00:00Z",
    ended_at: null,
    ...p,
  };
}

function mkTask(p: Partial<TaskView> & Pick<TaskView, "id">): TaskView {
  return {
    title: "t",
    description: "",
    status: "new",
    workflow: null,
    step: null,
    blocked: false,
    blocked_by: [],
    blocks: [],
    comment_count: 0,
    metadata: {},
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
    ...p,
  };
}

/** A state with the active project's slice carrying one task + a task selection. */
function taskState(taskId: string, status: TaskView["status"], sessions: SessionMeta[] = []): AppState {
  const s = initialState();
  return {
    ...s,
    activeProject: "/p",
    byProject: {
      "/p": { ...emptyProjectSlice(), tasks: { [taskId]: mkTask({ id: taskId, status }) }, taskOrder: [taskId] },
    },
    sessions: Object.fromEntries(sessions.map((m) => [m.id, m])),
    selection: { kind: "task", taskId },
  };
}

describe("sessionBadgeStatus (interactive turn activity)", () => {
  it("a live interactive session surfaces idle/working from activity (idle is the default)", () => {
    const base = mkSession({ id: "s1", task_id: "", kind: "interactive", status: "running" });
    expect(sessionBadgeStatus({ ...base, activity: undefined })).toBe("idle");
    expect(sessionBadgeStatus({ ...base, activity: "idle" })).toBe("idle");
    expect(sessionBadgeStatus({ ...base, activity: "busy" })).toBe("working");
  });

  it("a non-running interactive session shows its lifecycle status, not activity", () => {
    const queued = mkSession({ id: "s", task_id: "", kind: "interactive", status: "queued" });
    expect(sessionBadgeStatus(queued)).toBe("queued");
    const done = mkSession({ id: "s", task_id: "", kind: "interactive", status: "done", activity: "busy" });
    expect(sessionBadgeStatus(done)).toBe("done");
  });

  it("a task session always shows its lifecycle status (activity ignored)", () => {
    const running = mkSession({ id: "s", task_id: "t1", status: "running", activity: "busy" });
    expect(sessionBadgeStatus(running)).toBe("running");
  });
});

describe("composerMode (entity-driven)", () => {
  it("returns 'none' when nothing is selected", () => {
    expect(composerMode(initialState())).toBe("none");
  });

  it("workflow session + running/queued session → 'steer'", () => {
    const m = mkSession({ id: "s1", task_id: "t1", status: "running" });
    const s: AppState = { ...initialState(), sessions: { s1: m }, selection: { kind: "session", sessionId: "s1" } };
    expect(composerMode(s)).toBe("steer");
    const queued: AppState = { ...s, sessions: { s1: { ...m, status: "queued" } } };
    expect(composerMode(queued)).toBe("steer");
  });

  it("interactive session + running/queued session → 'chat'", () => {
    const m = mkSession({ id: "s1", task_id: "", kind: "interactive", workflow: "", step: "", status: "running" });
    const s: AppState = { ...initialState(), sessions: { s1: m }, selection: { kind: "session", sessionId: "s1" } };
    expect(composerMode(s)).toBe("chat");
    const queued: AppState = { ...s, sessions: { s1: { ...m, status: "queued" } } };
    expect(composerMode(queued)).toBe("chat");
  });

  it("interactive session + terminal session → 'none' (read-only transcript)", () => {
    const m = mkSession({ id: "s1", task_id: "", kind: "interactive", status: "done" });
    const s: AppState = { ...initialState(), sessions: { s1: m }, selection: { kind: "session", sessionId: "s1" } };
    expect(composerMode(s)).toBe("none");
  });

  it("session + terminal session → 'none' (read-only transcript, no composer)", () => {
    const m = mkSession({ id: "s1", task_id: "t1", status: "done" });
    const s: AppState = { ...initialState(), sessions: { s1: m }, selection: { kind: "session", sessionId: "s1" } };
    expect(composerMode(s)).toBe("none");
  });

  it("any selected task → 'comment', regardless of status or a running session", () => {
    const running = [mkSession({ id: "s1", task_id: "t1", status: "running" })];
    expect(composerMode(taskState("t1", "human", running))).toBe("comment");
    expect(composerMode(taskState("t1", "new"))).toBe("comment");
    expect(composerMode(taskState("t1", "work"))).toBe("comment");
    expect(composerMode(taskState("t1", "done"))).toBe("comment");
    expect(composerMode(taskState("t1", "cancel"))).toBe("comment");
  });
});

describe("sessionsForProject / selectedSession", () => {
  it("returns the active project's sessions in session order", () => {
    const s1 = mkSession({ id: "s1", task_id: "t1" });
    const s2 = mkSession({ id: "s2", task_id: "t2" });
    const s: AppState = {
      ...initialState(),
      activeProject: "/p",
      sessions: { s1, s2 },
      sessionOrderByProject: { "/p": ["s2", "s1"] },
    };
    expect(sessionsForProject(s).map((m) => m.id)).toEqual(["s2", "s1"]);
    expect(sessionsForProject(initialState())).toEqual([]);
  });

  it("resolves the selected session", () => {
    const s1 = mkSession({ id: "s1", task_id: "t1" });
    const s: AppState = { ...initialState(), sessions: { s1 }, selection: { kind: "session", sessionId: "s1" } };
    expect(selectedSession(s)?.id).toBe("s1");
    expect(selectedSession(initialState())).toBeNull();
  });
});

describe("selectedWorkflow / activeTask", () => {
  it("resolves the selected workflow by name in the active project", () => {
    const wf = mkWorkflow("feature-dev");
    const s: AppState = {
      ...initialState(),
      activeProject: "/p",
      byProject: { "/p": { ...emptyProjectSlice(), workflows: [wf] } },
      selection: { kind: "workflow", name: "feature-dev" },
    };
    expect(selectedWorkflow(s)?.name).toBe("feature-dev");
    expect(selectedWorkflow(initialState())).toBeNull();
  });

  it("activeTask resolves the selected task, null otherwise", () => {
    expect(activeTask(taskState("t1", "work"))?.id).toBe("t1");
    expect(activeTask(initialState())).toBeNull();
  });
});

describe("tasksByRecency", () => {
  it("sorts by updated_at desc, then created_at desc, then id", () => {
    const tasks = [
      mkTask({ id: "a", updated_at: "2024-01-01T00:00:00Z" }),
      mkTask({ id: "b", updated_at: "2024-01-03T00:00:00Z" }),
      mkTask({ id: "c", updated_at: "2024-01-02T00:00:00Z" }),
    ];
    expect(tasksByRecency(tasks).map((t) => t.id)).toEqual(["b", "c", "a"]);
  });

  it("breaks updated_at ties by created_at desc, then id asc", () => {
    const tasks = [
      mkTask({ id: "z", updated_at: "2024-01-01T00:00:00Z", created_at: "2024-01-01T00:00:00Z" }),
      mkTask({ id: "y", updated_at: "2024-01-01T00:00:00Z", created_at: "2024-01-02T00:00:00Z" }),
      mkTask({ id: "x", updated_at: "2024-01-01T00:00:00Z", created_at: "2024-01-02T00:00:00Z" }),
    ];
    // y and x tie on updated_at and created_at, so id breaks it (x before y);
    // both sort ahead of z (older created_at).
    expect(tasksByRecency(tasks).map((t) => t.id)).toEqual(["x", "y", "z"]);
  });

  it("does not mutate the input array", () => {
    const tasks = [
      mkTask({ id: "a", updated_at: "2024-01-01T00:00:00Z" }),
      mkTask({ id: "b", updated_at: "2024-01-03T00:00:00Z" }),
    ];
    tasksByRecency(tasks);
    expect(tasks.map((t) => t.id)).toEqual(["a", "b"]);
  });
});
