/**
 * Hybrid-ownership reconciliation (plan §3.7(2)).
 *
 * Matrix:
 *  - external edit of `title` on an idle task → accepted, visible via the store.
 *  - external edit of `blocked_by` on an idle task → accepted.
 *  - external edit of `status` on a task with a LIVE session → file restored
 *    from engine state, warning logged; a bundled `title` edit is kept.
 *  - the watcher does NOT echo the daemon's own writes back as external edits.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { readFile, rm, writeFile } from "node:fs/promises";

import { CapturingLogger, parseTask, serializeTask, Store } from "../src/index.ts";
import type { StoredTask } from "../src/index.ts";
import { fixedClock, tempDir, waitFor } from "./helpers.ts";

/** Rewrites task.json directly on disk, simulating a human/script edit. */
async function externalEdit(
  store: Store,
  id: string,
  mutate: (t: StoredTask) => void,
): Promise<void> {
  const path = store.paths.taskJson(id);
  const task = parseTask(await readFile(path, "utf8"));
  mutate(task);
  await writeFile(path, serializeTask(task));
}

describe("reconciliation — external edits to an idle task", () => {
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

  test("external title edit is accepted and visible via the store", async () => {
    const t = await store.createTask({ title: "original" });
    await externalEdit(store, t.id, (task) => {
      task.title = "edited-by-human";
    });
    await store.reconcileTask(t.id);

    const view = await store.taskView(t.id);
    expect(view.title).toBe("edited-by-human");
    expect(log.warns).toHaveLength(0);
  });

  test("external blocked_by edit is accepted", async () => {
    const blocker = await store.createTask({ title: "blocker" });
    const t = await store.createTask({ title: "t" });
    await externalEdit(store, t.id, (task) => {
      task.blocked_by = [blocker.id];
    });
    await store.reconcileTask(t.id);

    const view = await store.taskView(t.id);
    expect(view.blocked_by.map((b) => b.id)).toEqual([blocker.id]);
    expect(view.blocked).toBe(true);
    expect(log.warns).toHaveLength(0);
  });

  test("external deletion of an idle task is accepted: dropped from the store", async () => {
    const keep = await store.createTask({ title: "keep" });
    const gone = await store.createTask({ title: "gone" });
    await rm(store.paths.taskJson(gone.id)); // a human deletes the file
    await store.reconcileTask(gone.id);

    const ids = (await store.listTaskViews()).map((v) => v.id);
    expect(ids).toContain(keep.id);
    expect(ids).not.toContain(gone.id);
    expect(log.warns).toHaveLength(0); // an idle deletion is accepted silently
  });
});

describe("reconciliation — engine-owned edits with a live session", () => {
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

  /** Creates a task, enrolls it, and gives it a live (running) session. */
  async function enrolledWithLiveSession(): Promise<string> {
    const t = await store.createTask({ title: "enrolled" });
    await store.setPosition(t.id, { status: "work", workflow: "feature-dev", step: "dev" });
    await store.sessions.create({
      id: "0190a1b2-c3d4-7e5f-8a9b-000000000001",
      task_id: t.id,
      workflow: "feature-dev",
      step: "dev",
      agent: "@autosk/pi-agent/dev",
      cwd: dir.path,
      timestamp: "2026-01-01T00:00:00Z",
    });
    await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-000000000001", { status: "running" });
    return t.id;
  }

  test("external status edit is rejected: file restored from engine state + warning", async () => {
    const id = await enrolledWithLiveSession();
    expect(store.sessions.hasLiveSession(id)).toBe(true);

    await externalEdit(store, id, (task) => {
      task.status = "done"; // engine-owned: must be rejected
    });
    await store.reconcileTask(id);

    // The file on disk is back to the engine's status.
    const onDisk = parseTask(await readFile(store.paths.taskJson(id), "utf8"));
    expect(onDisk.status).toBe("work");
    expect(onDisk.step).toBe("dev");
    expect(onDisk.workflow).toBe("feature-dev");

    const view = await store.taskView(id);
    expect(view.status).toBe("work");
    expect(log.warns.length).toBeGreaterThan(0);
    expect(log.warns.some((w) => w.includes(id) && w.includes("engine-owned"))).toBe(true);
  });

  test("a bundled title edit is kept while status is restored", async () => {
    const id = await enrolledWithLiveSession();
    await externalEdit(store, id, (task) => {
      task.status = "cancel"; // rejected
      task.title = "human-renamed"; // accepted
    });
    await store.reconcileTask(id);

    const onDisk = parseTask(await readFile(store.paths.taskJson(id), "utf8"));
    expect(onDisk.status).toBe("work"); // restored
    expect(onDisk.title).toBe("human-renamed"); // kept
  });

  test("a tampered id field never spawns a phantom task dir (C2)", async () => {
    const id = await enrolledWithLiveSession();
    // A human edits BOTH an illegal engine-owned field AND the id field.
    await externalEdit(store, id, (task) => {
      task.status = "done"; // illegal (live session) → must be rejected
      task.id = "ask-evil99"; // tampered → must NOT become the write path
    });
    await store.reconcileTask(id);

    // The bogus id never became a directory; the canonical dir name wins.
    expect(existsSync(store.paths.taskDir("ask-evil99"))).toBe(false);
    // The REAL dir was restored from engine state, with the correct id field.
    const onDisk = parseTask(await readFile(store.paths.taskJson(id), "utf8"));
    expect(onDisk.id).toBe(id);
    expect(onDisk.status).toBe("work");
    // The view resolves by the real id, not the phantom one.
    expect((await store.taskView(id)).id).toBe(id);
    await expect(store.taskView("ask-evil99")).rejects.toThrow(/task not found/);
  });

  test("external deletion of a live-session task is rejected: file recreated + warning", async () => {
    const id = await enrolledWithLiveSession();
    await rm(store.paths.taskJson(id)); // a human deletes the file mid-session
    await store.reconcileTask(id);

    // The engine recreates the record from its last-known-good state.
    const onDisk = parseTask(await readFile(store.paths.taskJson(id), "utf8"));
    expect(onDisk.status).toBe("work");
    expect(onDisk.step).toBe("dev");
    expect(onDisk.workflow).toBe("feature-dev");

    // Still visible via the store, and the rejection was warned.
    const view = await store.taskView(id);
    expect(view.status).toBe("work");
    expect(log.warns.some((w) => w.includes(id) && w.includes("deleted externally"))).toBe(true);
  });

  test("once the session is no longer live, status edits are accepted again", async () => {
    const id = await enrolledWithLiveSession();
    await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-000000000001", {
      status: "done",
      ended_at: "2026-01-01T00:01:00Z",
    });
    expect(store.sessions.hasLiveSession(id)).toBe(false);

    await externalEdit(store, id, (task) => {
      task.status = "human";
    });
    await store.reconcileTask(id);

    const view = await store.taskView(id);
    expect(view.status).toBe("human");
  });
});

describe("reconciliation — a mutation never clobbers a pending external edit (C1)", () => {
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

  test("an accepted external title edit survives a concurrent updateTask(description)", async () => {
    const t = await store.createTask({ title: "A", description: "X" });
    // A human edits the title directly on disk; the store has NOT reconciled yet
    // (watch:false, no read since the edit).
    await externalEdit(store, t.id, (task) => {
      task.title = "B-human-edit";
    });
    // The engine now mutates a DIFFERENT field.
    await store.updateTask(t.id, { description: "Y" });

    // Both edits survive: the mutation reconciled-before-mutate, so it built on
    // the current disk title — not a stale cache that would resurrect "A".
    const onDisk = parseTask(await readFile(store.paths.taskJson(t.id), "utf8"));
    expect(onDisk.title).toBe("B-human-edit");
    expect(onDisk.description).toBe("Y");
    expect(log.warns).toHaveLength(0); // an idle title edit is accepted silently
  });

  test("a mutation does NOT launder an illegal engine-owned external edit", async () => {
    const t = await store.createTask({ title: "enrolled" });
    await store.setPosition(t.id, { status: "work", workflow: "wf", step: "dev" });
    await store.sessions.create({
      id: "0190a1b2-c3d4-7e5f-8a9b-0000000000c1",
      task_id: t.id,
      workflow: "wf",
      step: "dev",
      agent: "a",
      cwd: dir.path,
      timestamp: "2026-01-01T00:00:00Z",
    });
    await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-0000000000c1", { status: "running" });

    // A human illegally flips an engine-owned field on a live-session task, then
    // the engine mutates a human field.
    await externalEdit(store, t.id, (task) => {
      task.status = "done";
    });
    await store.updateTask(t.id, { description: "engine note" });

    // The illegal status was rejected (restored), not laundered into the write.
    const onDisk = parseTask(await readFile(store.paths.taskJson(t.id), "utf8"));
    expect(onDisk.status).toBe("work");
    expect(onDisk.description).toBe("engine note");
    expect(log.warns.some((w) => w.includes(t.id) && w.includes("engine-owned"))).toBe(true);
  });
});

describe("reconciliation — watcher echo suppression", () => {
  test("the watcher does not treat the daemon's own writes as external edits", async () => {
    const dir = tempDir();
    const log = new CapturingLogger();
    // Live watcher with a fast safety rescan, so reconciliation definitely runs.
    const store = new Store(dir.path, {
      watch: { rescanIntervalMs: 30, debounceMs: 5 },
      logger: log,
    });
    try {
      await store.open();
      const t = await store.createTask({ title: "owned" });
      await store.setPosition(t.id, { status: "work", workflow: "wf", step: "s1" });
      await store.sessions.create({
        id: "0190a1b2-c3d4-7e5f-8a9b-00000000000a",
        task_id: t.id,
        workflow: "wf",
        step: "s1",
        agent: "a",
        cwd: dir.path,
        timestamp: "2026-01-01T00:00:00Z",
      });
      await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-00000000000a", { status: "running" });

      // A burst of legitimate daemon writes to the live task's engine-owned step.
      for (let i = 0; i < 5; i++) {
        await store.setPosition(t.id, { status: "work", workflow: "wf", step: `s${i}` });
      }

      // Let the watcher + at least one safety rescan fire over our own writes.
      await new Promise((r) => setTimeout(r, 120));

      // None of the daemon's own writes were misread as a rejected external edit.
      expect(log.warns).toHaveLength(0);
      const view = await store.taskView(t.id);
      expect(view.step).toBe("s4");
    } finally {
      await store.close();
      dir.cleanup();
    }
  });

  test("the live watcher picks up a real external edit on its own (no store read)", async () => {
    const dir = tempDir();
    const log = new CapturingLogger();
    const store = new Store(dir.path, {
      watch: { rescanIntervalMs: 30, debounceMs: 5 },
      logger: log,
    });
    try {
      await store.open();
      // Enrol with a live session so the watcher's reconcile has an OBSERVABLE
      // side effect (restore + warn) that does not require a reconcile-on-read.
      const t = await store.createTask({ title: "enrolled" });
      await store.setPosition(t.id, { status: "work", workflow: "wf", step: "dev" });
      await store.sessions.create({
        id: "0190a1b2-c3d4-7e5f-8a9b-00000000000b",
        task_id: t.id,
        workflow: "wf",
        step: "dev",
        agent: "a",
        cwd: dir.path,
        timestamp: "2026-01-01T00:00:00Z",
      });
      await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-00000000000b", { status: "running" });

      await externalEdit(store, t.id, (task) => {
        task.status = "done"; // engine-owned: the watcher must reject + restore
      });

      // Observe ONLY via the raw file + the log — no store read triggers reconcile.
      await waitFor(async () => {
        const onDisk = parseTask(await readFile(store.paths.taskJson(t.id), "utf8"));
        return onDisk.status === "work" && log.warns.length > 0;
      });
    } finally {
      await store.close();
      dir.cleanup();
    }
  });

  test("a watcher-only store (no safety rescan) picks up an external edit via fs.watch", async () => {
    const dir = tempDir();
    const log = new CapturingLogger();
    // rescanIntervalMs:0 DISABLES the periodic safety net, so ONLY a genuine
    // fs.watch event can drive reconciliation — this actually exercises the
    // watcher (the rescan-enabled variants above could pass via the rescan alone).
    const store = new Store(dir.path, {
      watch: { rescanIntervalMs: 0, debounceMs: 5 },
      logger: log,
    });
    try {
      await store.open();
      const t = await store.createTask({ title: "enrolled" });
      await store.setPosition(t.id, { status: "work", workflow: "wf", step: "dev" });
      await store.sessions.create({
        id: "0190a1b2-c3d4-7e5f-8a9b-00000000000c",
        task_id: t.id,
        workflow: "wf",
        step: "dev",
        agent: "a",
        cwd: dir.path,
        timestamp: "2026-01-01T00:00:00Z",
      });
      await store.sessions.patchMeta("0190a1b2-c3d4-7e5f-8a9b-00000000000c", { status: "running" });

      await externalEdit(store, t.id, (task) => {
        task.status = "done"; // engine-owned: the watcher must reject + restore
      });

      // A real fs.watch event is REQUIRED here (no rescan fallback). macOS
      // recursive watch (FSEvents) can silently DROP the lone event under
      // parallel-suite load and needs a beat to warm up after the watch starts —
      // and with the rescan disabled there is nothing to recover a dropped event,
      // so a single edit racing a cold/overloaded watcher would hang the full
      // 20s (B1: this flaked ~12% even at a 20s deadline under parallel
      // `bun test`). Re-emit the edit until the watcher catches ONE event: the
      // store's signature-based reconcile is idempotent, a healthy watcher still
      // passes on the first event in tens of ms, and only a genuinely-broken
      // (never-delivering) watcher reaches the deadline.
      let lastPoke = 0;
      await waitFor(async () => {
        let onDisk;
        try {
          onDisk = parseTask(await readFile(store.paths.taskJson(t.id), "utf8"));
        } catch {
          return false; // torn read mid-write — retry
        }
        if (onDisk.status === "work" && log.warns.length > 0) return true;
        const now = Date.now();
        if (onDisk.status !== "work" && now - lastPoke > 250) {
          lastPoke = now;
          await externalEdit(store, t.id, (task) => {
            task.status = "done";
          });
        }
        return false;
      }, 20000);
    } finally {
      await store.close();
      dir.cleanup();
    }
    // Bun's per-test timeout defaults to 5s, which would preempt the 20s waitFor
    // above; give the test a deadline past its own waitFor so only a
    // genuinely-broken watcher fails.
  }, 25000);
});
