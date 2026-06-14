// services/ipc.ts — the ONE place the frontend crosses the Tauri bridge with
// `invoke` (plan §6 "IPC discipline"). Every autoskd JSON-RPC method gets a
// typed shim here; the rest of the app calls these functions and never touches
// `invoke` directly (enforced by scripts/check-ipc-discipline.mjs + eslint).
//
// Proto v2 (daemon/sdk/src/proto.ts): namespaced method names (meta.* /
// project.* / task.* / task.comment.* / registry.* / session.*), a `{ cwd }`
// selector on every project-scoped method (no database-path selector, no
// write-source discriminator), and RFC3339 UTC on the wire. The `daemon_request`
// Rust command is transport-agnostic (dials the local UDS daemon or the remote
// TCP daemon by backend mode), so this file is identical for both.

import { invoke } from "@tauri-apps/api/core";
import type {
  AgentInfo,
  AppSettings,
  Comment,
  DaemonStatus,
  Health,
  ProjectDiagnostics,
  ProjectInfo,
  SessionMeta,
  StepTarget,
  TaskFilter,
  TaskView,
  TranscriptLine,
  VersionInfo,
  WorkflowInfo,
} from "@/types";

/** Structured error surfaced from autoskd (mirror of proto-v2 RpcError). */
export class DaemonError extends Error {
  code: number;
  details?: unknown;
  constructor(code: number, message: string, details?: unknown) {
    super(message);
    this.name = "DaemonError";
    this.code = code;
    this.details = details;
  }
}

// Mirror of proto-v2 ErrorCodes (the subset the UI branches on).
export const ErrorCode = {
  MethodNotFound: -32601,
  InvalidParams: -32602,
  ProjectNotFound: 1001,
  InvalidProject: 1002,
  NotFound: 1003,
  Conflict: 1004,
} as const;

/**
 * The single generic JSON-RPC chokepoint. Forwards `method` + `params` to the
 * Rust `daemon_request` command, which performs the local-vs-remote switch and
 * returns the raw RPC `result` (or throws a structured `DaemonError`).
 */
export async function daemonRequest<T = unknown>(
  method: string,
  params: Record<string, unknown> = {},
): Promise<T> {
  try {
    return await invoke<T>("daemon_request", { method, params });
  } catch (err) {
    throw normalizeError(err);
  }
}

/**
 * The Rust side returns daemon errors as a JSON object `{ code, message,
 * details }` (serialised through Tauri's error channel as a string or object).
 * Normalise into a `DaemonError` so callers can branch on `code`. Exported for
 * unit tests (the whole UI's error branching depends on it).
 */
export function normalizeError(err: unknown): Error {
  if (err instanceof Error) {
    return err;
  }
  if (typeof err === "object" && err !== null) {
    const o = err as Record<string, unknown>;
    if (typeof o.code === "number" && typeof o.message === "string") {
      return new DaemonError(o.code, o.message, o.details);
    }
    if (typeof o.message === "string") {
      return new Error(o.message);
    }
  }
  if (typeof err === "string") {
    // Try to parse a JSON-encoded RpcError the Rust command may stringify.
    try {
      const parsed = JSON.parse(err);
      if (parsed && typeof parsed.code === "number") {
        return new DaemonError(parsed.code, parsed.message ?? err, parsed.details);
      }
    } catch {
      /* not JSON; fall through */
    }
    return new Error(err);
  }
  return new Error(String(err));
}

/** Merge a selector cwd with method-specific params. */
function sel(cwd: string, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return cwd ? { cwd, ...extra } : { ...extra };
}

// ---- app settings & connection (Tauri-local commands) ---------------------

export function getAppSettings(): Promise<AppSettings> {
  return invoke<AppSettings>("get_app_settings");
}

export function updateAppSettings(settings: AppSettings): Promise<AppSettings> {
  return invoke<AppSettings>("update_app_settings", { settings });
}

/** Force a (re)connect to the configured daemon; returns the resulting status. */
export function reconnectDaemon(): Promise<DaemonStatus> {
  return invoke<DaemonStatus>("reconnect_daemon");
}

export function getDaemonStatus(): Promise<DaemonStatus> {
  return invoke<DaemonStatus>("daemon_status");
}

// ---- meta ----------------------------------------------------------------

export function version(): Promise<VersionInfo> {
  return daemonRequest<VersionInfo>("meta.version");
}

export function healthz(): Promise<Health> {
  return daemonRequest<Health>("meta.healthz");
}

// ---- project -------------------------------------------------------------

export function projectList(): Promise<ProjectInfo[]> {
  return daemonRequest<ProjectInfo[]>("project.list");
}

export function projectAdd(cwd: string, name?: string): Promise<ProjectInfo> {
  return daemonRequest<ProjectInfo>("project.add", sel(cwd, name ? { name } : {}));
}

export function projectRemove(root: string): Promise<{ ok: boolean }> {
  // The selector for project.remove IS the project root (cwd).
  return daemonRequest("project.remove", sel(root));
}

/**
 * Lay the `.autosk/` skeleton for a directory and register it. The daemon's
 * `project.init` already calls `project.add` internally (daemon.ts), so the
 * project surfaces in `project.list` without a separate add call.
 */
export function projectInit(cwd: string): Promise<ProjectInfo> {
  return daemonRequest<ProjectInfo>("project.init", sel(cwd));
}

export function projectDiagnostics(cwd: string): Promise<ProjectDiagnostics> {
  return daemonRequest<ProjectDiagnostics>("project.diagnostics", sel(cwd));
}

// ---- task (reads) --------------------------------------------------------

export function taskList(cwd: string, filter: TaskFilter = {}): Promise<TaskView[]> {
  return daemonRequest<TaskView[]>("task.list", sel(cwd, { filter }));
}

export function taskGet(cwd: string, id: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.get", sel(cwd, { id }));
}

// ---- task (writes) -------------------------------------------------------

export interface CreateTaskArgs {
  title: string;
  description?: string;
  blocked_by?: string[];
}

export function taskCreate(cwd: string, args: CreateTaskArgs): Promise<TaskView> {
  return daemonRequest<TaskView>("task.create", sel(cwd, { ...args }));
}

export function taskUpdate(
  cwd: string,
  id: string,
  patch: { title?: string; description?: string },
): Promise<TaskView> {
  return daemonRequest<TaskView>("task.update", sel(cwd, { id, ...patch }));
}

export function taskDone(cwd: string, id: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.done", sel(cwd, { id }));
}

export function taskCancel(cwd: string, id: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.cancel", sel(cwd, { id }));
}

export function taskReopen(cwd: string, id: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.reopen", sel(cwd, { id }));
}

/** Enroll into a named workflow XOR materialise a single-step agent run. */
export function taskEnroll(
  cwd: string,
  id: string,
  target: { workflow: string } | { agent: string },
): Promise<TaskView> {
  return daemonRequest<TaskView>("task.enroll", sel(cwd, { id, ...target }));
}

/** Resume a parked (`human`) task, optionally to a specific step/status target. */
export function taskResume(cwd: string, id: string, to?: StepTarget): Promise<TaskView> {
  return daemonRequest<TaskView>("task.resume", sel(cwd, to ? { id, to } : { id }));
}

export function taskBlock(cwd: string, id: string, blockedBy: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.block", sel(cwd, { id, blocked_by: blockedBy }));
}

export function taskUnblock(cwd: string, id: string, blockedBy: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.unblock", sel(cwd, { id, blocked_by: blockedBy }));
}

/** Open this project's task-change push for the live connection (front-end issued). */
export function taskSubscribe(cwd: string): Promise<{ ok: boolean }> {
  return daemonRequest("task.subscribe", sel(cwd));
}

export function taskUnsubscribe(cwd: string): Promise<{ ok: boolean }> {
  return daemonRequest("task.unsubscribe", sel(cwd));
}

// ---- comments ------------------------------------------------------------

export function commentList(cwd: string, taskId: string): Promise<Comment[]> {
  return daemonRequest<Comment[]>("task.comment.list", sel(cwd, { task_id: taskId }));
}

export function commentAdd(cwd: string, taskId: string, text: string): Promise<Comment> {
  return daemonRequest<Comment>("task.comment.add", sel(cwd, { task_id: taskId, text }));
}

export function commentEdit(cwd: string, taskId: string, commentId: string, text: string): Promise<Comment> {
  return daemonRequest<Comment>("task.comment.edit", sel(cwd, { task_id: taskId, comment_id: commentId, text }));
}

export function commentDelete(cwd: string, taskId: string, commentId: string): Promise<{ ok: boolean }> {
  return daemonRequest("task.comment.delete", sel(cwd, { task_id: taskId, comment_id: commentId }));
}

// ---- registry (workflows + agents are code; read-only) -------------------

export function workflowList(cwd: string): Promise<WorkflowInfo[]> {
  return daemonRequest<WorkflowInfo[]>("registry.workflow.list", sel(cwd));
}

export function workflowGet(cwd: string, name: string): Promise<WorkflowInfo> {
  return daemonRequest<WorkflowInfo>("registry.workflow.get", sel(cwd, { name }));
}

export function agentList(cwd: string): Promise<AgentInfo[]> {
  return daemonRequest<AgentInfo[]>("registry.agent.list", sel(cwd));
}

// ---- sessions ------------------------------------------------------------

export function sessionList(cwd: string, taskId?: string): Promise<SessionMeta[]> {
  return daemonRequest<SessionMeta[]>("session.list", sel(cwd, taskId ? { task_id: taskId } : {}));
}

export function sessionGet(cwd: string, id: string): Promise<SessionMeta> {
  return daemonRequest<SessionMeta>("session.get", sel(cwd, { id }));
}

export interface SessionTranscriptResult {
  entries: TranscriptLine[];
  next_line: number;
}

export function sessionTranscript(
  cwd: string,
  id: string,
  fromLine?: number,
  limit?: number,
): Promise<SessionTranscriptResult> {
  const extra: Record<string, unknown> = { id };
  if (fromLine !== undefined) extra.from_line = fromLine;
  if (limit !== undefined) extra.limit = limit;
  return daemonRequest<SessionTranscriptResult>("session.transcript", sel(cwd, extra));
}

/**
 * Begin a live transcript tail. The daemon replays from `fromLine` (default 1)
 * as `session-event` `message` frames on the persistent connection, then tails;
 * the Rust backend re-emits them as a Tauri `session-event` which the events.ts
 * hub fans out. Returns once subscribed.
 */
export function sessionSubscribe(cwd: string, id: string, fromLine?: number): Promise<{ ok: boolean }> {
  return daemonRequest("session.subscribe", sel(cwd, fromLine !== undefined ? { id, from_line: fromLine } : { id }));
}

export function sessionUnsubscribe(cwd: string, id: string): Promise<{ ok: boolean }> {
  return daemonRequest("session.unsubscribe", sel(cwd, { id }));
}

/**
 * Subscribe to a project's session lifecycle channel: the daemon pushes a
 * `session-changed` notification whenever any session in the project is created
 * or changes status (queued → running → terminal), so the Sessions panel stays
 * live WITHOUT a per-session `session.subscribe`. Per-connection state; re-issue
 * after a reconnect (like `task.subscribe`).
 */
export function sessionSubscribeProject(cwd: string): Promise<{ ok: boolean }> {
  return daemonRequest("session.subscribeProject", sel(cwd));
}

export function sessionUnsubscribeProject(cwd: string): Promise<{ ok: boolean }> {
  return daemonRequest("session.unsubscribeProject", sel(cwd));
}

export function sessionInput(
  cwd: string,
  id: string,
  message: string,
  kind: "steer" | "followup",
): Promise<{ ok: boolean }> {
  return daemonRequest("session.input", sel(cwd, { id, message, kind }));
}

export function sessionAbort(cwd: string, id: string): Promise<{ ok: boolean }> {
  return daemonRequest("session.abort", sel(cwd, { id }));
}
