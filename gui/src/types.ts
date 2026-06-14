// Wire-projection types — TypeScript mirrors of the proto-v2 SDK in
// `daemon/sdk/src/{types,proto,transcript,workflow}.ts`. These are the JSON
// shapes `autoskd` returns over JSON-RPC. Field names are snake_case to match
// the wire exactly; RFC3339 UTC for all timestamps (the machine-wire-format
// rule from AGENTS.md — the frontend renders local). The pi-format transcript
// types (message content blocks, engine `autosk:*` customs) stay camelCase, as
// they are piped through verbatim from pi's session format.
//
// Keep this file in lockstep with `daemon/sdk/src`. The Go client mirror lives
// in `internal/daemon/api` (P7) — cross-check against it. The v1 Rust wire
// crate has been retired (P9) and is NOT mirrored here.

// ---------------------------------------------------------------------------
// Task domain (sdk/types.ts §3.1).
// ---------------------------------------------------------------------------

/** The five-status task enum. */
export type TaskStatus = "new" | "work" | "human" | "done" | "cancel";

/** A lightweight reference to a related task (dependency edges). */
export interface TaskRef {
  id: string;
  status: TaskStatus;
}

/**
 * The enriched task view. v2 drops the old ranking / authorship / free-form
 * key-value fields; `workflow` / `step` are `null` until the task is enrolled.
 * `blocked` and `blocks` are derived server-side.
 */
export interface TaskView {
  id: string;
  title: string;
  description: string;
  status: TaskStatus;
  workflow: string | null;
  step: string | null;
  blocked: boolean;
  blocked_by: TaskRef[];
  blocks: TaskRef[];
  comment_count: number;
  created_at: string;
  updated_at: string;
}

/** One comment on a task. Editable/deletable (not strictly append-only). */
export interface Comment {
  id: string;
  author: string;
  text: string;
  created_at: string;
  updated_at: string;
}

/** Filter for `task.list`. */
export interface TaskFilter {
  status?: TaskStatus | TaskStatus[];
  workflow?: string;
  step?: string;
  blocked?: boolean;
}

// ---------------------------------------------------------------------------
// Session domain (sdk/types.ts §3.2). Replaces the v1 run record.
// ---------------------------------------------------------------------------

/** Session lifecycle status. */
export type SessionStatus = "queued" | "running" | "done" | "failed" | "aborted";

/** Session record (`./.autosk/sessions/<id>.json`). */
export interface SessionMeta {
  id: string;
  task_id: string;
  workflow: string;
  step: string;
  agent: string;
  status: SessionStatus;
  error?: string;
  started_at: string | null;
  ended_at: string | null;
}

// ---------------------------------------------------------------------------
// Registry domain (sdk/types.ts §4). Workflows/agents are code, not data.
// ---------------------------------------------------------------------------

/**
 * The target of a transition: a sibling step within the same workflow, or a
 * terminal/park status.
 */
export type StepTarget = { step: string } | { status: "done" | "cancel" | "human" };

/** One step of a workflow as rendered from code for `registry.workflow.*`. */
export interface WorkflowStepInfo {
  name: string;
  /**
   * Terminal/park status for a statusStep (`"done"|"cancel"|"human"`), or
   * `null` for an agent step (whose `name` IS the agent key).
   */
  status: "done" | "cancel" | "human" | null;
  /** Conservative declared target set (a superset). */
  targets: StepTarget[];
}

/** A workflow rendered from code (`registry.workflow.get`). Read-only projection. */
export interface WorkflowInfo {
  name: string;
  description?: string;
  /** snake_case on the wire (the `WorkflowDefinition.firstStep` projection). */
  first_step: string;
  steps: WorkflowStepInfo[];
  /** Isolation provider tag; `"none"` when the workflow has no provider. */
  isolation: string;
}

// ---------------------------------------------------------------------------
// pi-format transcript entries (sdk/transcript.ts §3.2).
// ---------------------------------------------------------------------------

export interface TextContent {
  type: "text";
  text: string;
  textSignature?: string;
}

export interface ThinkingContent {
  type: "thinking";
  thinking: string;
  thinkingSignature?: string;
  redacted?: boolean;
}

export interface ImageContent {
  type: "image";
  /** base64-encoded image data. */
  data: string;
  /** e.g. `"image/png"`. */
  mimeType: string;
}

export interface ToolCall {
  type: "toolCall";
  id: string;
  name: string;
  arguments: Record<string, unknown>;
  thoughtSignature?: string;
}

/** A block of message content. */
export type ContentBlock = TextContent | ThinkingContent | ImageContent | ToolCall;

/** Token usage + cost accounting attached to assistant messages. */
export interface Usage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  totalTokens: number;
  cost: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
    total: number;
  };
}

export type StopReason = "stop" | "length" | "toolUse" | "error" | "aborted";

export interface UserMessage {
  role: "user";
  content: string | (TextContent | ImageContent)[];
  /** Unix timestamp in milliseconds. */
  timestamp: number;
}

export interface AssistantMessage {
  role: "assistant";
  content: (TextContent | ThinkingContent | ToolCall)[];
  api?: string;
  provider: string;
  model: string;
  usage: Usage;
  stopReason: StopReason;
  errorMessage?: string;
  /** Unix timestamp in milliseconds. */
  timestamp: number;
}

export interface ToolResultMessage {
  role: "toolResult";
  toolCallId: string;
  toolName: string;
  content: (TextContent | ImageContent)[];
  details?: unknown;
  isError: boolean;
  /** Unix timestamp in milliseconds. */
  timestamp: number;
}

/** A pi message, written via `ctx.log.message(...)`. */
export type TranscriptMessage = UserMessage | AssistantMessage | ToolResultMessage;

/** Line 1 of a transcript: pi's `SessionHeader` with autosk fields added. */
export interface SessionHeader {
  type: "session";
  version: 1;
  id: string;
  task_id: string;
  workflow: string;
  step: string;
  agent: string;
  /** RFC3339 UTC. */
  timestamp: string;
  cwd: string;
}

/** Base for every non-header entry (pi's `SessionEntryBase` minus `parentId`). */
export interface TranscriptEntryBase {
  type: string;
  /** 8-char hex id. */
  id: string;
  /** RFC3339 UTC. */
  timestamp: string;
}

/** A pi message-schema entry (`ctx.log.message`). */
export interface MessageEntry extends TranscriptEntryBase {
  type: "message";
  message: TranscriptMessage;
}

/** A generic custom entry (`ctx.log.custom`) — the agent logging channel. */
export interface CustomEntry<T = unknown> extends TranscriptEntryBase {
  type: "custom";
  customType: string;
  data?: T;
}

/** The structural custom types the engine emits itself. */
export const AUTOSK_CUSTOM_TYPES = [
  "autosk:transit",
  "autosk:steer",
  "autosk:error",
  "autosk:session_end",
] as const;

export type AutoskCustomType = (typeof AUTOSK_CUSTOM_TYPES)[number];

/** `autosk:transit` payload — one committed transition. */
export interface TransitData {
  to: StepTarget;
  from?: { workflow: string; step: string };
}

/** `autosk:steer` payload — a steer/followup message injected into a live session. */
export interface SteerData {
  kind: "steer" | "followup";
  message: string;
}

/** `autosk:error` payload — an error surfaced during the session. */
export interface ErrorData {
  error: string;
  message?: string;
}

/** `autosk:session_end` payload — the session's terminal status. */
export interface SessionEndData {
  status: "done" | "failed" | "aborted";
  error?: string;
}

export interface TransitEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:transit";
  data: TransitData;
}

export interface SteerEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:steer";
  data: SteerData;
}

export interface ErrorEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:error";
  data: ErrorData;
}

export interface SessionEndEntry extends TranscriptEntryBase {
  type: "custom";
  customType: "autosk:session_end";
  data: SessionEndData;
}

/** The union of engine structural entries. */
export type EngineEntry = TransitEntry | SteerEntry | ErrorEntry | SessionEndEntry;

/** Any non-header transcript entry. */
export type TranscriptEntry = MessageEntry | CustomEntry | EngineEntry;

/** Any line of a transcript file. */
export type TranscriptLine = SessionHeader | TranscriptEntry;

/** Type guard: is `s` one of the engine's structural custom types? */
export function isAutoskCustomType(s: string): s is AutoskCustomType {
  return (AUTOSK_CUSTOM_TYPES as readonly string[]).includes(s);
}

// ---------------------------------------------------------------------------
// Project / meta (sdk/proto.ts §4).
// ---------------------------------------------------------------------------

/** A project in the persisted registry. v2 carries no database path — there is no DB. */
export interface ProjectInfo {
  root: string;
  name: string;
}

/** One extension load error surfaced via `project.diagnostics` (§3.6). */
export interface ExtensionLoadError {
  /** Path or npm package name of the offending extension. */
  source: string;
  error: string;
}

/** `project.diagnostics` result. */
export interface ProjectDiagnostics {
  root: string;
  extensions: ExtensionLoadError[];
}

/** `meta.healthz` per-project row. */
export interface HealthProject {
  root: string;
  queued: number;
  running: number;
  opened_at: string;
}

/** `meta.healthz` result. */
export interface Health {
  ok: boolean;
  workers: number;
  queued: number;
  running: number;
  projects: HealthProject[];
}

/** `meta.version` result. */
export interface VersionInfo {
  version: string;
  commit: string;
}

// ---------------------------------------------------------------------------
// Notification payloads (sdk/proto.ts §4).
// ---------------------------------------------------------------------------

/** `task-changed` payload — carries the full TaskView (no refetch needed). */
export interface TaskChangedParams {
  /** The project root the task belongs to. */
  root: string;
  task: TaskView;
}

/** `project-changed` payload. */
export interface ProjectChangedParams {
  project: ProjectInfo;
}

/** `session-event` payload. `kind` mirrors the v1 SSE frames. */
export interface SessionEventParams {
  root: string;
  session_id: string;
  kind: "message" | "status" | "done" | "error";
  /** Present on `message` (a single transcript line) — also used for replay. */
  event?: TranscriptLine;
  /** Present on `status` / `done` (the decorated session meta). */
  session?: SessionMeta;
  /** Present on `error`. */
  error?: string;
  /** Monotonic replay cursor (the line number of `event`). */
  line?: number;
}

/**
 * `session-changed` payload — a project-scoped push of one session's metadata
 * whenever it is created or changes status (queued → running → terminal).
 * Delivered to connections that issued `session.subscribeProject`. Unlike
 * `session-event` it never carries a transcript line, only the SessionMeta, so
 * the Sessions panel stays live without a per-session subscribe.
 */
export interface SessionChangedParams {
  root: string;
  session: SessionMeta;
}

// ---- frontend-only types --------------------------------------------------

/** Backend transport mode (an app setting). */
export type BackendMode = "local" | "remote";

/** App settings persisted by the Tauri backend (`AppSettings` in Rust). */
export interface AppSettings {
  backend_mode: BackendMode;
  remote_host: string;
  remote_token: string;
}

/** Daemon connection status surfaced to the UI. */
export interface DaemonStatus {
  connected: boolean;
  mode: BackendMode;
  error?: string | null;
}

/** Reserved-status set helpers. */
export const OPEN_STATUSES = ["new", "work", "human"] as const;
export const TERMINAL_STATUSES = ["done", "cancel"] as const;
