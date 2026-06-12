/**
 * The engine (plan §3.7(3-4)): scheduler + worker pool, session lifecycle,
 * `ctx.transit`, isolation acquire/release, transcript writer, steer/abort
 * routing, and crash recovery. RPC-independent — drivable purely from tests.
 */

export {
  Engine,
  DEFAULT_WORKERS,
  DEFAULT_RESCAN_MS,
  type EngineOptions,
  type EnrollTarget,
} from "./engine.ts";
export { SessionRuntime, type SessionRuntimeInit } from "./session.ts";
export { TranscriptWriter } from "./transcript.ts";
export { execOneShot, spawnChild } from "./child.ts";
export {
  validateTarget,
  positionFor,
  buildTransitContext,
  type Position,
  type TransitContextInput,
} from "./transition.ts";
export { buildTasksApi, buildWorkflowsApi, declaredTargets } from "./context.ts";
export {
  EngineError,
  ENGINE_COMMENT_AUTHOR,
  errMsg,
  type EngineProject,
  type EngineEvent,
  type EngineListener,
  type SessionHost,
} from "./types.ts";
