/**
 * Subscriptions (plan §4 acceptance):
 *  - `session.subscribe` does replay-then-tail honouring `from_line`;
 *  - `task-changed` fires on an EXTERNAL file edit the P2 watcher reconciles;
 *  - `project-changed` fires on `project.add`.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import type { AgentDefinition, SessionChangedParams, SessionEventParams } from "@autosk/sdk";

import { canonicalize } from "../src/index.ts";
import { gate } from "./engineHarness.ts";
import { waitFor } from "./helpers.ts";
import { startTestDaemon, type RpcClient, type TestDaemon } from "./rpcHarness.ts";

/** session-event params off a notification frame. */
function ev(n: { params: unknown }): SessionEventParams {
  return n.params as SessionEventParams;
}

/** session-changed params off a notification frame. */
function sc(n: { params: unknown }): SessionChangedParams {
  return n.params as SessionChangedParams;
}

describe("session.subscribe replay-then-tail", () => {
  let td: TestDaemon;
  let cwd: string;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("sess");
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("replays from from_line, then live-tails new entries and the done frame", async () => {
    const release = gate();
    const agent: AgentDefinition = {
      onRun: async (ctx) => {
        ctx.log.custom("test:hello", { n: 1 });
        await release.wait;
        ctx.log.custom("test:bye", { n: 2 });
        await ctx.transit({ status: "done" });
      },
    };
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", { name: "logger", firstStep: "logger", steps: { logger: agent } });

    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "logged" });
    await client.call("task.enroll", { cwd, id: task.id, workflow: "logger" });

    // Wait until the session is live and has logged `test:hello` (so the replay
    // has something past the header to return).
    let sessionId = "";
    await waitFor(async () => {
      const sessions = await client.call<{ id: string; status: string }[]>("session.list", { cwd });
      if (sessions.length === 0) return false;
      sessionId = sessions[0]!.id;
      const { entries } = await client.call<{ entries: { customType?: string }[] }>("session.transcript", {
        cwd,
        id: sessionId,
      });
      return entries.some((e) => e.customType === "test:hello");
    });

    // Subscribe from line 2 (skip the header) — replay-then-tail.
    await client.call("session.subscribe", { cwd, id: sessionId, from_line: 2 });

    // The replay must skip the header (line 1) and include `test:hello`.
    const helloFrame = await client.waitForNotification(
      (n) => n.method === "session-event" && ev(n).event?.type === "custom" && (ev(n).event as { customType?: string }).customType === "test:hello",
    );
    expect(ev(helloFrame).kind).toBe("message");
    expect(ev(helloFrame).line).toBe(2);
    // No replayed frame carried the header (type:"session") because from_line=2.
    const headerReplayed = client.notifications.some(
      (n) => n.method === "session-event" && ev(n).event?.type === "session",
    );
    expect(headerReplayed).toBe(false);

    // Release the agent: it logs `test:bye` then transits done → live tail.
    release.open();

    const byeFrame = await client.waitForNotification(
      (n) =>
        n.method === "session-event" &&
        ev(n).event?.type === "custom" &&
        (ev(n).event as { customType?: string }).customType === "test:bye",
    );
    expect(ev(byeFrame).kind).toBe("message");

    const doneFrame = await client.waitForNotification(
      (n) => n.method === "session-event" && ev(n).kind === "done",
    );
    expect(ev(doneFrame).session?.status).toBe("done");
  });

  test("an out-of-range from_line clamps to the tail and still tails later appends (review #7)", async () => {
    const release = gate();
    const agent: AgentDefinition = {
      onRun: async (ctx) => {
        ctx.log.custom("test:hello", { n: 1 });
        await release.wait;
        ctx.log.custom("test:late", { n: 2 });
        await ctx.transit({ status: "done" });
      },
    };
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", { name: "logger2", firstStep: "logger2", steps: { logger2: agent } });

    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "logged" });
    await client.call("task.enroll", { cwd, id: task.id, workflow: "logger2" });

    let sessionId = "";
    await waitFor(async () => {
      const sessions = await client.call<{ id: string; status: string }[]>("session.list", { cwd });
      if (sessions.length === 0) return false;
      sessionId = sessions[0]!.id;
      const { entries } = await client.call<{ entries: { customType?: string }[] }>("session.transcript", {
        cwd,
        id: sessionId,
      });
      return entries.some((e) => e.customType === "test:hello");
    });

    // Subscribe with a from_line FAR past the current tail. The clamp must pull
    // the cursor back to the tail so later appends (which land below 9999) still
    // tail; without it the cursor sticks at 9999 and `test:late` is dropped.
    await client.call("session.subscribe", { cwd, id: sessionId, from_line: 9999 });

    release.open();

    const lateFrame = await client.waitForNotification(
      (n) =>
        n.method === "session-event" &&
        ev(n).event?.type === "custom" &&
        (ev(n).event as { customType?: string }).customType === "test:late",
    );
    expect(ev(lateFrame).kind).toBe("message");
  });

  test("subscribing to an unknown session → NOT_FOUND", async () => {
    const client = await td.client();
    const frame = await client.callRaw("session.subscribe", {
      cwd,
      id: "00000000-0000-7000-8000-000000000000",
    });
    expect(frame.error?.code).toBeDefined();
  });
});

describe("task-changed on an external file edit (P2 watcher)", () => {
  let td: TestDaemon;
  let cwd: string;

  beforeEach(async () => {
    // Watcher ON with a fast rescan backstop so the test is not at the mercy of
    // fs.watch latency/availability.
    td = await startTestDaemon({ storeWatch: { debounceMs: 10, rescanIntervalMs: 120 } });
    cwd = await td.makeProject("watched");
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("an external task.json edit pushes task-changed to a subscriber", async () => {
    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "original" });
    await client.call("task.subscribe", { cwd });

    // Externally rewrite the task's title (bypassing the daemon).
    const taskJson = join(cwd, ".autosk", "tasks", task.id, "task.json");
    const record = JSON.parse(readFileSync(taskJson, "utf8"));
    record.title = "edited externally";
    writeFileSync(taskJson, JSON.stringify(record, null, 2) + "\n");

    const note = await client.waitForNotification(
      (n) =>
        n.method === "task-changed" &&
        (n.params as { task: { id: string; title: string } }).task.id === task.id &&
        (n.params as { task: { title: string } }).task.title === "edited externally",
    );
    expect((note.params as { task: { title: string } }).task.title).toBe("edited externally");
  });
});

describe("session-changed project channel", () => {
  let td: TestDaemon;
  let cwd: string;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("sesschan");
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("session.subscribeProject pushes queued+running then terminal WITHOUT a per-session subscribe", async () => {
    const release = gate();
    const agent: AgentDefinition = {
      onRun: async (ctx) => {
        await release.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", { name: "gated", firstStep: "gated", steps: { gated: agent } });

    const client = await td.client();
    // Subscribe to the project session channel BEFORE any session exists.
    await client.call("session.subscribeProject", { cwd });

    const task = await client.call<{ id: string }>("task.create", { cwd, title: "live" });
    await client.call("task.enroll", { cwd, id: task.id, workflow: "gated" });

    // A `running` frame arrives for this task's session even though we never
    // learned the session id to `session.subscribe` it.
    const runningFrame = await client.waitForNotification(
      (n) =>
        n.method === "session-changed" &&
        sc(n).session.task_id === task.id &&
        sc(n).session.status === "running",
    );
    const sessionId = sc(runningFrame).session.id;
    expect(sc(runningFrame).root).toBe(await canonicalize(cwd));

    // The create (queued) frame was pushed too — proves "created" coverage.
    const sawQueued = client.notifications.some(
      (n) =>
        n.method === "session-changed" &&
        sc(n).session.id === sessionId &&
        sc(n).session.status === "queued",
    );
    expect(sawQueued).toBe(true);

    // Releasing the gate lets the agent transit → a terminal `done` frame.
    release.open();
    const doneFrame = await client.waitForNotification(
      (n) =>
        n.method === "session-changed" &&
        sc(n).session.id === sessionId &&
        sc(n).session.status === "done",
    );
    expect(sc(doneFrame).session.status).toBe("done");
  });

  test("session.unsubscribeProject stops the pushes", async () => {
    const agent: AgentDefinition = {
      onRun: async (ctx) => ctx.transit({ status: "done" }),
    };
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", { name: "quick", firstStep: "quick", steps: { quick: agent } });

    const client = await td.client();
    await client.call("session.subscribeProject", { cwd });
    await client.call("session.unsubscribeProject", { cwd });

    const task = await client.call<{ id: string }>("task.create", { cwd, title: "silent" });
    await client.call("task.enroll", { cwd, id: task.id, workflow: "quick" });

    // Drive the session to completion via the per-session list, then assert no
    // session-changed frame was ever delivered after the unsubscribe.
    await waitFor(async () => {
      const sessions = await client.call<{ status: string }[]>("session.list", { cwd });
      return sessions.length > 0 && sessions.every((s) => s.status === "done");
    });
    expect(client.notifications.some((n) => n.method === "session-changed")).toBe(false);
  });
});

describe("project-changed on project.add", () => {
  let td: TestDaemon;

  beforeEach(async () => {
    td = await startTestDaemon();
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("project.add fans out a project-changed notification", async () => {
    const client = await td.client();
    await client.call("project.subscribe", {});
    const projDir = await td.makeProject("added");

    const added = await client.call<{ root: string }>("project.add", { cwd: projDir });

    const note = await client.waitForNotification((n) => n.method === "project-changed");
    const project = (note.params as { project: { root: string } }).project;
    expect(project.root).toBe(await canonicalize(projDir));
    expect(added.root).toBe(await canonicalize(projDir));
  });
});
