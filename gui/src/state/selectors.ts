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

/**
 * Tasks sorted by most-recently-updated first (the Tasks panel renders one flat,
 * ungrouped list). `updated_at` is the primary key; `created_at` then `id` break
 * ties so the order is stable across renders. `tsMillis` (declared below) is a
 * hoisted function, so referencing it here is fine.
 */
export function tasksByRecency(tasks: TaskView[]): TaskView[] {
  return tasks.slice().sort((a, b) => {
    const byUpdated = tsMillis(b.updated_at) - tsMillis(a.updated_at);
    if (byUpdated !== 0) return byUpdated;
    const byCreated = tsMillis(b.created_at) - tsMillis(a.created_at);
    if (byCreated !== 0) return byCreated;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
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
 * The unified composer mode, driven by the SELECTED ENTITY:
 *   - session selected + running/queued job → "steer" (steer + abort; cancel
 *     and abort live in the session header, the composer is just the input)
 *   - session selected + terminal job       → "none" (read-only transcript)
 *   - task selected (any status)            → "comment" (a single comment box;
 *     enroll/resume/reopen moved to the Enroll button in the task header)
 *   - nothing (or workflow) selected        → "none"
 */
export type ComposerMode = "steer" | "comment" | "none";

export function composerMode(state: AppState): ComposerMode {
  const sel = state.selection;
  if (sel.kind === "session") {
    const job = state.jobs[sel.jobId];
    if (job && (job.status === "running" || job.status === "queued")) return "steer";
    return "none";
  }
  if (sel.kind === "task") {
    return activeSlice(state).tasks[sel.taskId] ? "comment" : "none";
  }
  return "none";
}


