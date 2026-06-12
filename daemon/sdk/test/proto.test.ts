import { describe, expect, test } from "bun:test";
import {
  ErrorCodes,
  RPC_METHODS,
  RPC_NOTIFICATIONS,
  type ParamsOf,
  type ResultOf,
  type RpcMethod,
} from "../src/index.ts";

/**
 * The proto-v2 surface as documented in plan §4. This list is the diffable
 * artifact P5/P7/P8 check their implementations against: if a method is added
 * to `RPC_METHODS` without updating §4 (or vice versa), this test fails. The
 * type map (`RpcMethodMap`) is pinned to `RPC_METHODS` by compile-time
 * assertions in `proto.ts`, so a method present in the runtime list is also
 * guaranteed to carry a param/result type.
 */
const PLAN_SECTION_4_METHODS = [
  // meta
  "meta.version",
  "meta.auth",
  "meta.healthz",
  "meta.shutdown",
  // project
  "project.list",
  "project.add",
  "project.remove",
  "project.init",
  "project.diagnostics",
  "project.subscribe",
  "project.unsubscribe",
  // task
  "task.list",
  "task.get",
  "task.create",
  "task.update",
  "task.enroll",
  "task.resume",
  "task.done",
  "task.cancel",
  "task.reopen",
  "task.block",
  "task.unblock",
  "task.comment.add",
  "task.comment.list",
  "task.comment.edit",
  "task.comment.delete",
  "task.subscribe",
  "task.unsubscribe",
  // registry
  "registry.workflow.list",
  "registry.workflow.get",
  "registry.agent.list",
  // session
  "session.list",
  "session.get",
  "session.transcript",
  "session.subscribe",
  "session.unsubscribe",
  "session.input",
  "session.abort",
].sort();

const PLAN_SECTION_4_NOTIFICATIONS = ["task-changed", "project-changed", "session-event"].sort();

describe("proto-v2 method manifest", () => {
  test("RPC_METHODS matches plan §4 exactly", () => {
    expect(([...RPC_METHODS] as string[]).sort()).toEqual(PLAN_SECTION_4_METHODS);
  });

  test("RPC_METHODS has no duplicates", () => {
    expect(new Set(RPC_METHODS).size).toBe(RPC_METHODS.length);
  });

  test("every method is namespaced under a known domain", () => {
    const domains = new Set(["meta", "project", "task", "registry", "session"]);
    for (const method of RPC_METHODS) {
      expect(domains.has(method.split(".")[0]!)).toBe(true);
    }
  });
});

describe("proto-v2 notification manifest", () => {
  test("RPC_NOTIFICATIONS matches plan §4 exactly", () => {
    expect(([...RPC_NOTIFICATIONS] as string[]).sort()).toEqual(PLAN_SECTION_4_NOTIFICATIONS);
  });
});

describe("proto-v2 error codes", () => {
  test("mirror v1 autosk-proto error_codes", () => {
    expect(ErrorCodes.PARSE_ERROR).toBe(-32700);
    expect(ErrorCodes.INVALID_REQUEST).toBe(-32600);
    expect(ErrorCodes.METHOD_NOT_FOUND).toBe(-32601);
    expect(ErrorCodes.INVALID_PARAMS).toBe(-32602);
    expect(ErrorCodes.INTERNAL_ERROR).toBe(-32603);
    expect(ErrorCodes.PROJECT_NOT_FOUND).toBe(1001);
    expect(ErrorCodes.INVALID_PROJECT).toBe(1002);
    expect(ErrorCodes.NOT_FOUND).toBe(1003);
    expect(ErrorCodes.CONFLICT).toBe(1004);
  });
});

describe("proto-v2 type-level helpers", () => {
  test("ParamsOf / ResultOf resolve to concrete shapes (compile-time)", () => {
    // These assignments only have to typecheck; the runtime body is trivial.
    const listParams: ParamsOf<"task.list"> = { cwd: "/repo" };
    const created: ResultOf<"task.create"> = {
      id: "ask-abc123",
      title: "t",
      description: "",
      status: "new",
      workflow: null,
      step: null,
      blocked: false,
      blocked_by: [],
      blocks: [],
      comment_count: 0,
      created_at: "2026-06-12T09:00:00Z",
      updated_at: "2026-06-12T09:00:00Z",
    };
    const method: RpcMethod = "session.transcript";
    expect(listParams.cwd).toBe("/repo");
    expect(created.status).toBe("new");
    expect(method).toBe("session.transcript");
  });
});
