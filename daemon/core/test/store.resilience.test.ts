/**
 * Resilience + validation on the human-editable files (hybrid ownership).
 *
 * Under hybrid ownership a human can hand-edit `task.json` / `comments.jsonl`,
 * so a single corrupt file is foreseeable. None of it may brick the project:
 *  - a malformed `task.json` is skipped (or restored for a live session), never
 *    aborting `open()` / `listTaskViews()` for the other tasks.
 *  - a malformed `comments.jsonl` line is skipped, never failing the project
 *    list for unrelated tasks.
 *  - `addComment` refuses a non-existent task instead of orphaning a file.
 *  - `block()` refuses a self-block.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { appendFile, mkdir, writeFile } from "node:fs/promises";

import { CapturingLogger, serializeTask, Store, type StoredTask } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

describe("resilience — a corrupt task.json never bricks the project", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;
  let log: CapturingLogger;

  beforeEach(async () => {
    dir = tempDir();
    log = new CapturingLogger();
    store = new Store(dir.path, { watch: false, logger: log });
    await store.open();
  });
  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  /** Writes a `task.json` with garbage bytes for a brand-new task id. */
  async function seedCorruptTask(id: string): Promise<void> {
    await mkdir(store.paths.taskDir(id), { recursive: true });
    await writeFile(store.paths.taskJson(id), "{ not json");
  }

  test("listTaskViews skips the corrupt task and surfaces the good ones", async () => {
    const good = await store.createTask({ title: "good" });
    await seedCorruptTask("ask-badbad");

    const views = await store.listTaskViews(); // must NOT throw
    expect(views.map((v) => v.id)).toEqual([good.id]);
    expect(log.warns.some((w) => w.includes("ask-badbad") && w.includes("unparseable"))).toBe(true);
  });

  test("a fresh open() over a dir with a corrupt task.json does not throw", async () => {
    const good = await store.createTask({ title: "good" });
    await seedCorruptTask("ask-badbad");

    const log2 = new CapturingLogger();
    const reopened = new Store(dir.path, { watch: false, logger: log2 });
    await reopened.open(); // must NOT throw on the corrupt file
    const views = await reopened.listTaskViews();
    expect(views.map((v) => v.id)).toEqual([good.id]);
    expect(log2.warns.some((w) => w.includes("ask-badbad"))).toBe(true);
    await reopened.close();
  });

  test("a persistently-corrupt idle task.json warns once, not once per list (M2)", async () => {
    await store.createTask({ title: "good" });
    await seedCorruptTask("ask-badbad");

    await store.listTaskViews();
    await store.listTaskViews();
    await store.listTaskViews();

    // De-duped across the three reconcile sweeps: a polling client cannot spin
    // the daemon log on a file the operator has not fixed yet.
    const badWarns = log.warns.filter((w) => w.includes("ask-badbad") && w.includes("unparseable"));
    expect(badWarns).toHaveLength(1);
  });

  test("a previously-cached task that becomes corrupt drops from the list (C3)", async () => {
    const good = await store.createTask({ title: "good" });
    const doomed = await store.createTask({ title: "doomed" });
    // Both are cached + visible first.
    expect((await store.listTaskViews()).map((v) => v.id).sort()).toEqual(
      [good.id, doomed.id].sort(),
    );

    // A human corrupts the already-cached task's task.json.
    await writeFile(store.paths.taskJson(doomed.id), "{ not json");
    const views = await store.listTaskViews();

    // Consistent with a never-cached corrupt file: it VANISHES (not stale
    // last-known-good) — the corruption behaves the same way regardless of
    // whether the task was cached first.
    expect(views.map((v) => v.id)).toEqual([good.id]);
    expect(log.warns.some((w) => w.includes(doomed.id) && w.includes("unparseable"))).toBe(true);
    // taskView for the now-dropped task is a clean not-found, not a parse error.
    await expect(store.taskView(doomed.id)).rejects.toThrow(/task not found/);
  });

  test("fixing then re-breaking a task.json re-arms the warning (memo cleared)", async () => {
    const id = "ask-feed01";
    await seedCorruptTask(id);
    await store.listTaskViews(); // first warn

    // The operator rescues the file with a valid record.
    const rescued: StoredTask = {
      id,
      title: "rescued",
      description: "",
      status: "new",
      workflow: null,
      step: null,
      blocked_by: [],
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    };
    await writeFile(store.paths.taskJson(id), serializeTask(rescued));
    expect((await store.listTaskViews()).map((v) => v.id)).toContain(id); // parses + visible

    // It breaks again — the good parse cleared the memo, so this re-warns.
    await writeFile(store.paths.taskJson(id), "{ broken again");
    await store.listTaskViews();

    const warns = log.warns.filter((w) => w.includes(id) && w.includes("unparseable"));
    expect(warns).toHaveLength(2);
  });
});

describe("resilience — a corrupt comments.jsonl line never bricks the list", () => {
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

  test("a malformed line is skipped; comment_count + listComments stay valid", async () => {
    const a = await store.createTask({ title: "A" });
    const b = await store.createTask({ title: "B" }); // an unrelated task
    await store.addComment(a.id, { author: "x", text: "ok" });
    // A human fat-fingers a line into A's comments file.
    await appendFile(store.paths.commentsJsonl(a.id), "{ broken line\n");

    // The whole-project list must not throw, and B must still be visible.
    const views = await store.listTaskViews();
    expect(new Set(views.map((v) => v.id))).toEqual(new Set([a.id, b.id]));
    expect(views.find((v) => v.id === a.id)!.comment_count).toBe(1); // only the valid line
    expect(await store.listComments(a.id)).toHaveLength(1);
  });
});

describe("resilience — a skipped comment line is surfaced once (C5)", () => {
  let dir: ReturnType<typeof tempDir>;
  let store: Store;
  let log: CapturingLogger;

  beforeEach(async () => {
    dir = tempDir();
    log = new CapturingLogger();
    store = new Store(dir.path, { watch: false, logger: log });
    await store.open();
  });
  afterEach(async () => {
    await store.close();
    dir.cleanup();
  });

  test("a malformed comments.jsonl line warns once per signature, not per list", async () => {
    const a = await store.createTask({ title: "A" });
    await store.addComment(a.id, { author: "x", text: "ok" });
    // A human fat-fingers a line in.
    await appendFile(store.paths.commentsJsonl(a.id), "{ broken line\n");

    await store.listTaskViews();
    await store.listTaskViews();
    await store.listTaskViews();

    // De-duped: a polling client cannot spin the log on an unfixed file, but the
    // dropped human comment is no longer completely invisible.
    const skipWarns = log.warns.filter((w) => w.includes(a.id) && w.includes("malformed"));
    expect(skipWarns).toHaveLength(1);
    expect((await store.listTaskViews()).find((v) => v.id === a.id)!.comment_count).toBe(1);
  });
});

describe("validation — comment + block edge cases", () => {
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

  test("addComment on a non-existent task throws and orphans nothing", async () => {
    await expect(store.addComment("ask-nope01", { author: "x", text: "hi" })).rejects.toThrow(
      /task not found/,
    );
    expect(existsSync(store.paths.taskDir("ask-nope01"))).toBe(false);
  });

  test("editComment / deleteComment on a non-existent task throw", async () => {
    await expect(store.editComment("ask-nope02", "cm-000000", "x")).rejects.toThrow(/task not found/);
    await expect(store.deleteComment("ask-nope02", "cm-000000")).rejects.toThrow(/task not found/);
  });

  test("block() rejects a self-block; blocked_by stays empty", async () => {
    const a = await store.createTask({ title: "A" });
    await expect(store.block(a.id, a.id)).rejects.toThrow(/cannot block itself/);
    const view = await store.taskView(a.id);
    expect(view.blocked_by).toEqual([]);
    expect(view.blocked).toBe(false);
  });

  test("comment ids are unique within a task (the edit/delete key) (M1)", async () => {
    const t = await store.createTask({ title: "chatty" });
    const ids = new Set<string>();
    for (let i = 0; i < 200; i++) {
      const c = await store.addComment(t.id, { author: "x", text: `m${i}` });
      expect(ids.has(c.id)).toBe(false); // never retargets an existing comment
      ids.add(c.id);
    }
    expect(ids.size).toBe(200);
  });
});
