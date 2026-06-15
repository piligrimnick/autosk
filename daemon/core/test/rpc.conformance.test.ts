/**
 * Proto-v2 conformance (plan §4 acceptance): the live daemon's registered
 * handler set matches `RPC_METHODS` with no drift in either direction, the
 * "deliberately absent" v1 methods are unregistered, and every name in
 * `RPC_METHODS` round-trips over UDS (a structured response, never
 * METHOD_NOT_FOUND).
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { ErrorCodes, RPC_METHODS } from "@autosk/sdk";
import { startTestDaemon, TEST_TOKEN, type TestDaemon } from "./rpcHarness.ts";

const ABSENT_SESSION_ID = "00000000-0000-7000-8000-000000000000";

/** Minimal, never-METHOD_NOT_FOUND params for each method (errors are fine). */
function paramsFor(method: string, cwd: string, initDir: string): unknown {
  switch (method) {
    case "meta.version":
    case "meta.healthz":
    case "project.list":
      return null;
    case "meta.auth":
      return { token: TEST_TOKEN };
    case "project.add":
    case "project.remove":
    case "project.diagnostics":
    case "project.subscribe":
    case "project.unsubscribe":
    case "task.list":
    case "task.subscribe":
    case "task.unsubscribe":
    case "registry.workflow.list":
    case "session.list":
      return { cwd };
    case "project.init":
      return { cwd: initDir };
    case "task.get":
    case "task.resume":
    case "task.done":
    case "task.cancel":
    case "task.reopen":
      return { cwd, id: "ask-zzzzzz" };
    case "task.create":
      return { cwd, title: "round-trip" };
    case "task.update":
      return { cwd, id: "ask-zzzzzz", title: "x" };
    case "task.enroll":
      return { cwd, id: "ask-zzzzzz", workflow: "nope" };
    case "task.block":
    case "task.unblock":
      return { cwd, id: "ask-zzzzzz", blocked_by: "ask-aaaaaa" };
    case "task.comment.add":
      return { cwd, task_id: "ask-zzzzzz", text: "x" };
    case "task.comment.list":
      return { cwd, task_id: "ask-zzzzzz" };
    case "task.comment.edit":
      return { cwd, task_id: "ask-zzzzzz", comment_id: "cm-aaaaaa", text: "x" };
    case "task.comment.delete":
      return { cwd, task_id: "ask-zzzzzz", comment_id: "cm-aaaaaa" };
    case "registry.workflow.get":
      return { cwd, name: "nope" };
    case "extension.list":
      return { cwd };
    case "extension.install":
    case "extension.remove":
      // A bare token is rejected (INVALID_PARAMS) BEFORE any npm install — so the
      // round-trip never touches the network, only proves the method is wired.
      return { cwd, source: "not-a-valid-source" };
    case "session.get":
    case "session.transcript":
    case "session.subscribe":
    case "session.unsubscribe":
    case "session.abort":
      return { cwd, id: ABSENT_SESSION_ID };
    case "session.input":
      return { cwd, id: ABSENT_SESSION_ID, message: "x", kind: "steer" };
    default:
      return { cwd };
  }
}

describe("proto-v2 conformance (live daemon over UDS)", () => {
  let td: TestDaemon;
  let cwd: string;
  let initDir: string;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("conf");
    initDir = `${td.dir}/init-target`;
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("the registered handler set equals RPC_METHODS (no drift either way)", () => {
    const registered = [...td.runtime.daemon.registeredMethods()] as string[];
    const expected = [...RPC_METHODS] as string[];
    // Neither direction drifts.
    expect(registered.sort()).toEqual(expected.sort());
  });

  test("the 'deliberately absent' v1 methods are unregistered", async () => {
    const absent = [
      "sql.query",
      "sql.exec",
      "step.next",
      "signal.forTask",
      "signal.forJob",
      "job.list",
      "job.get",
      "maint.compact",
      "task.setPriority",
      "task.setStatus",
      "task.unblockAll",
      "workflow.create",
      "workflow.delete",
      "workflow.updateIsolation",
      "agent.install",
      "agent.uninstall",
      // v2 removed the agent registry: agents are inline step values now.
      "registry.agent.list",
    ];
    const registered = new Set(td.runtime.daemon.registeredMethods());
    const client = await td.client();
    for (const method of absent) {
      expect(registered.has(method as never)).toBe(false);
      const frame = await client.callRaw(method, { cwd });
      expect(frame.error?.code).toBe(ErrorCodes.METHOD_NOT_FOUND);
    }
  });

  test("every RPC_METHODS name (except meta.shutdown) round-trips, never METHOD_NOT_FOUND", async () => {
    const client = await td.client();
    for (const method of RPC_METHODS) {
      if (method === "meta.shutdown") continue; // round-tripped in its own test (it tears down)
      const frame = await client.callRaw(method, paramsFor(method, cwd, initDir));
      // A well-formed response: exactly one of result / error.
      const hasResult = frame.result !== undefined;
      const hasError = frame.error !== undefined;
      expect(hasResult || hasError).toBe(true);
      if (hasError) {
        expect(frame.error!.code).not.toBe(ErrorCodes.METHOD_NOT_FOUND);
      }
    }
  });
});

describe("meta.shutdown round-trips and tears the daemon down", () => {
  test("returns { ok: true } then exits", async () => {
    const td = await startTestDaemon({ shutdownDelayMs: 60 });
    const client = await td.client();
    const ok = await client.call<{ ok: boolean }>("meta.shutdown", null);
    expect(ok).toEqual({ ok: true });
    // The shutdown hook fires after the reply flushes; the harness `exit` recorder
    // resolves `exited`.
    const code = await td.exited;
    expect(code).toBe(0);
    await td.cleanup();
  });
});
