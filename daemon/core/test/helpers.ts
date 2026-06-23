/**
 * Shared test scaffolding: throwaway temp dirs + a deterministic clock.
 */

import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Clock, Store } from "../src/index.ts";

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

/**
 * A "fully settled" barrier — the correct sync point for any assertion on
 * session-meta status or isolation state after a run finishes.
 *
 * The engine's commit path flips the TASK's observable status at step 1
 * (`setPosition`, so a concurrent scan keeps seeing the live session and never
 * re-dispatches the old step) but only seals the session meta and releases
 * isolation at steps 3-4 — separated by a real transcript `flush()`. So a bare
 * `waitFor(() => taskView.status === X)` fires EARLY, while the session is still
 * `running` and the worktree unreleased; asserting on those a beat later is a
 * race that loses under full-suite load. This waits until the task has reached
 * `status` AND no session is still live (`queued`/`running`) for it, which is
 * true only AFTER the seal (step 4). Reordering the engine to seal first is NOT
 * an option — it would re-introduce the stale-step re-dispatch (BLOCKER #1) — so
 * the barrier is what must move, not the commit order.
 */
export async function waitForComplete(
  store: Store,
  taskId: string,
  status: string,
  timeoutMs = 2000,
): Promise<void> {
  await waitFor(async () => {
    const view = await store.taskView(taskId);
    return view.status === status && store.sessions.liveSessionsForTask(taskId).length === 0;
  }, timeoutMs);
}
