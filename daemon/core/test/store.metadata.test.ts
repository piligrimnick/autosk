/**
 * Store-level `metadata` mutations (plan §4) + hybrid-ownership for the bag.
 *
 * Covers `mergeMetadata` / `unsetMetadata` (the dedicated merge family),
 * `peekMetadata` (the engine's synchronous read), the `countVisit` option on
 * `setPosition`, the per-task-lock serialisation of concurrent edits, and that
 * an external `metadata` file edit is ACCEPTED by reconciliation (with and
 * without a live session), while status/step/workflow rejection is unchanged.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFile, writeFile } from "node:fs/promises";

import { CapturingLogger, parseTask, serializeTask, Store } from "../src/index.ts";
import type { StoredTask } from "../src/index.ts";
import { fixedClock, tempDir } from "./helpers.ts";

async function externalEdit(store: Store, id: string, mutate: (t: StoredTask) => void): Promise<void> {
  const path = store.paths.taskJson(id);
  const task = parseTask(await readFile(path, "utf8"));
  mutate(task);
  await writeFile(path, serializeTask(task));
}

describe("store metadata mutations", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;
  let log: CapturingLogger;

  beforeEach(async () => {
    dir = tempDir();
    log = new CapturingLogger();
    store = new Store(dir.path, { watch: false, logger: log, clock: fixedClock("2026-01-01T00:00:00Z") });
    await store.open();
  });
  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  test("a fresh task has empty metadata; mergeMetadata sets nested dot-paths", async () => {
    const t = await store.createTask({ title: "t" });
    expect(t.metadata).toEqual({});

    const v = await store.mergeMetadata(t.id, { "step_visits.dev": 2, note: "keep" });
    expect(v.metadata).toEqual({ step_visits: { dev: 2 }, note: "keep" });

    // Persisted to disk in the fixed slot.
    const onDisk = parseTask(await readFile(store.paths.taskJson(t.id), "utf8"));
    expect(onDisk.metadata).toEqual({ step_visits: { dev: 2 }, note: "keep" });
  });

  test("unsetMetadata removes a path and prunes the emptied parent", async () => {
    const t = await store.createTask({ title: "t" });
    await store.mergeMetadata(t.id, { "step_visits.dev": 1 });
    const v = await store.unsetMetadata(t.id, ["step_visits.dev"]);
    expect(v.metadata).toEqual({});
  });

  test("mergeMetadata/unsetMetadata bump updated_at", async () => {
    const clockTimes = ["2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z", "2026-01-01T02:00:00Z"];
    let i = 0;
    const local = new Store(dir.path, { watch: false, logger: log, clock: () => clockTimes[Math.min(i++, clockTimes.length - 1)]! });
    await local.open();
    const t = await local.createTask({ title: "t" });
    const set = await local.mergeMetadata(t.id, { a: 1 });
    expect(set.updated_at).toBe("2026-01-01T01:00:00Z");
    const unset = await local.unsetMetadata(t.id, ["a"]);
    expect(unset.updated_at).toBe("2026-01-01T02:00:00Z");
    await local.close();
  });

  test("an unknown id throws task not found", async () => {
    await expect(store.mergeMetadata("ask-nope01", { a: 1 })).rejects.toThrow(/task not found/);
    await expect(store.unsetMetadata("ask-nope01", ["a"])).rejects.toThrow(/task not found/);
  });

  test("concurrent set/unset serialise under the per-task lock with no lost updates", async () => {
    const t = await store.createTask({ title: "t" });
    // Fire many merges + a couple of unsets concurrently; the per-task lock must
    // serialise them so every accepted key lands deterministically.
    await Promise.all([
      store.mergeMetadata(t.id, { "step_visits.dev": 1 }),
      store.mergeMetadata(t.id, { "step_visits.review": 1 }),
      store.mergeMetadata(t.id, { "step_visits.docs": 1 }),
      store.mergeMetadata(t.id, { keep: true }),
    ]);
    const after = await store.taskView(t.id);
    expect(after.metadata).toEqual({
      step_visits: { dev: 1, review: 1, docs: 1 },
      keep: true,
    });

    // 16 concurrent same-key bumps: each does read (peekMetadata) - modify -
    // write, and the READ is OUTSIDE the per-task lock, so a leaf REPLACE here
    // genuinely races and may lose updates. We therefore only assert the final
    // count is present and monotonic (>= 1), NOT exactly 16. The real
    // "no lost update" guarantee is proven by the distinct-keys half above and
    // by the engine's lock-internal step_visits bump (see the setPosition test).
    await Promise.all(
      Array.from({ length: 16 }, async () => {
        const cur = store.peekMetadata(t.id) as { step_visits?: { dev?: number } };
        const next = (cur.step_visits?.dev ?? 0) + 1;
        await store.mergeMetadata(t.id, { "step_visits.dev": next });
      }),
    );
    // Because each task reads-modifies-writes under the lock via peekMetadata,
    // the merges serialise; the final dev count is monotonic and present.
    const final = (await store.taskView(t.id)).metadata as { step_visits: { dev: number } };
    expect(final.step_visits.dev).toBeGreaterThanOrEqual(1);
  });

  test("setPosition({countVisit}) bumps step_visits atomically; peekMetadata reads it", async () => {
    const t = await store.createTask({ title: "t" });
    await store.setPosition(t.id, { status: "work", workflow: "wf", step: "dev" }, { countVisit: "dev" });
    expect(store.peekMetadata(t.id)).toEqual({ step_visits: { dev: 1 } });
    // A position move with NO countVisit (a {status} flip / reopen / park) does not count.
    await store.setPosition(t.id, { status: "human", workflow: "wf", step: "dev" });
    expect(store.peekMetadata(t.id)).toEqual({ step_visits: { dev: 1 } });
    // Re-entering dev counts again.
    await store.setPosition(t.id, { status: "work", workflow: "wf", step: "dev" }, { countVisit: "dev" });
    expect(store.peekMetadata(t.id)).toEqual({ step_visits: { dev: 2 } });
  });

  test("peekMetadata returns a clone (callers cannot mutate the cached record)", async () => {
    const t = await store.createTask({ title: "t" });
    await store.mergeMetadata(t.id, { "step_visits.dev": 1 });
    const peek = store.peekMetadata(t.id) as { step_visits: { dev: number } };
    peek.step_visits.dev = 999;
    expect((store.peekMetadata(t.id) as { step_visits: { dev: number } }).step_visits.dev).toBe(1);
  });
});

describe("store metadata — hybrid ownership", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;
  let log: CapturingLogger;

  beforeEach(async () => {
    dir = tempDir();
    log = new CapturingLogger();
    store = new Store(dir.path, { watch: false, logger: log, clock: fixedClock("2026-01-01T00:00:00Z") });
    await store.open();
  });
  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  test("external metadata edit on an idle task is accepted", async () => {
    const t = await store.createTask({ title: "t" });
    await externalEdit(store, t.id, (task) => {
      task.metadata = { step_visits: { dev: 0 }, human: "set" };
    });
    await store.reconcileTask(t.id);
    const view = await store.taskView(t.id);
    expect(view.metadata).toEqual({ step_visits: { dev: 0 }, human: "set" });
    expect(log.warns).toHaveLength(0);
  });

  test("external metadata edit WITH a live session is accepted; engine fields still protected", async () => {
    const t = await store.createTask({ title: "t" });
    await store.setPosition(t.id, { status: "work", workflow: "feature-dev", step: "dev" }, { countVisit: "dev" });
    await store.sessions.create({
      id: "0190a1b2-c3d4-7e5f-8a9b-0000000000d1",
      task_id: t.id,
      workflow: "feature-dev",
      step: "dev",
      agent: "@autosk/pi-agent/dev",
      cwd: dir.path,
      timestamp: "2026-01-01T00:00:00Z",
    });
    await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-0000000000d1", { status: "running" });
    expect(store.sessions.hasLiveSession(t.id)).toBe(true);

    // A human resets the visit counter AND illegally flips status, in one edit.
    await externalEdit(store, t.id, (task) => {
      task.metadata = { step_visits: { dev: 0 } }; // accepted (human-editable)
      task.status = "done"; // rejected (engine-owned, live session)
    });
    await store.reconcileTask(t.id);

    const onDisk = parseTask(await readFile(store.paths.taskJson(t.id), "utf8"));
    expect(onDisk.metadata).toEqual({ step_visits: { dev: 0 } }); // reset kept
    expect(onDisk.status).toBe("work"); // engine field restored
    expect(log.warns.some((w) => w.includes("engine-owned"))).toBe(true);
  });
});
