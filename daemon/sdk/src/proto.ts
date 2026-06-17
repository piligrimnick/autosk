/**
 * Proto-v2 — the JSON-RPC v2 wire types (plan §4).
 *
 * Same envelope as v1: one JSON object per line; a `{cwd}` selector on
 * project-scoped methods; RFC3339 UTC timestamps on the wire. These types are
 * the SINGLE SOURCE OF TRUTH the Go (`internal/daemon/api`) and Tauri
 * (`gui/src-tauri`) clients mirror in P7/P8.
 *
 * WIRE-FIELD CASING: every field that crosses the wire is **snake_case**,
 * uniformly — matching the v1 contract (the Go `internal/daemon/api` view
 * types use `task_id`/`created_at`/… with no rename) and the
 * v2 view types in `./types.ts`. The plan §4 sketch wrote a few session
 * params in camelCase (`taskId`, `fromLine`); they are normalised here
 * (`task_id`, `from_line`) so the three mirrored codebases never carry
 * per-field JSON tag overrides. (The extension-facing JS API in
 * `./workflow.ts` / `./agent.ts` / `./api.ts` is NOT a wire surface and stays
 * idiomatic camelCase.)
 *
 * The canonical method / notification lists live here as runtime const arrays
 * (`RPC_METHODS`, `RPC_NOTIFICATIONS`) so P5/P7/P8 can diff against them; the
 * `RpcMethodMap` / `RpcNotificationMap` type maps bind every name to its
 * param/result (or payload) shape, and the compile-time assertions at the
 * bottom of this file fail typecheck if the two ever drift apart.
 */

import type {
  AgentInfo,
  Comment,
  SessionMeta,
  TaskFilter,
  TaskView,
  WorkflowInfo,
} from "./types.ts";
import type { StepTarget } from "./workflow.ts";
import type { TranscriptLine, TranscriptMessage } from "./transcript.ts";

// ---------------------------------------------------------------------------
// Envelope (plan §4.1).
// ---------------------------------------------------------------------------

/** A client→server request. */
export interface RpcRequest {
  id: number;
  method: string;
  params?: unknown;
}

/** A server→client response. Exactly one of `result` / `error` is set. */
export interface RpcResponse {
  id: number;
  result?: unknown;
  error?: RpcError;
}

/** The error payload of a failed response. */
export interface RpcError {
  code: number;
  message: string;
  details?: unknown;
}

/** A server→client notification (no id, no response expected). */
export interface RpcNotification {
  method: string;
  params: unknown;
}

/**
 * JSON-RPC error codes (mirrors the v1 JSON-RPC error-code set). The
 * reserved range is kept for protocol failures; the `1xxx` range carries the
 * domain errors the Go side maps to 4xx.
 */
export const ErrorCodes = {
  PARSE_ERROR: -32700,
  INVALID_REQUEST: -32600,
  METHOD_NOT_FOUND: -32601,
  INVALID_PARAMS: -32602,
  INTERNAL_ERROR: -32603,
  /** The `{cwd}` selector did not resolve to a project. */
  PROJECT_NOT_FOUND: 1001,
  /** The selector was malformed (empty / non-absolute cwd). */
  INVALID_PROJECT: 1002,
  /** A requested entity (task/session/…) was not found. */
  NOT_FOUND: 1003,
  /** The entity exists but is not in a state that accepts the request now. Retryable. */
  CONFLICT: 1004,
  /**
   * A terminal verb (`task.done`/`task.cancel`) would discard uncommitted changes
   * in the task's isolation environment (e.g. a git worktree). Retryable with
   * `force:true`. Not worktree-specific — any isolation provider may raise it.
   */
  ENVIRONMENT_DIRTY: 1005,
} as const;

export type ErrorCode = (typeof ErrorCodes)[keyof typeof ErrorCodes];

// ---------------------------------------------------------------------------
// Shared param / result building blocks.
// ---------------------------------------------------------------------------

/** Project selector carried by every project-scoped method (plan §4). */
export interface ProjectSelector {
  cwd: string;
}

/** Generic success acknowledgement. */
export interface OkResult {
  ok: boolean;
}

export interface VersionInfo {
  version: string;
  commit: string;
}

export interface AuthParams {
  token: string;
}

/** One project in the aggregated health view. */
export interface HealthProject {
  root: string;
  queued: number;
  running: number;
  opened_at: string;
}

export interface Health {
  ok: boolean;
  workers: number;
  queued: number;
  running: number;
  projects: HealthProject[];
}

/**
 * A project in the persisted registry (`~/.autosk/projects.json`, plan §3.7).
 * v2 drops `db_path` — there is no database.
 */
export interface ProjectInfo {
  root: string;
  name: string;
}

/** One extension load error surfaced via `project.diagnostics` (plan §3.6). */
export interface ExtensionLoadError {
  /** Path or npm package name of the offending extension. */
  source: string;
  error: string;
}

/** `project.diagnostics` result (plan §4). */
export interface ProjectDiagnostics {
  root: string;
  extensions: ExtensionLoadError[];
}

// ---- task domain params ---------------------------------------------------

export interface TaskListParams extends ProjectSelector {
  filter?: TaskFilter;
}
export interface TaskGetParams extends ProjectSelector {
  id: string;
}
/**
 * `task.done` / `task.cancel`. `force:true` reaps the task's isolation env
 * (worktree) even when it has uncommitted changes (the branch is preserved);
 * without it a dirty env is refused with {@link ErrorCodes.ENVIRONMENT_DIRTY}.
 */
export interface TaskTerminalParams extends TaskGetParams {
  force?: boolean;
}
export interface TaskCreateParams extends ProjectSelector {
  title: string;
  description?: string;
  blocked_by?: string[];
}
export interface TaskUpdateParams extends ProjectSelector {
  id: string;
  title?: string;
  description?: string;
}
/**
 * Enroll a non-`work` task into a named workflow, transitioning it to `step`
 * (default: the workflow's first step). Allowed from `new`/`cancel`/`human`.
 */
export interface TaskEnrollParams extends ProjectSelector {
  id: string;
  workflow: string;
  /** Starting step name; omitted ⇒ the workflow's first step. */
  step?: string;
}
export interface TaskResumeParams extends ProjectSelector {
  id: string;
  to?: StepTarget;
}
export interface TaskBlockParams extends ProjectSelector {
  id: string;
  blocked_by: string;
}
export interface CommentAddParams extends ProjectSelector {
  task_id: string;
  text: string;
  author?: string;
}
export interface CommentListParams extends ProjectSelector {
  task_id: string;
}
export interface CommentEditParams extends ProjectSelector {
  task_id: string;
  comment_id: string;
  text: string;
}
export interface CommentDeleteParams extends ProjectSelector {
  task_id: string;
  comment_id: string;
}

// ---- registry domain params ----------------------------------------------

export interface WorkflowGetParams extends ProjectSelector {
  name: string;
}

// ---- extension management params / results --------------------------------

/** Install an extension (`npm:<spec>` or a local path). `local` → project scope. */
export interface ExtensionInstallParams extends ProjectSelector {
  source: string;
  /** Install into the project (`<root>/.autosk/`) instead of globally. */
  local?: boolean;
}
/** Remove an extension entry from `settings.json`. `local` → project scope. */
export interface ExtensionRemoveParams extends ProjectSelector {
  source: string;
  local?: boolean;
}
/**
 * Update installed npm extensions to newer registry versions in place
 * (`autosk ext update`). `source` targets a single extension by npm name;
 * `scope` forces one scope (`global` / `project`); absent ⇒ auto (the union of
 * global + project inside a project, global only outside one). `dry_run` reports
 * available updates and installs nothing.
 */
export interface ExtensionUpdateParams extends ProjectSelector {
  source?: string;
  scope?: "global" | "project";
  dry_run?: boolean;
}
export interface ExtensionInstallResult {
  scope: "global" | "project";
  /** The canonical `settings.json` entry written (`npm:<spec>` | `<abs-path>`). */
  source: string;
  /** The `settings.json` the entry was written to. */
  settings_path: string;
  /** Whether an npm install ran (false for a local-path source). */
  installed: boolean;
}
export interface ExtensionRemoveResult {
  scope: "global" | "project";
  /** The entry actually removed (npm matches by name, so its version may differ
   * from the argument); when nothing matched, the canonical entry form. */
  source: string;
  settings_path: string;
  /** Whether a matching entry was removed. */
  removed: boolean;
}
/** One classified `settings.json#extensions` entry (`extension.list`). */
export interface ExtensionEntryInfo {
  /** The raw entry (`npm:<spec>` | `<abs-path>` | unrecognised). */
  source: string;
  scope: "global" | "project";
  kind: "npm" | "local" | "invalid";
  /** Whether it resolves to a loadable extension right now. */
  resolved: boolean;
}
export interface ExtensionListResult {
  entries: ExtensionEntryInfo[];
}
/**
 * One extension considered by `extension.update`. `status` is the outcome:
 * `updated`/`up-to-date`/`failed` (real run), `available`/`unknown` (dry-run),
 * or `skipped` (version-pinned npm or a local-path entry — nothing to update).
 * `from_version` is the installed version before; `to_version` the latest (or
 * installed-after on a real update); `reason` explains a skip/failure.
 */
export interface ExtensionUpdateEntry {
  /** The raw `settings.json` entry (`npm:<spec>` | `<abs-path>`). */
  source: string;
  /** The npm package name (or the local path for a local entry). */
  name: string;
  scope: "global" | "project";
  status: "updated" | "up-to-date" | "skipped" | "failed" | "available" | "unknown";
  from_version?: string;
  to_version?: string;
  reason?: string;
}
export interface ExtensionUpdateResult {
  entries: ExtensionUpdateEntry[];
  /** Whether this was a dry-run (no installs performed). */
  dry_run: boolean;
  /** Whether anything was actually updated (drives the restart hint). */
  changed: boolean;
}

// ---- session domain params ------------------------------------------------

export interface SessionListParams extends ProjectSelector {
  task_id?: string;
}
export interface SessionGetParams extends ProjectSelector {
  id: string;
}
export interface SessionTranscriptParams extends ProjectSelector {
  id: string;
  /** 1-based line to start from (header is line 1). Defaults to the start. */
  from_line?: number;
  limit?: number;
}
export interface SessionTranscriptResult {
  entries: TranscriptLine[];
  /** Cursor to pass as the next `from_line` for tailing. */
  next_line: number;
}
export interface SessionSubscribeParams extends ProjectSelector {
  id: string;
  /** Replay from this line before tailing. */
  from_line?: number;
}
export interface SessionInputParams extends ProjectSelector {
  id: string;
  message: string;
  kind: "steer" | "followup";
}
export interface SessionAbortParams extends ProjectSelector {
  id: string;
}
/** Creates an interactive (taskless) chat session for a registered agent. */
export interface SessionCreateParams extends ProjectSelector {
  /** A registered agent name (see `registry.agent.list`). */
  agent: string;
}
/** Gracefully ends a live interactive session → status `done`. */
export interface SessionEndParams extends ProjectSelector {
  id: string;
}

// ---------------------------------------------------------------------------
// Method map (plan §4). Keyed by the fully-qualified method name.
// ---------------------------------------------------------------------------

export interface RpcMethodMap {
  // meta
  "meta.version": { params: null; result: VersionInfo };
  "meta.auth": { params: AuthParams; result: OkResult };
  "meta.healthz": { params: null; result: Health };
  "meta.shutdown": { params: null; result: OkResult };

  // project
  "project.list": { params: null; result: ProjectInfo[] };
  "project.add": { params: { cwd: string; name?: string }; result: ProjectInfo };
  "project.remove": { params: ProjectSelector; result: OkResult };
  "project.init": { params: { cwd: string }; result: ProjectInfo };
  "project.diagnostics": { params: ProjectSelector; result: ProjectDiagnostics };
  "project.subscribe": { params: ProjectSelector; result: OkResult };
  "project.unsubscribe": { params: ProjectSelector; result: OkResult };

  // task
  "task.list": { params: TaskListParams; result: TaskView[] };
  "task.get": { params: TaskGetParams; result: TaskView };
  "task.create": { params: TaskCreateParams; result: TaskView };
  "task.update": { params: TaskUpdateParams; result: TaskView };
  "task.enroll": { params: TaskEnrollParams; result: TaskView };
  "task.resume": { params: TaskResumeParams; result: TaskView };
  "task.done": { params: TaskTerminalParams; result: TaskView };
  "task.cancel": { params: TaskTerminalParams; result: TaskView };
  "task.reopen": { params: TaskGetParams; result: TaskView };
  "task.block": { params: TaskBlockParams; result: TaskView };
  "task.unblock": { params: TaskBlockParams; result: TaskView };
  "task.comment.add": { params: CommentAddParams; result: Comment };
  "task.comment.list": { params: CommentListParams; result: Comment[] };
  "task.comment.edit": { params: CommentEditParams; result: Comment };
  "task.comment.delete": { params: CommentDeleteParams; result: OkResult };
  "task.subscribe": { params: ProjectSelector; result: OkResult };
  "task.unsubscribe": { params: ProjectSelector; result: OkResult };

  // registry
  "registry.workflow.list": { params: ProjectSelector; result: WorkflowInfo[] };
  "registry.workflow.get": { params: WorkflowGetParams; result: WorkflowInfo };
  "registry.agent.list": { params: ProjectSelector; result: AgentInfo[] };

  // extension management (autosk ext)
  "extension.install": { params: ExtensionInstallParams; result: ExtensionInstallResult };
  "extension.list": { params: ProjectSelector; result: ExtensionListResult };
  "extension.remove": { params: ExtensionRemoveParams; result: ExtensionRemoveResult };
  "extension.update": { params: ExtensionUpdateParams; result: ExtensionUpdateResult };

  // session
  "session.list": { params: SessionListParams; result: SessionMeta[] };
  "session.get": { params: SessionGetParams; result: SessionMeta };
  "session.transcript": { params: SessionTranscriptParams; result: SessionTranscriptResult };
  "session.subscribe": { params: SessionSubscribeParams; result: OkResult };
  "session.unsubscribe": { params: SessionGetParams; result: OkResult };
  /** Project-scoped session lifecycle channel (queued/running/terminal pushes). */
  "session.subscribeProject": { params: ProjectSelector; result: OkResult };
  "session.unsubscribeProject": { params: ProjectSelector; result: OkResult };
  "session.input": { params: SessionInputParams; result: OkResult };
  "session.abort": { params: SessionAbortParams; result: OkResult };
  /** Creates + dispatches an interactive (taskless) chat session. */
  "session.create": { params: SessionCreateParams; result: SessionMeta };
  /** Gracefully ends a live interactive session (status → `done`). */
  "session.end": { params: SessionEndParams; result: OkResult };
}

/** A valid proto-v2 method name. */
export type RpcMethod = keyof RpcMethodMap;

/** The params type for a given method. */
export type ParamsOf<M extends RpcMethod> = RpcMethodMap[M]["params"];

/** The result type for a given method. */
export type ResultOf<M extends RpcMethod> = RpcMethodMap[M]["result"];

/**
 * The canonical, runtime-enumerable list of every proto-v2 method (plan §4).
 * The compile-time assertions below pin this to `keyof RpcMethodMap`.
 */
export const RPC_METHODS = [
  "meta.version",
  "meta.auth",
  "meta.healthz",
  "meta.shutdown",
  "project.list",
  "project.add",
  "project.remove",
  "project.init",
  "project.diagnostics",
  "project.subscribe",
  "project.unsubscribe",
  "task.list",
  "task.get",
  "task.create",
  "task.update",
  "task.enroll",
  "task.resume",
  "task.done",
  "task.cancel",
  "task.reopen",
  "task.block",
  "task.unblock",
  "task.comment.add",
  "task.comment.list",
  "task.comment.edit",
  "task.comment.delete",
  "task.subscribe",
  "task.unsubscribe",
  "registry.workflow.list",
  "registry.workflow.get",
  "registry.agent.list",
  "extension.install",
  "extension.list",
  "extension.remove",
  "extension.update",
  "session.list",
  "session.get",
  "session.transcript",
  "session.subscribe",
  "session.unsubscribe",
  "session.subscribeProject",
  "session.unsubscribeProject",
  "session.input",
  "session.abort",
  "session.create",
  "session.end",
] as const satisfies readonly RpcMethod[];

// ---------------------------------------------------------------------------
// Notifications (plan §4). Same push model as v1.
// ---------------------------------------------------------------------------

/** `task-changed` payload. */
export interface TaskChangedParams {
  /** The project root the task belongs to. */
  root: string;
  task: TaskView;
}

/** `project-changed` payload. */
export interface ProjectChangedParams {
  project: ProjectInfo;
}

/** `session-event` payload. `kind` mirrors the v1 SSE frames, plus the
 * ephemeral `partial` streaming frame. */
export interface SessionEventParams {
  root: string;
  session_id: string;
  kind: "message" | "status" | "done" | "error" | "partial";
  /** Present on `message` (a single transcript line) — also used for replay. */
  event?: TranscriptLine;
  /** Present on `status` / `done` (the decorated session meta). */
  session?: SessionMeta;
  /** Present on `error`. */
  error?: string;
  /** Monotonic replay cursor (the line number of `event`). */
  line?: number;
  /**
   * Present on `partial`: a non-persisted, CUMULATIVE message snapshot of an
   * in-progress assistant turn. EPHEMERAL — it is never written to the
   * transcript, carries no `line`, and does NOT advance the subscription
   * cursor; the eventual committed `message` frame supersedes it.
   */
  partial?: TranscriptMessage;
}

/**
 * `session-changed` payload — a project-scoped push of one session's metadata
 * whenever it is created or changes status (queued → running → terminal).
 * Delivered to connections that issued `session.subscribeProject` for `root`.
 *
 * Distinct from `session-event` (the per-session transcript tail): this never
 * carries transcript lines, only the decorated {@link SessionMeta}, so a client
 * keeps its session list/panel live WITHOUT knowing a session id ahead of time
 * to subscribe per-session.
 */
export interface SessionChangedParams {
  root: string;
  session: SessionMeta;
}

export interface RpcNotificationMap {
  "task-changed": TaskChangedParams;
  "project-changed": ProjectChangedParams;
  "session-event": SessionEventParams;
  "session-changed": SessionChangedParams;
}

/** A valid proto-v2 notification name. */
export type RpcNotificationMethod = keyof RpcNotificationMap;

/** The payload type for a given notification. */
export type NotificationParamsOf<N extends RpcNotificationMethod> = RpcNotificationMap[N];

/** The canonical, runtime-enumerable list of every proto-v2 notification (plan §4). */
export const RPC_NOTIFICATIONS = [
  "task-changed",
  "project-changed",
  "session-event",
  "session-changed",
] as const satisfies readonly RpcNotificationMethod[];

// ---------------------------------------------------------------------------
// Compile-time conformance: the runtime lists and the type maps must agree.
// These fail `bun run typecheck` if a method/notification is added to one side
// but not the other.
// ---------------------------------------------------------------------------

type Equal<X, Y> = (<T>() => T extends X ? 1 : 2) extends <T>() => T extends Y ? 1 : 2
  ? true
  : false;
type Expect<T extends true> = T;

type _AssertMethodsCovered = Expect<Equal<(typeof RPC_METHODS)[number], RpcMethod>>;
type _AssertNotificationsCovered = Expect<
  Equal<(typeof RPC_NOTIFICATIONS)[number], RpcNotificationMethod>
>;
