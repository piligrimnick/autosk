// state/selectors.ts — derived views over the normalized store. The big one is
// `buildTimeline`: the autosk-specific "conversation" — the selected task's
// jobs' transcripts concatenated chronologically, with comments and
// step-signals interleaved by timestamp (plan §6 "Center task-timeline").

import type { Comment, Job, MessageEvent, Signal, TaskView, Workflow } from "@/types";
import type { AppState, ProjectSlice } from "./types";
import { emptyExtras, emptyProjectSlice } from "./types";
import { selectedSessionJobId, selectedTaskId, selectedWorkflowName } from "./selection";

export function activeSlice(state: AppState): ProjectSlice {
  if (!state.activeProject) return emptyProjectSlice();
  return state.byProject[state.activeProject] ?? emptyProjectSlice();
}

export function activeTasks(state: AppState): TaskView[] {
  const slice = activeSlice(state);
  return slice.taskOrder.map((id) => slice.tasks[id]).filter(Boolean);
}

/** The selected task id, or null when the selection is not a task. */
export function activeTaskId(state: AppState): string | null {
  return selectedTaskId(state.selection);
}

export function activeTask(state: AppState): TaskView | null {
  const id = activeTaskId(state);
  if (!id) return null;
  const slice = activeSlice(state);
  return slice.tasks[id] ?? null;
}

/** The Job backing the selected session, or null. */
export function selectedSessionJob(state: AppState): Job | null {
  const id = selectedSessionJobId(state.selection);
  if (!id) return null;
  return state.jobs[id] ?? null;
}

/** The selected workflow (looked up by name in the active project), or null. */
export function selectedWorkflow(state: AppState): Workflow | null {
  const name = selectedWorkflowName(state.selection);
  if (!name) return null;
  return activeSlice(state).workflows.find((w) => w.name === name) ?? null;
}

/** All sessions (jobs) of the active project, newest first (Sessions panel). */
export function sessionsForProject(state: AppState): Job[] {
  const root = state.activeProject;
  if (!root) return [];
  const order = state.sessionOrderByProject[root] ?? [];
  return order.map((id) => state.jobs[id]).filter(Boolean);
}

/** Status display order for the sidebar groups. */
export const STATUS_ORDER = ["work", "human", "new", "done", "cancel"] as const;

export interface StatusGroup {
  status: string;
  tasks: TaskView[];
}

/** Group a project's tasks by status in STATUS_ORDER, dropping empty groups. */
export function groupByStatus(tasks: TaskView[]): StatusGroup[] {
  const groups: StatusGroup[] = [];
  for (const status of STATUS_ORDER) {
    const inGroup = tasks.filter((t) => t.status === status);
    if (inGroup.length > 0) {
      groups.push({ status, tasks: inGroup });
    }
  }
  // Any unknown status falls into a trailing catch-all group.
  const known = new Set(STATUS_ORDER as readonly string[]);
  const other = tasks.filter((t) => !known.has(t.status));
  if (other.length > 0) {
    groups.push({ status: "other", tasks: other });
  }
  return groups;
}

export function jobsForTask(state: AppState, taskId: string | null): Job[] {
  if (!taskId) return [];
  const extras = state.extrasByTask[taskId] ?? emptyExtras();
  // Prefer the normalized job map (carries live status/streaming updates).
  return extras.jobs
    .map((j) => state.jobs[j.job_id] ?? j)
    .slice()
    .sort((a, b) => cmpTs(a.created_at, b.created_at));
}

export interface Activity {
  running: boolean;
  streaming: boolean;
}

/** Lightweight running/streaming flags for a task, derived from the job map. */
export function taskActivity(state: AppState, taskId: string): Activity {
  let running = false;
  let streaming = false;
  for (const job of Object.values(state.jobs)) {
    if (job.task_id !== taskId) continue;
    if (job.status === "running" || job.status === "queued") {
      running = true;
      if (job.streaming) streaming = true;
    }
  }
  return { running, streaming };
}

/**
 * Build a `taskId -> Activity` map in a SINGLE pass over the (global) job map.
 * The sidebar renders one row per task across every visited project, so calling
 * `taskActivity` per row is O(tasks × jobs); compute this once per render and
 * look up instead.
 */
export function taskActivityMap(state: AppState): Map<string, Activity> {
  const map = new Map<string, Activity>();
  for (const job of Object.values(state.jobs)) {
    if (job.status !== "running" && job.status !== "queued") continue;
    const cur = map.get(job.task_id) ?? { running: false, streaming: false };
    cur.running = true;
    if (job.streaming) cur.streaming = true;
    map.set(job.task_id, cur);
  }
  return map;
}

const NO_ACTIVITY: Activity = { running: false, streaming: false };

/** Per-task lookup against a precomputed activity map. */
export function activityOf(map: Map<string, Activity>, taskId: string): Activity {
  return map.get(taskId) ?? NO_ACTIVITY;
}

/** The newest non-terminal (running/queued) job for a task, if any. */
export function runningJob(state: AppState, taskId: string | null): Job | null {
  const jobs = jobsForTask(state, taskId);
  const live = jobs.filter((j) => j.status === "running" || j.status === "queued");
  if (live.length === 0) return null;
  return live[live.length - 1];
}

// ---- timeline -------------------------------------------------------------

export type TimelineItem =
  | { kind: "job-start"; ts: number; job: Job }
  | { kind: "message"; ts: number; jobId: string; event: MessageEvent; eventIndex: number }
  | { kind: "comment"; ts: number; comment: Comment }
  | { kind: "signal"; ts: number; signal: Signal };

function tsMillis(ts: string | null | undefined, fallback = 0): number {
  if (!ts) return fallback;
  const n = Date.parse(ts);
  return Number.isNaN(n) ? fallback : n;
}

function cmpTs(a: string | null | undefined, b: string | null | undefined): number {
  return tsMillis(a) - tsMillis(b);
}

/**
 * Build the task timeline: every job's transcript messages, plus comments and
 * step-signals, interleaved by timestamp. Jobs are delimited by a `job-start`
 * marker. Message timestamps fall back to the owning job's start time so an
 * untimestamped transcript line still sorts in the right cluster (and keeps its
 * within-job order via a secondary index).
 */
export function buildTimeline(state: AppState, taskId: string | null): TimelineItem[] {
  if (!taskId) return [];
  const extras = state.extrasByTask[taskId] ?? emptyExtras();
  const jobs = jobsForTask(state, taskId);
  const items: TimelineItem[] = [];

  for (const job of jobs) {
    const jobTs = tsMillis(job.started_at ?? job.created_at, 0);
    items.push({ kind: "job-start", ts: jobTs, job });
    const msgs = state.messagesByJob[job.job_id] ?? [];
    msgs.forEach((event, idx) => {
      items.push({
        kind: "message",
        ts: tsMillis(event.ts, jobTs) + idx * 0.001, // keep stable within-job order
        jobId: job.job_id,
        event,
        eventIndex: idx,
      });
    });
  }

  for (const comment of extras.comments) {
    items.push({ kind: "comment", ts: tsMillis(comment.created_at), comment });
  }
  for (const signal of extras.signals) {
    items.push({ kind: "signal", ts: tsMillis(signal.created_at), signal });
  }

  items.sort((a, b) => a.ts - b.ts);
  return items;
}

/**
 * A stable React key for a timeline item. buildTimeline interleaves by
 * timestamp, so a backdated comment / late signal can land mid-list; keying by
 * identity (not array index) keeps existing rows mounted (preserves selection,
 * react-markdown subtrees, scroll) when an item is inserted in the middle.
 */
export function timelineKey(item: TimelineItem): string {
  switch (item.kind) {
    case "job-start":
      return `job:${item.job.job_id}`;
    case "message":
      return `m:${item.jobId}:${item.eventIndex}`;
    case "comment":
      return `c:${item.comment.id}`;
    case "signal":
      return `s:${item.signal.transition_id}`;
  }
}

/**
 * The unified composer mode, driven by the SELECTED ENTITY (redesign plan §6.4,
 * decision #5):
 *   - session selected + running/queued job → "steer" (steer/follow-up/abort)
 *   - session selected + terminal job       → "readonly"
 *   - task selected                         → task-status composer
 *     ("new" enroll / "human" resume / "work" comment / terminal reopen),
 *     ignoring any running job (the session view is where you steer)
 *   - nothing (or workflow) selected        → "none"
 */
export type ComposerMode = "steer" | "readonly" | "new" | "human" | "enrolled" | "terminal" | "none";

export function composerMode(state: AppState): ComposerMode {
  const sel = state.selection;
  if (sel.kind === "session") {
    const job = state.jobs[sel.jobId];
    if (job && (job.status === "running" || job.status === "queued")) return "steer";
    return "readonly";
  }
  if (sel.kind === "task") {
    const task = activeSlice(state).tasks[sel.taskId];
    if (!task) return "none";
    switch (task.status) {
      case "human":
        return "human";
      case "new":
        return "new";
      case "work":
        return "enrolled";
      default:
        return "terminal";
    }
  }
  return "none";
}


