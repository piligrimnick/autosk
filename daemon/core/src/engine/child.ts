/**
 * Engine adapters over the generic SDK process primitives (`@autosk/sdk`'s
 * `runChild` / `spawnChild`) backing `ctx.exec` (one-shot) and `ctx.spawn`
 * (long-lived, stdio-streamed) — plan §4.2.
 *
 * The Bun stdio/abort plumbing (`LineDispatcher`, `readLines`, abort wiring, env
 * merge) now lives in `@autosk/sdk` so the engine and a userspace sandbox library
 * (e.g. `@autosk/sandbox`'s `docker run` wrapper) share ONE implementation. These
 * thin wrappers add only the engine's default injection: the child's cwd defaults
 * to the session's `ctx.cwd` (always the project root now — isolation is
 * agent-owned) and the session's `AbortSignal` is wired so an abort / daemon
 * shutdown kills the child. Core has no pi knowledge — these are generic process
 * helpers the `@autosk/pi-agent` extension (P6) builds on to drive `pi --mode rpc`.
 */

import {
  runChild,
  spawnChild as spawnChildProc,
  type ChildHandle,
  type ExecOptions,
  type ExecResult,
  type SpawnOptions,
} from "@autosk/sdk";

/** Defaults the engine injects into `ctx.exec`. */
export interface ExecDefaults {
  defaultCwd: string;
  defaultSignal: AbortSignal;
}

/** Runs a one-shot child to completion, capturing stdout/stderr (plan §4.2). */
export async function execOneShot(
  cmd: string[],
  opts: ExecOptions & ExecDefaults,
): Promise<ExecResult> {
  return runChild(cmd, {
    cwd: opts.cwd ?? opts.defaultCwd,
    env: opts.env,
    input: opts.input,
    signal: opts.signal ?? opts.defaultSignal,
    timeoutMs: opts.timeoutMs,
  });
}

/** Defaults the engine injects into `ctx.spawn`. */
export interface SpawnDefaults {
  defaultCwd: string;
  signal: AbortSignal;
}

/** Spawns a long-lived child with line-buffered stdout/stderr (plan §4.2). */
export function spawnChild(cmd: string[], opts: SpawnOptions & SpawnDefaults): ChildHandle {
  return spawnChildProc(cmd, {
    cwd: opts.cwd ?? opts.defaultCwd,
    env: opts.env,
    signal: opts.signal,
  });
}
