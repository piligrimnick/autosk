/**
 * Shared engine types (plan §3.3-3.5, §3.7(3-4)).
 *
 * The engine is RPC-independent — it is constructed once by the daemon (P5) and
 * fed projects via {@link EngineProject}. These types are the seam between the
 * {@link Engine} orchestrator and a single running {@link SessionRuntime} (the
 * {@link SessionHost} interface), kept in their own module so `engine.ts` and
 * `session.ts` can import them without a cycle.
 */

import {
  ErrorCodes,
  type SessionMeta,
  type TaskView,
  type TranscriptEntry,
  type TranscriptMessage,
} from "@autosk/sdk";

import type { Clock } from "../store/clock.ts";
import type { Logger } from "../store/logger.ts";
import type { ExtensionRegistry } from "../extensions/registry.ts";
import type { Store } from "../store/store.ts";

/** The comment author the engine records for its own task mutations (parks). */
export const ENGINE_COMMENT_AUTHOR = "autosk";

/**
 * A project the engine schedules over: its canonical root, file store, and the
 * extension registry (workflows + agents). The daemon adds one of these per
 * opened project via {@link Engine.addProject}; the worker pool is global across
 * all of them (plan §3.7(3)).
 */
export interface EngineProject {
  root: string;
  store: Store;
  registry: ExtensionRegistry;
}

/**
 * An event the engine emits to subscribers (tests + P5's RPC notification fan-out).
 * Mirrors the proto-v2 `task-changed` / `session-event` push model loosely; P5
 * maps these onto the wire shapes.
 */
export type EngineEvent =
  | { type: "task-changed"; root: string; task: TaskView }
  | {
      type: "session-event";
      root: string;
      session_id: string;
      kind: "status" | "done" | "error" | "message" | "partial";
      meta?: SessionMeta;
      entry?: TranscriptEntry;
      error?: string;
      /** Present on `partial`: the ephemeral, cumulative message snapshot. */
      partial?: TranscriptMessage;
    };

/** A subscriber to {@link EngineEvent}s. */
export type EngineListener = (event: EngineEvent) => void;

/**
 * The slice of the {@link Engine} a {@link SessionRuntime} drives back into
 * (park the task, emit notifications, re-scan). Declared here so `session.ts`
 * never imports `engine.ts` directly.
 */
export interface SessionHost {
  readonly clock: Clock;
  readonly logger: Logger;
  /** Parks a task to `human` with `error` (status flip + an `autosk` comment + notify). */
  park(project: EngineProject, taskId: string, error: string): Promise<void>;
  /**
   * Enrolls a (freshly-created) task into a workflow — the per-session MCP
   * server's `task create --workflow` path. Runs `onTransit` for the entry edge.
   */
  enrollTask(project: EngineProject, taskId: string, target: { workflow: string }): Promise<TaskView>;
  /** Re-reads the task and emits a `task-changed` event. */
  notifyTaskChanged(project: EngineProject, taskId: string): Promise<void>;
  /** Emits a session lifecycle event. */
  emitSession(project: EngineProject, meta: SessionMeta, kind: "status" | "done" | "error"): void;
  /** Emits a transcript `message` event for one appended entry. */
  emitSessionMessage(project: EngineProject, sessionId: string, entry: TranscriptEntry): void;
  /** Emits an ephemeral `partial` (in-progress) message snapshot (no disk, no cursor). */
  emitSessionPartial(project: EngineProject, sessionId: string, message: TranscriptMessage): void;
  /** Requests a scheduler scan (coalesced). */
  kickScan(): void;
}

/** A typed engine error carrying a proto-v2 {@link ErrorCodes} code for P5 to map. */
export class EngineError extends Error {
  constructor(
    readonly code: number,
    message: string,
  ) {
    super(message);
    this.name = "EngineError";
  }

  static notFound(message: string): EngineError {
    return new EngineError(ErrorCodes.NOT_FOUND, message);
  }
  static conflict(message: string): EngineError {
    return new EngineError(ErrorCodes.CONFLICT, message);
  }
  static projectNotFound(message: string): EngineError {
    return new EngineError(ErrorCodes.PROJECT_NOT_FOUND, message);
  }
  static invalidParams(message: string): EngineError {
    return new EngineError(ErrorCodes.INVALID_PARAMS, message);
  }
}

/** Normalises an unknown thrown value to a message string. */
export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
