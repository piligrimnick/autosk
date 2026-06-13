/**
 * Error envelope mapping (plan §4 acceptance): EngineError codes pass straight
 * through (enroll on a non-`new` task → CONFLICT 1004; unknown id → NOT_FOUND
 * 1003), malformed params → INVALID_PARAMS, the project selector errors map to
 * INVALID_PROJECT / PROJECT_NOT_FOUND, and a malformed line never kills the
 * connection.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { join } from "node:path";

import { ErrorCodes } from "@autosk/sdk";

import { startTestDaemon, type TestDaemon } from "./rpcHarness.ts";

describe("RPC error mapping", () => {
  let td: TestDaemon;
  let cwd: string;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("errs");
    // Register a human-first-step workflow so an enrolled task lands non-`new`
    // without spawning a session (the engine never schedules a human step).
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", {
      name: "wf",
      firstStep: "review",
      steps: { review: { human: true } },
    });
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("unknown task id → NOT_FOUND (1003)", async () => {
    const client = await td.client();
    for (const method of ["task.get", "task.resume", "task.done", "task.cancel", "task.reopen"]) {
      const frame = await client.callRaw(method, { cwd, id: "ask-zzzzzz" });
      expect(frame.error?.code).toBe(ErrorCodes.NOT_FOUND);
    }
  });

  test("enroll on an already-enrolled task → CONFLICT (1004)", async () => {
    const client = await td.client();
    const task = await client.call<{ id: string; status: string }>("task.create", { cwd, title: "t" });
    const enrolled = await client.call<{ status: string }>("task.enroll", { cwd, id: task.id, workflow: "wf" });
    expect(enrolled.status).toBe("human"); // human first step parks it, no session
    const frame = await client.callRaw("task.enroll", { cwd, id: task.id, workflow: "wf" });
    expect(frame.error?.code).toBe(ErrorCodes.CONFLICT);
  });

  test("resume on a non-parked task → CONFLICT (1004)", async () => {
    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "t" });
    const frame = await client.callRaw("task.resume", { cwd, id: task.id });
    expect(frame.error?.code).toBe(ErrorCodes.CONFLICT);
  });

  test("enroll with an unknown workflow → INVALID_PARAMS (-32602)", async () => {
    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "t" });
    const frame = await client.callRaw("task.enroll", { cwd, id: task.id, workflow: "ghost" });
    expect(frame.error?.code).toBe(ErrorCodes.INVALID_PARAMS);
  });

  test("enroll with both workflow and agent → INVALID_PARAMS", async () => {
    const client = await td.client();
    const task = await client.call<{ id: string }>("task.create", { cwd, title: "t" });
    const frame = await client.callRaw("task.enroll", { cwd, id: task.id, workflow: "wf", agent: "a" });
    expect(frame.error?.code).toBe(ErrorCodes.INVALID_PARAMS);
  });

  test("session.input / abort on an unknown session → NOT_FOUND", async () => {
    const client = await td.client();
    const input = await client.callRaw("session.input", {
      cwd,
      id: "00000000-0000-7000-8000-000000000000",
      message: "hi",
      kind: "steer",
    });
    expect(input.error?.code).toBe(ErrorCodes.NOT_FOUND);
    const abort = await client.callRaw("session.abort", { cwd, id: "00000000-0000-7000-8000-000000000000" });
    expect(abort.error?.code).toBe(ErrorCodes.NOT_FOUND);
  });

  test("an empty / relative cwd → INVALID_PROJECT (1002)", async () => {
    const client = await td.client();
    expect((await client.callRaw("task.list", { cwd: "" })).error?.code).toBe(ErrorCodes.INVALID_PROJECT);
    expect((await client.callRaw("task.list", { cwd: "relative/path" })).error?.code).toBe(
      ErrorCodes.INVALID_PROJECT,
    );
  });

  test("a cwd with no .autosk/ → PROJECT_NOT_FOUND (1001)", async () => {
    const client = await td.client();
    const frame = await client.callRaw("task.list", { cwd: join(td.dir, "no-project-here") });
    expect(frame.error?.code).toBe(ErrorCodes.PROJECT_NOT_FOUND);
  });

  test("missing params object → INVALID_PARAMS", async () => {
    const client = await td.client();
    expect((await client.callRaw("task.get", null)).error?.code).toBe(ErrorCodes.INVALID_PARAMS);
    expect((await client.callRaw("task.get", { cwd })).error?.code).toBe(ErrorCodes.INVALID_PARAMS); // missing id
  });

  test("a malformed line gets PARSE_ERROR but does NOT kill the connection", async () => {
    const client = await td.client();
    client.sendRawLine("{ this is not json");
    const parseErr = await client.waitForFrame((f) => f.error?.code === ErrorCodes.PARSE_ERROR);
    expect(parseErr.error?.code).toBe(ErrorCodes.PARSE_ERROR);
    // The same connection still serves the next request.
    const version = await client.call<{ version: string }>("meta.version", null);
    expect(typeof version.version).toBe("string");
  });
});
