// services/ipc.ts — the ONE place the frontend crosses the Tauri bridge with
// `invoke` (plan §6 "IPC discipline"). Every autoskd JSON-RPC method gets a
// typed shim here; the rest of the app calls these functions and never touches
// `invoke` directly (enforced by scripts/check-ipc-discipline.mjs + eslint).
//
// All project-scoped methods carry a selector `{ cwd }` (mirroring the old
// `X-Autosk-Cwd` header). The Rust command `daemon_request` is transport-
// agnostic: it dials the local UDS daemon or the remote TCP daemon depending on
// the app's backend mode, so this file is identical for local and remote.

import { invoke } from "@tauri-apps/api/core";
import type {
  Agent,
  AppSettings,
  Comment,
  DaemonStatus,
  Health,
  Job,
  MessageEvent,
  ProjectInfo,
  Signal,
  TaskView,
  UpdateIsolationReport,
  VersionInfo,
  Workflow,
} from "@/types";

/** Structured error surfaced from autoskd (mirror of autosk-proto ErrorObject). */
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

// Mirror of autosk-proto::rpc::error_codes (the subset the UI branches on).
export const ErrorCode = {
  MethodNotFound: -32601,
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
    // Try to parse a JSON-encoded ErrorObject the Rust command may stringify.
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
  return daemonRequest<VersionInfo>("version");
}

export function healthz(cwd?: string, all = false): Promise<Health> {
  return daemonRequest<Health>("healthz", all ? { all: true } : sel(cwd ?? ""));
}

// ---- project -------------------------------------------------------------

export function projectList(): Promise<ProjectInfo[]> {
  return daemonRequest<ProjectInfo[]>("project.list");
}

export function projectAdd(cwd: string): Promise<ProjectInfo> {
  return daemonRequest<ProjectInfo>("project.add", sel(cwd));
}

export function projectRemove(root: string): Promise<{ removed: boolean }> {
  return daemonRequest("project.remove", { root });
}

export function projectInit(
  cwd: string,
  skipBootstrap = false,
): Promise<{ root: string; db_path: string; schema_version: number; bootstrapped: boolean }> {
  return daemonRequest("project.init", sel(cwd, { skip_bootstrap: skipBootstrap }));
}

// ---- task (reads) --------------------------------------------------------

export interface TaskFilter {
  statuses?: string[];
  priority?: number;
  workflow_id?: string;
  agent_name?: string;
  author_name?: string;
  step_agent_name?: string;
  search?: string;
}

export function taskList(cwd: string, filter: TaskFilter = {}): Promise<TaskView[]> {
  return daemonRequest<TaskView[]>("task.list", sel(cwd, filter as Record<string, unknown>));
}

export function taskGet(cwd: string, id: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.get", sel(cwd, { id }));
}

export function taskReady(cwd: string, limit = 0): Promise<TaskView[]> {
  return daemonRequest<TaskView[]>("task.ready", sel(cwd, { limit }));
}

// ---- task (writes) -------------------------------------------------------

export interface CreateTaskArgs {
  title: string;
  description?: string;
  priority?: number;
  blocks?: string[];
  blocked_by?: string[];
  workflow?: string;
  agent?: string;
  step?: string;
}

export function taskCreate(cwd: string, args: CreateTaskArgs): Promise<TaskView> {
  return daemonRequest<TaskView>("task.create", sel(cwd, { source: "gui", ...args }));
}

export function taskUpdate(
  cwd: string,
  id: string,
  patch: { title?: string; description?: string; priority?: number; status?: string },
): Promise<TaskView> {
  return daemonRequest<TaskView>("task.update", sel(cwd, { id, ...patch }));
}

export function taskSetStatus(cwd: string, id: string, status: string): Promise<TaskView> {
  return daemonRequest<TaskView>("task.setStatus", sel(cwd, { id, status }));
}

export function taskSetTitleDescription(
  cwd: string,
  id: string,
  title: string,
  description: string,
): Promise<TaskView> {
  return daemonRequest<TaskView>("task.setTitleDescription", sel(cwd, { id, title, description }));
}

export function taskSetPriority(cwd: string, id: string, priority: number): Promise<TaskView> {
  return daemonRequest<TaskView>("task.setPriority", sel(cwd, { id, priority }));
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

export function taskEnroll(
  cwd: string,
  id: string,
  args: { workflow?: string; agent?: string; step?: string; base_ref?: string },
): Promise<TaskView & { base_ref_ignored?: boolean }> {
  return daemonRequest("task.enroll", sel(cwd, { id, source: "gui", ...args }));
}

export function taskResume(cwd: string, id: string, toStep = ""): Promise<TaskView> {
  return daemonRequest<TaskView>("task.resume", sel(cwd, { id, to_step: toStep, source: "gui" }));
}

export function taskBlock(cwd: string, id: string, blockers: string[]): Promise<{ ok: boolean }> {
  return daemonRequest("task.block", sel(cwd, { id, blockers, source: "gui" }));
}

export function taskUnblock(cwd: string, id: string, blockers: string[]): Promise<{ ok: boolean }> {
  return daemonRequest("task.unblock", sel(cwd, { id, blockers, source: "gui" }));
}

export function taskSetMetadata(
  cwd: string,
  id: string,
  metadata: Record<string, unknown>,
): Promise<{ task: TaskView; changed: boolean }> {
  // replace_all wholesale-replaces the metadata object (lazy "M" hotkey parity).
  return daemonRequest("task.metadata.set", sel(cwd, { id, value: metadata, replace_all: true, source: "gui" }));
}

// ---- comments ------------------------------------------------------------

export function commentList(cwd: string, taskId: string): Promise<Comment[]> {
  return daemonRequest<Comment[]>("comment.list", sel(cwd, { task_id: taskId }));
}

export function commentAdd(cwd: string, taskId: string, text: string): Promise<Comment> {
  return daemonRequest<Comment>("comment.add", sel(cwd, { task_id: taskId, text, source: "gui" }));
}

// ---- workflows -----------------------------------------------------------

export function workflowList(cwd: string, includeSynthetic = false): Promise<Workflow[]> {
  return daemonRequest<Workflow[]>("workflow.list", sel(cwd, { include_synthetic: includeSynthetic }));
}

export function workflowGet(cwd: string, name: string): Promise<Workflow> {
  return daemonRequest<Workflow>("workflow.get", sel(cwd, { name }));
}

export function workflowCreate(cwd: string, json: string, noInstall = false): Promise<{ name: string }> {
  return daemonRequest("workflow.create", sel(cwd, { json, no_install: noInstall, source: "gui" }));
}

export function workflowDelete(cwd: string, name: string): Promise<{ ok: boolean }> {
  return daemonRequest("workflow.delete", sel(cwd, { name, source: "gui" }));
}

export function workflowUpdateIsolation(
  cwd: string,
  name: string,
  mode: string,
  force = false,
  dryRun = false,
): Promise<UpdateIsolationReport> {
  return daemonRequest("workflow.updateIsolation", sel(cwd, { name, mode, force, dry_run: dryRun, source: "gui" }));
}

// ---- agents --------------------------------------------------------------

export function agentList(cwd: string): Promise<Agent[]> {
  return daemonRequest<Agent[]>("agent.list", sel(cwd));
}

export function agentInstall(cwd: string, name: string, version = "", spec = ""): Promise<Agent> {
  return daemonRequest<Agent>("agent.install", sel(cwd, { name, version, spec }));
}

export function agentUninstall(cwd: string, name: string, force = false): Promise<{ ok: boolean }> {
  return daemonRequest("agent.uninstall", sel(cwd, { name, force }));
}

// ---- jobs ----------------------------------------------------------------

export interface JobFilter {
  task_id?: string;
  workflow_id?: string;
  statuses?: string[];
  limit?: number;
}

export function jobList(cwd: string, filter: JobFilter = {}): Promise<Job[]> {
  return daemonRequest<Job[]>("job.list", sel(cwd, filter as Record<string, unknown>));
}

export function jobGet(cwd: string, id: string): Promise<Job> {
  return daemonRequest<Job>("job.get", sel(cwd, { id }));
}

export function jobMessages(cwd: string, jobId: string, full = true, limit = 0): Promise<MessageEvent[]> {
  return daemonRequest<MessageEvent[]>("job.messages", sel(cwd, { job_id: jobId, full, limit }));
}

export function jobCancel(cwd: string, jobId: string): Promise<Job> {
  return daemonRequest<Job>("job.cancel", sel(cwd, { job_id: jobId }));
}

export function jobInput(
  cwd: string,
  jobId: string,
  message: string,
  behavior: "" | "steer" | "follow_up" = "",
): Promise<{ job_id: string; dispatched: string }> {
  return daemonRequest("job.input", sel(cwd, { job_id: jobId, message, streaming_behavior: behavior }));
}

export function jobAbort(cwd: string, jobId: string): Promise<{ job_id: string; ok: boolean }> {
  return daemonRequest("job.abort", sel(cwd, { job_id: jobId }));
}

export interface SubscribeArgs {
  attach?: boolean;
  full?: boolean;
  limit?: number;
  from_event_id?: number;
}

/**
 * Begin a live transcript tail for a job. The daemon pushes `job-event`
 * notifications on the persistent connection; the Rust backend re-emits them as
 * a Tauri `job-event` which the events.ts hub fans out. Returns once subscribed.
 */
export function jobSubscribe(cwd: string, jobId: string, args: SubscribeArgs = {}): Promise<unknown> {
  return daemonRequest("job.subscribe", sel(cwd, { job_id: jobId, ...args }));
}

export function jobUnsubscribe(cwd: string, jobId: string): Promise<unknown> {
  return daemonRequest("job.unsubscribe", sel(cwd, { job_id: jobId }));
}

// ---- signals -------------------------------------------------------------

export function signalsForTask(cwd: string, taskId: string): Promise<Signal[]> {
  return daemonRequest<Signal[]>("signal.forTask", sel(cwd, { task_id: taskId }));
}

export function signalsForJob(cwd: string, jobId: string): Promise<Signal[]> {
  return daemonRequest<Signal[]>("signal.forJob", sel(cwd, { job_id: jobId }));
}

// ---- step / sql / maint --------------------------------------------------

export function stepNext(cwd: string, id: string, to: string): Promise<Record<string, unknown>> {
  return daemonRequest("step.next", sel(cwd, { id, to }));
}

export function sqlQuery(cwd: string, query: string): Promise<{ columns: string[]; rows: unknown[][] }> {
  return daemonRequest("sql.query", sel(cwd, { query }));
}

export function sqlExec(cwd: string, query: string): Promise<{ rows_affected: number }> {
  return daemonRequest("sql.exec", sel(cwd, { query }));
}
