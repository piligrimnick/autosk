/**
 * Idle-shutdown (plan §4 acceptance): with a shortened window, an idle daemon
 * (no connections, no queued/running sessions, no `status=work` tasks) shuts
 * down and exits; a live connection OR a pending work task holds it open.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import type { AgentDefinition } from "@autosk/sdk";

import { startTestDaemon, type TestDaemon } from "./rpcHarness.ts";

/** Resolves with the promise's value if it settles within `ms`, else `"pending"`. */
function settledWithin<T>(p: Promise<T>, ms: number): Promise<T | "pending"> {
  return Promise.race([p, new Promise<"pending">((r) => setTimeout(() => r("pending"), ms))]);
}

describe("idle-shutdown", () => {
  let td: TestDaemon;

  beforeEach(async () => {
    td = await startTestDaemon({ idleWindowMs: 80, idleCheckMs: 20 });
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("an idle daemon shuts down after the window and exits 0", async () => {
    expect(await settledWithin(td.exited, 2000)).toBe(0);
  });

  test("a live connection holds the daemon open; closing it lets it shut down", async () => {
    const client = await td.client();
    // Keep the connection busy-ish so the harness keeps it alive.
    await client.call("meta.version", null);
    expect(await settledWithin(td.exited, 300)).toBe("pending");
    client.close();
    expect(await settledWithin(td.exited, 2000)).toBe(0);
  });

  test("a pending status=work task holds the daemon open", async () => {
    const cwd = await td.makeProject("busy");
    const handle = await td.handle(cwd);
    // An agent step, but the enrolled task is BLOCKED so the scheduler never
    // dispatches it — it sits at `status=work` (pending work) without ever
    // spawning a session.
    handle.extensions.addWorkflow("test", {
      name: "wf",
      firstStep: "todo",
      steps: { todo: { onRun: async () => {} } },
    });

    const client = await td.client();
    const blocker = await client.call<{ id: string }>("task.create", { cwd, title: "blocker" });
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "t" });
    await client.call("task.block", { cwd, id: task.id, blocked_by: blocker.id });
    const enrolled = await client.call<{ status: string }>("task.enroll", { cwd, id: task.id, workflow: "wf" });
    expect(enrolled.status).toBe("work");
    client.close(); // drop the connection so ONLY the work task keeps it alive

    expect(await settledWithin(td.exited, 300)).toBe("pending");
  });

  test("a queued/running session holds the daemon open", async () => {
    const cwd = await td.makeProject("running");
    const handle = await td.handle(cwd);
    const agent: AgentDefinition = {
      // Never transits until aborted/torn down — keeps a running session live.
      onRun: () => new Promise<void>(() => {}),
    };
    handle.extensions.addWorkflow("test", { name: "hang", firstStep: "do", steps: { do: agent } });

    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "t" });
    await client.call("task.enroll", { cwd, id: task.id, workflow: "hang" });
    client.close();

    expect(await settledWithin(td.exited, 300)).toBe("pending");
  });
});
