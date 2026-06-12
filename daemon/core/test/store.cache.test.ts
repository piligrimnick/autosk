/**
 * The mtime-keyed read cache actually short-circuits the hot read path (B2).
 *
 * Plan §3.7(2): "in-memory cache keyed by mtime; reads go through the cache."
 * A steady-state `listTaskViews` over an UNCHANGED project must reconcile via a
 * cheap signature probe (a stat) and must NOT `readFile` + parse every
 * `task.json` — otherwise a polling `task.list` over a large project pays
 * O(N) reads + O(N) parses on every poll. A genuine external edit still bumps
 * the signature and drops into a real `readDisk`.
 */

import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { readFile, writeFile } from "node:fs/promises";

import { parseTask, serializeTask, Store, TaskStore } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

describe("read cache — listTaskViews is O(N) stats, not O(N) reads", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;

  beforeEach(async () => {
    dir = tempDir();
    store = new Store(dir.path, { watch: false });
    await store.open();
  });
  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  test("two steady-state listTaskViews calls do not readDisk any task.json", async () => {
    for (let i = 0; i < 5; i++) await store.createTask({ title: `t${i}` });

    const probe = spyOn(TaskStore.prototype, "probeSig");
    const read = spyOn(TaskStore.prototype, "readDisk");
    try {
      await store.listTaskViews();
      await store.listTaskViews();
      // Every task reconciled through the cheap signature probe; none re-read.
      expect(read).toHaveBeenCalledTimes(0);
      expect(probe.mock.calls.length).toBeGreaterThan(0);
    } finally {
      probe.mockRestore();
      read.mockRestore();
    }
  });

  test("a real external edit still drops into exactly one readDisk", async () => {
    const t = await store.createTask({ title: "t" });
    await store.createTask({ title: "other" });
    await store.listTaskViews(); // warm the cache for both

    // A human edits one task.json directly (changes the signature).
    const path = store.paths.taskJson(t.id);
    const edited = parseTask(await readFile(path, "utf8"));
    edited.title = "edited-by-human";
    await writeFile(path, serializeTask(edited));

    const read = spyOn(TaskStore.prototype, "readDisk");
    try {
      const views = await store.listTaskViews();
      // Only the one changed file is re-read; the unchanged one stays cached.
      expect(read).toHaveBeenCalledTimes(1);
      expect(views.find((v) => v.id === t.id)!.title).toBe("edited-by-human");
    } finally {
      read.mockRestore();
    }
  });
});
