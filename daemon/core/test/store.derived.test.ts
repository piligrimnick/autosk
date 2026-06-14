/**
 * Derived data (plan §3.7(5)): `blocked` / `blocks` computed from `blocked_by`
 * edges across the project, plus the session-by-task index.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { Store } from "../src/index.ts";
import { tempDir } from "./helpers.ts";

describe("derived blocks / blocked", () => {
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

  test("blocks is the reverse of blocked_by; blocked tracks open blockers", async () => {
    const a = await store.createTask({ title: "A" });
    const b = await store.createTask({ title: "B", blocked_by: [a.id] });

    const viewB = await store.taskView(b.id);
    expect(viewB.blocked_by.map((r) => r.id)).toEqual([a.id]);
    expect(viewB.blocked).toBe(true); // A is open (new)

    const viewA = await store.taskView(a.id);
    expect(viewA.blocks.map((r) => r.id)).toEqual([b.id]);
    expect(viewA.blocked).toBe(false);

    // Closing the blocker clears B's blocked flag.
    await store.setPosition(a.id, { status: "done", workflow: null, step: null });
    const viewB2 = await store.taskView(b.id);
    expect(viewB2.blocked).toBe(false);
    // The edge (and its blocks/blocked_by refs) still exists, now showing done.
    expect(viewB2.blocked_by[0]?.status).toBe("done");
  });

  test("a TaskRef carries the referenced task's current status", async () => {
    const a = await store.createTask({ title: "A" });
    const b = await store.createTask({ title: "B", blocked_by: [a.id] });
    await store.setPosition(a.id, { status: "work", workflow: "wf", step: "s" });

    const viewB = await store.taskView(b.id);
    expect(viewB.blocked_by[0]).toEqual({ id: a.id, status: "work" });
    const viewA = await store.taskView(a.id);
    expect(viewA.blocks[0]).toEqual({ id: b.id, status: "new" });
  });

  test("comment_count reflects the comments file", async () => {
    const a = await store.createTask({ title: "A" });
    expect((await store.taskView(a.id)).comment_count).toBe(0);
    await store.addComment(a.id, { author: "x", text: "hi" });
    await store.addComment(a.id, { author: "x", text: "again" });
    expect((await store.taskView(a.id)).comment_count).toBe(2);
  });

  test("filters narrow the list view", async () => {
    const a = await store.createTask({ title: "A" });
    await store.createTask({ title: "B" });
    await store.setPosition(a.id, { status: "work", workflow: "wf", step: "dev" });

    expect((await store.listTaskViews({ status: "work" })).map((v) => v.id)).toEqual([a.id]);
    expect((await store.listTaskViews({ status: "new" })).length).toBe(1);
    expect((await store.listTaskViews({ workflow: "wf" })).map((v) => v.id)).toEqual([a.id]);
    expect((await store.listTaskViews({ status: ["new", "work"] })).length).toBe(2);
    // An EMPTY status list is "no constraint" (all statuses), not "match none":
    // the lazy dashboard and the CLI `--status all` lean on this.
    expect((await store.listTaskViews({ status: [] })).length).toBe(2);
  });
});

describe("session-by-task index", () => {
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

  test("sessionsForTask filters by task_id; liveSessionsForTask tracks status", async () => {
    const a = await store.createTask({ title: "A" });
    const b = await store.createTask({ title: "B" });

    await store.sessions.create({
      id: "0190a1b2-0000-7000-8000-000000000001",
      task_id: a.id,
      workflow: "wf",
      step: "dev",
      agent: "ag",
      cwd: dir.path,
      timestamp: "2026-01-01T00:00:00Z",
    });
    await store.sessions.create({
      id: "0190a1b2-0000-7000-8000-000000000002",
      task_id: a.id,
      workflow: "wf",
      step: "review",
      agent: "ag",
      cwd: dir.path,
      timestamp: "2026-01-01T00:01:00Z",
    });
    await store.sessions.create({
      id: "0190a1b2-0000-7000-8000-000000000003",
      task_id: b.id,
      workflow: "wf",
      step: "dev",
      agent: "ag",
      cwd: dir.path,
      timestamp: "2026-01-01T00:02:00Z",
    });

    // Newest-id first (descending) is the default order.
    expect(store.sessions.sessionsForTask(a.id).map((m) => m.id)).toEqual([
      "0190a1b2-0000-7000-8000-000000000002",
      "0190a1b2-0000-7000-8000-000000000001",
    ]);
    expect(store.sessions.sessionsForTask(b.id)).toHaveLength(1);

    // All start queued → all live.
    expect(store.sessions.hasLiveSession(a.id)).toBe(true);
    expect(store.sessions.liveSessionsForTask(a.id)).toHaveLength(2);

    // Finish both of A's sessions → no longer live.
    await store.sessions.patchMeta("0190a1b2-0000-7000-8000-000000000001", { status: "done" });
    await store.sessions.patchMeta("0190a1b2-0000-7000-8000-000000000002", { status: "aborted" });
    expect(store.sessions.hasLiveSession(a.id)).toBe(false);
    expect(store.sessions.hasLiveSession(b.id)).toBe(true);
  });

  test("the index is rebuilt from disk on open()", async () => {
    // Seed sessions with one store, then re-open a fresh store over the same dir.
    {
      const seed = new Store(dir.path, { watch: false });
      const a = await seed.createTask({ title: "A" });
      await seed.sessions.create({
        id: "0190a1b2-0000-7000-8000-0000000000aa",
        task_id: a.id,
        workflow: "wf",
        step: "dev",
        agent: "ag",
        cwd: dir.path,
        timestamp: "2026-01-01T00:00:00Z",
      });
      await seed.sessions.patchMeta("0190a1b2-0000-7000-8000-0000000000aa", { status: "running" });
      await seed.close();
    }

    const reopened = new Store(dir.path, { watch: false });
    await reopened.open();
    const views = await reopened.listTaskViews();
    expect(views).toHaveLength(1);
    const taskId = views[0]!.id;
    expect(reopened.sessions.sessionsForTask(taskId)).toHaveLength(1);
    expect(reopened.sessions.hasLiveSession(taskId)).toBe(true);
    await reopened.close();
  });
});
