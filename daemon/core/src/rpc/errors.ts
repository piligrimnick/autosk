/**
 * RPC error envelope mapping (plan §4 error model).
 *
 * Every throw that escapes a handler is normalised onto a wire {@link RpcError}
 * shape `{ code, message, details? }`. The engine's {@link EngineError} carries a
 * proto-v2 code already, so its code passes straight through; the project
 * resolver's typed errors map onto `PROJECT_NOT_FOUND` / `INVALID_PROJECT`; an
 * {@link RpcError} thrown by a handler carries its own code; everything else is
 * an `INTERNAL_ERROR`.
 */

import { ErrorCodes, type RpcError as RpcErrorPayload } from "@autosk/sdk";

import { EngineError } from "../engine/types.ts";
import { InvalidProjectError, ProjectNotFoundError } from "../project/resolve.ts";

/** A typed error a handler can throw to control the wire `code`/`message`/`details`. */
export class RpcError extends Error {
  constructor(
    readonly code: number,
    message: string,
    readonly details?: unknown,
  ) {
    super(message);
    this.name = "RpcError";
  }
}

/** Normalises any thrown value onto the wire {@link RpcErrorPayload}. */
export function toRpcError(e: unknown): RpcErrorPayload {
  if (e instanceof RpcError) {
    return e.details !== undefined
      ? { code: e.code, message: e.message, details: e.details }
      : { code: e.code, message: e.message };
  }
  if (e instanceof EngineError) {
    return { code: e.code, message: e.message };
  }
  if (e instanceof ProjectNotFoundError) {
    return { code: ErrorCodes.PROJECT_NOT_FOUND, message: e.message };
  }
  if (e instanceof InvalidProjectError) {
    return { code: ErrorCodes.INVALID_PROJECT, message: e.message };
  }
  return { code: ErrorCodes.INTERNAL_ERROR, message: e instanceof Error ? e.message : String(e) };
}
