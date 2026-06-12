/**
 * @autosk/core — the autoskd v2 daemon binary.
 *
 * Placeholder for P1. The real daemon is built across P2–P5:
 *  - P2: file store + project manager;
 *  - P3: extension loader + per-project registries;
 *  - P4: scheduler + session lifecycle + `ctx.transit`;
 *  - P5: JSON-RPC v2 server (UDS + TCP/token).
 *
 * This module imports the proto-v2 surface from `@autosk/sdk` purely to keep
 * the workspace wiring (cross-package resolution) under typecheck from P1 on.
 */

import { RPC_METHODS, type RpcMethod } from "@autosk/sdk";

/** The version the daemon reports over `meta.version`. */
export const VERSION = "0.0.0-dev";

/** Methods the P1 placeholder daemon serves (none yet). */
export const IMPLEMENTED_METHODS: readonly RpcMethod[] = [];

export function main(): void {
  // P5 wires up the RPC server here. For now, just prove the SDK is reachable.
  console.error(
    `autoskd v2 (placeholder): proto-v2 defines ${RPC_METHODS.length} methods; ` +
      `${IMPLEMENTED_METHODS.length} implemented so far.`,
  );
}

if (import.meta.main) {
  main();
}
