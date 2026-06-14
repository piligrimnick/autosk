/**
 * Agent definitions and the run context the engine hands an agent (plan §3.4).
 *
 * Agents are *code* registered by extensions. `onRun` executes one full step
 * in-process and MUST call `ctx.transit(...)` exactly once before returning;
 * returning without a transit fails the session (`error="agent_did_not_transit"`)
 * and parks the task to `human`. Core has no pi knowledge — pi-based agents
 * ship as the `@autosk/pi-agent` extension on top of `ctx.spawn` + `ctx.transit`.
 */

import type { Comment, TaskFilter, TaskView, WorkflowInfo } from "./types.ts";
import type { StepTarget } from "./workflow.ts";
import type { TranscriptMessage } from "./transcript.ts";

/**
 * An agent the engine can run for a step (plan §3.4).
 *
 * An agent is an inline step value: the workflow step key IS the agent name
 * (there is no separate `name` field and no separate agent registry). The
 * engine discriminates an agent step from a {@link StatusStep} structurally via
 * the presence of `onRun`.
 */
export interface AgentDefinition {
  /**
   * Runs one full step. MUST call `ctx.transit(...)` exactly once before
   * returning.
   */
  onRun(ctx: AgentRunContext): Promise<void>;
  /** Invoked when a client calls `session.input {kind:"steer"}` on a live session. */
  onSteer?(ctx: AgentRunContext, message: string): Promise<void>;
  /** Invoked when a client calls `session.input {kind:"followup"}` on a live session. */
  onFollowup?(ctx: AgentRunContext, message: string): Promise<void>;
  /** Invoked on `session.abort`. */
  onAbort?(ctx: AgentRunContext): Promise<void>;
}

/** Options for a one-shot child process via `ctx.exec` (plan §3.4). */
export interface ExecOptions {
  cwd?: string;
  env?: Record<string, string>;
  /** Data written to the child's stdin, then closed. */
  input?: string | Uint8Array;
  /** Aborts the child. Defaults to the session's `ctx.signal`. */
  signal?: AbortSignal;
  /** Kill the child after this many milliseconds. */
  timeoutMs?: number;
}

/** Result of a one-shot child process (plan §3.4). */
export interface ExecResult {
  code: number | null;
  stdout: string;
  stderr: string;
}

/** Options for a long-lived child process via `ctx.spawn` (plan §3.4). */
export interface SpawnOptions {
  cwd?: string;
  env?: Record<string, string>;
}

/**
 * A long-lived interactive child with stdio streaming (plan §3.4). This is how
 * the pi-agent extension drives `pi --mode rpc` over JSON-lines stdio.
 */
export interface ChildHandle {
  stdin: WritableStreamDefaultWriter<Uint8Array>;
  /** Line-buffered stdout. */
  onStdout(cb: (line: string) => void): void;
  /** Line-buffered stderr. */
  onStderr(cb: (line: string) => void): void;
  kill(signal?: string): void;
  exited: Promise<{ code: number | null }>;
}

/** Live task access for the running session (plan §3.4). */
export interface TasksAPI {
  /** The task this session runs for. */
  currentId: string;
  /** Re-reads the current task from the store. */
  current(): Promise<TaskView>;
  get(id: string): Promise<TaskView>;
  list(filter?: TaskFilter): Promise<TaskView[]>;
  /** Defaults to the current task. */
  comments(id?: string): Promise<Comment[]>;
}

/** Live registry access plus the session's current position (plan §3.4). */
export interface WorkflowsAPI {
  current: { workflow: string; step: string; targets: StepTarget[] };
  /** Rendered registry view (in-memory, synchronous). */
  get(name: string): WorkflowInfo | undefined;
  list(): WorkflowInfo[];
}

/** The pi-format transcript writer (plan §3.2, §3.4). */
export interface TranscriptAPI {
  /** Writes a pi message-schema entry. */
  message(message: TranscriptMessage): void;
  /** Writes a `custom` entry — the generic agent logging channel. */
  custom(customType: string, data?: unknown): void;
}

/** The context the engine hands `onRun` / `onSteer` / `onFollowup` / `onAbort` (plan §3.4). */
export interface AgentRunContext {
  session: { id: string };
  /** Project root, or the isolation handle's path. */
  cwd: string;
  /** Fired on abort / daemon shutdown. */
  signal: AbortSignal;

  tasks: TasksAPI;
  workflows: WorkflowsAPI;
  log: TranscriptAPI;

  /** Shorthand: comment on the current task. */
  comment(text: string): Promise<void>;
  /** Validates via `workflow.onTransit`, then commits. A second call throws. */
  transit(to: StepTarget): Promise<void>;

  /** One-shot child process. */
  exec(cmd: string[], opts?: ExecOptions): Promise<ExecResult>;
  /** Long-lived interactive child with stdio streaming. */
  spawn(cmd: string[], opts?: SpawnOptions): ChildHandle;
}
