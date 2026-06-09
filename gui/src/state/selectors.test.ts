// Unit tests for the derived selectors (pure; no browser, no daemon). These
// lock the timeline interleave/ordering, composer-mode precedence, status
// grouping, running-job selection, and stable timeline keys — several of which
// back the reconnect / first-load fixes.

import { describe, it, expect } from "vitest";
import {
  activeTask,
  buildTimeline,
  composerMode,
  runningJob,
  selectedSessionJob,
  selectedWorkflow,
  sessionsForProject,
  tasksByRecency,
  timelineKey,
} from "./selectors";
import { emptyProjectSlice, initialState, type AppState } from "./types";
import type { Comment, Job, MessageEvent, Signal, TaskView, Workflow } from "@/types";

function mkWorkflow(name: string): Workflow {
  return {
    id: `wf-${name}`,
    name,
    description: "",
    is_synthetic: false,
    first_step: "dev",
    first_step_id: "s1",
    steps: [],
    task_count: 0,
    isolation: "none",
    non_terminal_task_count: 0,
    non_terminal_tasks: [],
    created_at: "2024-01-01T00:00:00Z",
  };
}

function mkJob(p: Partial<Job> & Pick<Job, "job_id" | "task_id">): Job {
  return {
    step_id: "s1",
    status: "running",
    corrections_used: 0,
    max_corrections: 0,
    created_at: "2024-01-01T00:00:00Z",
    duration_ms: 0,
    attach_count: 0,
    streaming: false,
    workflow_name: "wf",
    step_name: "dev",
    agent_name: "a",
    ...p,
  };
}

function mkTask(p: Partial<TaskView> & Pick<TaskView, "id">): TaskView {
  return {
    title: "t",
    description: "",
    status: "new",
    priority: 2,
    author_id: "u",
    author_name: "u",
    workflow_id: "",
    workflow_name: "",
    current_step_id: "",
    step_name: "",
    agent_id: "",
    agent_name: "",
    blocked: false,
    blocked_by: [],
    blocks: [],
    comment_count: 0,
    metadata: null,
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
    ...p,
  };
}

function mkComment(p: Partial<Comment> & Pick<Comment, "id" | "created_at">): Comment {
  return { task_id: "t1", author_id: "u", author_name: "u", text: "hi", ...p };
}

function mkSignal(p: Partial<Signal> & Pick<Signal, "transition_id" | "created_at">): Signal {
  return {
    task_id: "t1",
    job_id: "j1",
    step_id: "s1",
    step_name: "dev",
    workflow_id: "wf",
    workflow_name: "wf",
    target: "review",
    agent_id: "a",
    agent_name: "a",
    ...p,
  };
}

function msg(text: string): MessageEvent {
  return { kind: "assistant_text", text };
}

/** A state with one task's extras + jobs map + transcript wired up. */
function stateWith(opts: {
  taskId: string;
  jobs?: Job[];
  comments?: Comment[];
  signals?: Signal[];
  messages?: Record<string, MessageEvent[]>;
}): AppState {
  const s = initialState();
  const jobs = opts.jobs ?? [];
  return {
    ...s,
    jobs: Object.fromEntries(jobs.map((j) => [j.job_id, j])),
    messagesByJob: opts.messages ?? {},
    extrasByTask: {
      [opts.taskId]: {
        jobs,
        comments: opts.comments ?? [],
        signals: opts.signals ?? [],
        loaded: true,
      },
    },
  };
}

describe("buildTimeline", () => {
  it("interleaves job-start, messages, comments and signals by timestamp", () => {
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "done", started_at: "2024-01-01T00:00:00Z" });
    const s = stateWith({
      taskId: "t1",
      jobs: [job],
      comments: [mkComment({ id: 1, created_at: "2024-01-01T00:00:05Z" })],
      signals: [mkSignal({ transition_id: 1, created_at: "2024-01-01T00:00:10Z" })],
      messages: { j1: [msg("first"), msg("second")] },
    });

    const items = buildTimeline(s, "t1");
    expect(items.map((i) => i.kind)).toEqual(["job-start", "message", "message", "comment", "signal"]);
  });

  it("preserves within-job message order when message timestamps are absent", () => {
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "done", started_at: "2024-01-01T00:00:00Z" });
    const s = stateWith({ taskId: "t1", jobs: [job], messages: { j1: [msg("a"), msg("b"), msg("c")] } });
    const texts = buildTimeline(s, "t1")
      .filter((i) => i.kind === "message")
      .map((i) => (i.kind === "message" ? i.event.text : ""));
    expect(texts).toEqual(["a", "b", "c"]);
  });

  it("returns [] for a null task", () => {
    expect(buildTimeline(initialState(), null)).toEqual([]);
  });
});

/** A state with the active project's slice carrying one task + a task selection. */
function taskState(taskId: string, status: string, jobs: Job[] = []): AppState {
  const s = initialState();
  return {
    ...s,
    activeProject: "/p",
    byProject: {
      "/p": { ...emptyProjectSlice(), tasks: { [taskId]: mkTask({ id: taskId, status }) }, taskOrder: [taskId] },
    },
    jobs: Object.fromEntries(jobs.map((j) => [j.job_id, j])),
    selection: { kind: "task", taskId },
  };
}

describe("composerMode (entity-driven)", () => {
  it("returns 'none' when nothing is selected", () => {
    expect(composerMode(initialState())).toBe("none");
  });

  it("session + running/queued job → 'steer'", () => {
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "running" });
    const s: AppState = { ...initialState(), jobs: { j1: job }, selection: { kind: "session", jobId: "j1" } };
    expect(composerMode(s)).toBe("steer");
    const queued: AppState = { ...s, jobs: { j1: { ...job, status: "queued" } } };
    expect(composerMode(queued)).toBe("steer");
  });

  it("session + terminal job → 'readonly'", () => {
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "done" });
    const s: AppState = { ...initialState(), jobs: { j1: job }, selection: { kind: "session", jobId: "j1" } };
    expect(composerMode(s)).toBe("readonly");
  });

  it("task status drives the mode, ignoring any running job (decision #5)", () => {
    const running = [mkJob({ job_id: "j1", task_id: "t1", status: "running" })];
    expect(composerMode(taskState("t1", "human", running))).toBe("human");
    expect(composerMode(taskState("t1", "new"))).toBe("new");
    expect(composerMode(taskState("t1", "work"))).toBe("enrolled");
    expect(composerMode(taskState("t1", "done"))).toBe("terminal");
    expect(composerMode(taskState("t1", "cancel"))).toBe("terminal");
  });
});

describe("sessionsForProject / selectedSessionJob", () => {
  it("returns the active project's jobs in session order", () => {
    const j1 = mkJob({ job_id: "j1", task_id: "t1" });
    const j2 = mkJob({ job_id: "j2", task_id: "t2" });
    const s: AppState = {
      ...initialState(),
      activeProject: "/p",
      jobs: { j1, j2 },
      sessionOrderByProject: { "/p": ["j2", "j1"] },
    };
    expect(sessionsForProject(s).map((j) => j.job_id)).toEqual(["j2", "j1"]);
    expect(sessionsForProject(initialState())).toEqual([]);
  });

  it("resolves the selected session's job", () => {
    const j1 = mkJob({ job_id: "j1", task_id: "t1" });
    const s: AppState = { ...initialState(), jobs: { j1 }, selection: { kind: "session", jobId: "j1" } };
    expect(selectedSessionJob(s)?.job_id).toBe("j1");
    expect(selectedSessionJob(initialState())).toBeNull();
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

describe("runningJob", () => {
  it("returns the newest non-terminal job", () => {
    const older = mkJob({ job_id: "old", task_id: "t1", status: "running", created_at: "2024-01-01T00:00:00Z" });
    const newer = mkJob({ job_id: "new", task_id: "t1", status: "running", created_at: "2024-01-01T00:05:00Z" });
    const s = stateWith({ taskId: "t1", jobs: [older, newer] });
    expect(runningJob(s, "t1")?.job_id).toBe("new");
  });

  it("returns null when every job is terminal", () => {
    const s = stateWith({ taskId: "t1", jobs: [mkJob({ job_id: "j1", task_id: "t1", status: "done" })] });
    expect(runningJob(s, "t1")).toBeNull();
  });

  it("treats a QUEUED job as live", () => {
    const s = stateWith({ taskId: "t1", jobs: [mkJob({ job_id: "j1", task_id: "t1", status: "queued" })] });
    expect(runningJob(s, "t1")?.job_id).toBe("j1");
  });

  it("picks the newest live job (a queued job over an older terminal one)", () => {
    const older = mkJob({ job_id: "old", task_id: "t1", status: "done", created_at: "2024-01-01T00:00:00Z" });
    const newer = mkJob({ job_id: "new", task_id: "t1", status: "queued", created_at: "2024-01-01T00:05:00Z" });
    const s = stateWith({ taskId: "t1", jobs: [older, newer] });
    expect(runningJob(s, "t1")?.job_id).toBe("new");
  });
});

describe("timelineKey", () => {
  it("derives stable identity keys per item kind", () => {
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "done", started_at: "2024-01-01T00:00:00Z" });
    const s = stateWith({
      taskId: "t1",
      jobs: [job],
      comments: [mkComment({ id: 7, created_at: "2024-01-01T00:00:05Z" })],
      signals: [mkSignal({ transition_id: 9, created_at: "2024-01-01T00:00:10Z" })],
      messages: { j1: [msg("a")] },
    });
    const keys = buildTimeline(s, "t1").map(timelineKey);
    expect(keys).toEqual(["job:j1", "m:j1:0", "c:7", "s:9"]);
    // Keys are unique (no index collisions).
    expect(new Set(keys).size).toBe(keys.length);
  });
});
