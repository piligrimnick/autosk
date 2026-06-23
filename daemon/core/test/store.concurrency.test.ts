/**
 * Concurrency: N parallel mutations of one task through the store produce a
 * valid final file — never torn JSON, never a lost update (plan §3.7(2)).
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFile } from "node:fs/promises";

import { parseTask, Store } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

describe("store concurrency", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;

  beforeEach(() => {
    dir = tempDir();
    store = new Store(dir.path, { watch: false });
  });

  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  test("N parallel updates leave one complete, parseable file", async () => {
    const created = await store.createTask({ title: "base", description: "" });
    const id = created.id;

    // 50 concurrent title rewrites + interleaved comment appends.
    const ops: Promise<unknown>[] = [];
    for (let i = 0; i < 50; i++) {
      ops.push(store.updateTask(id, { title: `title-${i}` }));
      ops.push(store.addComment(id, { author: "tester", text: `c-${i}` }));
    }
    await Promise.all(ops);

    // The file is complete + parseable (no torn JSON).
    const bytes = await readFile(store.paths.taskJson(id), "utf8");
    const task = parseTask(bytes); // throws on torn/invalid JSON
    expect(task.id).toBe(id);
    // The final title is one of the writes we issued (an atomic rename won).
    expect(task.title).toMatch(/^title-\d+$/);
    // created_at is preserved across every update.
    expect(task.created_at).toBe(created.created_at);

    // All 50 comment appends survived (no lost update under the comments lock).
    const comments = await store.listComments(id);
    expect(comments.length).toBe(50);
    const texts = new Set(comments.map((c) => c.text));
    expect(texts.size).toBe(50);
  });

  test("parallel creates of distinct tasks all land", async () => {
    const created = await Promise.all(
      Array.from({ length: 30 }, (_, i) => store.createTask({ title: `t-${i}` })),
    );
    const ids = new Set(created.map((t) => t.id));
    expect(ids.size).toBe(30);
    const views = await store.listTaskViews();
    expect(views.length).toBe(30);
    for (const v of views) expect(v.title).toMatch(/^t-\d+$/);
  });
});
