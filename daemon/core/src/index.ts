/**
 * @autosk/core — the autoskd v2 daemon binary.
 *
 * Built across P2–P5:
 *  - P2: file store + project manager (this module's `store/` + `project/`);
 *  - P3: extension loader + per-project registries;
 *  - P4: scheduler + session lifecycle + `ctx.transit`;
 *  - P5: JSON-RPC v2 server (UDS + TCP/token).
 */

import { RPC_METHODS, type RpcMethod } from "@autosk/sdk";

export * from "./store/index.ts";
export * from "./project/index.ts";

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
