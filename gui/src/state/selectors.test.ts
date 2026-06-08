// Unit tests for the derived selectors (pure; no browser, no daemon). These
// lock the timeline interleave/ordering, composer-mode precedence, status
// grouping, running-job selection, the one-pass activity map, and stable
// timeline keys — several of which back the reconnect / first-load fixes.

import { describe, it, expect } from "vitest";
import {
  activityOf,
  buildTimeline,
  composerMode,
  groupByStatus,
  runningJob,
  taskActivityMap,
  timelineKey,
} from "./selectors";
import { initialState, type AppState } from "./types";
import type { Comment, Job, MessageEvent, Signal, TaskView } from "@/types";

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

describe("composerMode", () => {
  it("returns 'new' for a null task", () => {
    expect(composerMode(initialState(), null)).toBe("new");
  });

  it("prioritises a running job over the task status", () => {
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "running" });
    const s = stateWith({ taskId: "t1", jobs: [job] });
    expect(composerMode(s, mkTask({ id: "t1", status: "human" }))).toBe("running");
  });

  it("a QUEUED job alone flips the composer into 'running' (Cancel/Abort surface)", () => {
    // The Cancel-job affordance lives in RunningComposer, which renders only
    // when composerMode==='running'. A queued job (no running run yet) must
    // still drive 'running' so a queued job is cancellable — the exact gap the
    // validator bounced on. Drive it from non-'running' task statuses.
    const job = mkJob({ job_id: "j1", task_id: "t1", status: "queued" });
    const s = stateWith({ taskId: "t1", jobs: [job] });
    expect(composerMode(s, mkTask({ id: "t1", status: "new" }))).toBe("running");
    expect(composerMode(s, mkTask({ id: "t1", status: "work" }))).toBe("running");
  });

  it("maps statuses to modes when no job is running", () => {
    const s = stateWith({ taskId: "t1", jobs: [] });
    expect(composerMode(s, mkTask({ id: "t1", status: "human" }))).toBe("human");
    expect(composerMode(s, mkTask({ id: "t1", status: "new" }))).toBe("new");
    expect(composerMode(s, mkTask({ id: "t1", status: "work" }))).toBe("enrolled");
    expect(composerMode(s, mkTask({ id: "t1", status: "done" }))).toBe("terminal");
    expect(composerMode(s, mkTask({ id: "t1", status: "cancel" }))).toBe("terminal");
  });
});

describe("groupByStatus", () => {
  it("orders groups by STATUS_ORDER, drops empties, and trails unknowns in 'other'", () => {
    const tasks = [
      mkTask({ id: "1", status: "done" }),
      mkTask({ id: "2", status: "work" }),
      mkTask({ id: "3", status: "weird" }),
      mkTask({ id: "4", status: "new" }),
    ];
    const groups = groupByStatus(tasks);
    expect(groups.map((g) => g.status)).toEqual(["work", "new", "done", "other"]);
    expect(groups.find((g) => g.status === "other")?.tasks.map((t) => t.id)).toEqual(["3"]);
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

describe("taskActivityMap / activityOf", () => {
  it("flags running + streaming per task in a single pass over the job map", () => {
    const s = stateWith({
      taskId: "t1",
      jobs: [
        mkJob({ job_id: "j1", task_id: "t1", status: "running", streaming: true }),
        mkJob({ job_id: "j2", task_id: "t2", status: "queued", streaming: false }),
        mkJob({ job_id: "j3", task_id: "t3", status: "done", streaming: true }),
      ],
    });
    const map = taskActivityMap(s);
    expect(activityOf(map, "t1")).toEqual({ running: true, streaming: true });
    expect(activityOf(map, "t2")).toEqual({ running: true, streaming: false });
    expect(activityOf(map, "t3")).toEqual({ running: false, streaming: false }); // terminal
    expect(activityOf(map, "missing")).toEqual({ running: false, streaming: false });
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
