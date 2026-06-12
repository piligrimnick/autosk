/**
 * @autosk/sdk — public, extension-facing types and helpers for autoskd v2.
 *
 * See the package README and `docs/plans/20260612-Bun-Daemon-Extensions.md`
 * for the design. The three layers re-exported here:
 *
 *  - concept model — `workflow.ts`, `agent.ts`, `types.ts`, `api.ts`;
 *  - transcript format — `transcript.ts` (pi-compatible session entries);
 *  - proto-v2 wire types — `proto.ts` (the Go/Tauri mirror source of truth).
 */

export * from "./types.ts";
export * from "./workflow.ts";
export * from "./agent.ts";
export * from "./transcript.ts";
export * from "./proto.ts";
export * from "./api.ts";
export * from "./singleStep.ts";
export * from "./ids.ts";
