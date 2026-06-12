/**
 * Shared test scaffolding: throwaway temp dirs + a deterministic clock.
 */

import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Clock } from "../src/index.ts";

/** Creates a fresh temp dir; returns its path + a cleanup fn. */
export function tempDir(): { path: string; cleanup: () => void } {
  const path = mkdtempSync(join(tmpdir(), "autosk-test-"));
  return { path, cleanup: () => rmSync(path, { recursive: true, force: true }) };
}

/**
 * A clock that returns a fixed timestamp, or steps through a supplied list
 * (repeating the last value once exhausted) so updates get distinct times.
 */
export function fixedClock(times: string | string[]): Clock {
  if (typeof times === "string") return () => times;
  let i = 0;
  return () => times[Math.min(i++, times.length - 1)]!;
}

/** Polls `cond` until it returns true or `timeoutMs` elapses. */
export async function waitFor(
  cond: () => boolean | Promise<boolean>,
  timeoutMs = 2000,
  stepMs = 10,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await cond()) return;
    if (Date.now() > deadline) throw new Error("waitFor: timed out");
    await new Promise((r) => setTimeout(r, stepMs));
  }
}
