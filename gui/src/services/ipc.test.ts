// Unit tests for services/ipc.ts: the daemon error funnel (`normalizeError`)
// and the proto-v1→v2 method rewire. The rewire is the PR's central change, yet
// `daemonRequest(method: string, …)` takes an untyped string, so the method-name
// literals in ipc.ts have NO compile-time tie to the SDK RpcMethodMap. These
// tests lock each survivor shim to its namespaced v2 method name AND the
// `{cwd}`-only selector (no v1 `source` / `db_path`), so a typo or a missed
// namespacing fails here instead of silently at runtime. Pure; no daemon — the
// Tauri `invoke` bridge is mocked.

import { describe, it, expect, beforeEach, vi } from "vitest";

// vi.mock is hoisted above the imports; use vi.hoisted so the factory can close
// over the shared spy.
const { invokeMock } = vi.hoisted(() => ({ invokeMock: vi.fn() }));
vi.mock("@tauri-apps/api/core", () => ({ invoke: invokeMock }));

import * as ipc from "./ipc";
import { DaemonError, normalizeError } from "./ipc";

describe("normalizeError", () => {
  it("passes through an existing Error untouched", () => {
    const e = new Error("boom");
    expect(normalizeError(e)).toBe(e);
  });

  it("parses a JSON-encoded ErrorObject string into a DaemonError", () => {
    const out = normalizeError(JSON.stringify({ code: 1003, message: "not found", details: { id: "x" } }));
    expect(out).toBeInstanceOf(DaemonError);
    const de = out as DaemonError;
    expect(de.code).toBe(1003);
    expect(de.message).toBe("not found");
    expect(de.details).toEqual({ id: "x" });
  });

  it("maps a structured object with code+message to a DaemonError", () => {
    const out = normalizeError({ code: 1004, message: "conflict" });
    expect(out).toBeInstanceOf(DaemonError);
    expect((out as DaemonError).code).toBe(1004);
  });

  it("falls back to a plain Error for an object with only a message", () => {
    const out = normalizeError({ message: "plain" });
    expect(out).toBeInstanceOf(Error);
    expect(out).not.toBeInstanceOf(DaemonError);
    expect(out.message).toBe("plain");
  });

  it("wraps a non-JSON string as a plain Error", () => {
    const out = normalizeError("just a string");
    expect(out).toBeInstanceOf(Error);
    expect(out).not.toBeInstanceOf(DaemonError);
    expect(out.message).toBe("just a string");
  });

  it("stringifies anything else", () => {
    expect(normalizeError(42).message).toBe("42");
    expect(normalizeError(null).message).toBe("null");
  });
});

// ---- proto-v2 method rewire (acceptance #6) -------------------------------

const cwd = "/repo";

// Each case calls a survivor shim and pins the JSON-RPC `method` + `params` that
// `daemonRequest` forwards through the Rust `daemon_request` command. The
// selector is `{cwd}` only — no v1 `source` / `db_path`.
const cases: Array<{ name: string; run: () => unknown; method: string; params: Record<string, unknown> }> = [
  // meta.* (un-namespaced v1 version/healthz → namespaced)
  { name: "version", run: () => ipc.version(), method: "meta.version", params: {} },
  { name: "healthz", run: () => ipc.healthz(), method: "meta.healthz", params: {} },
  // project.*
  { name: "projectList", run: () => ipc.projectList(), method: "project.list", params: {} },
  { name: "projectAdd", run: () => ipc.projectAdd(cwd), method: "project.add", params: { cwd } },
  { name: "projectAdd(name)", run: () => ipc.projectAdd(cwd, "proj"), method: "project.add", params: { cwd, name: "proj" } },
  { name: "projectRemove", run: () => ipc.projectRemove(cwd), method: "project.remove", params: { cwd } },
  { name: "projectInit", run: () => ipc.projectInit(cwd), method: "project.init", params: { cwd } },
  { name: "projectDiagnostics", run: () => ipc.projectDiagnostics(cwd), method: "project.diagnostics", params: { cwd } },
  // task.* reads
  { name: "taskList", run: () => ipc.taskList(cwd, {}), method: "task.list", params: { cwd, filter: {} } },
  { name: "taskList(filter)", run: () => ipc.taskList(cwd, { status: "work" }), method: "task.list", params: { cwd, filter: { status: "work" } } },
  { name: "taskGet", run: () => ipc.taskGet(cwd, "ask-1"), method: "task.get", params: { cwd, id: "ask-1" } },
  // task.* writes
  { name: "taskCreate", run: () => ipc.taskCreate(cwd, { title: "T" }), method: "task.create", params: { cwd, title: "T" } },
  { name: "taskUpdate", run: () => ipc.taskUpdate(cwd, "ask-1", { title: "X" }), method: "task.update", params: { cwd, id: "ask-1", title: "X" } },
  { name: "taskDone", run: () => ipc.taskDone(cwd, "ask-1"), method: "task.done", params: { cwd, id: "ask-1" } },
  { name: "taskDone(force)", run: () => ipc.taskDone(cwd, "ask-1", true), method: "task.done", params: { cwd, id: "ask-1", force: true } },
  { name: "taskCancel", run: () => ipc.taskCancel(cwd, "ask-1"), method: "task.cancel", params: { cwd, id: "ask-1" } },
  { name: "taskCancel(force)", run: () => ipc.taskCancel(cwd, "ask-1", true), method: "task.cancel", params: { cwd, id: "ask-1", force: true } },
  { name: "taskReopen", run: () => ipc.taskReopen(cwd, "ask-1"), method: "task.reopen", params: { cwd, id: "ask-1" } },
  { name: "taskEnroll(workflow)", run: () => ipc.taskEnroll(cwd, "ask-1", { workflow: "wf" }), method: "task.enroll", params: { cwd, id: "ask-1", workflow: "wf" } },
  { name: "taskEnroll(workflow, step)", run: () => ipc.taskEnroll(cwd, "ask-1", { workflow: "wf", step: "dev" }), method: "task.enroll", params: { cwd, id: "ask-1", workflow: "wf", step: "dev" } },
  { name: "taskResume", run: () => ipc.taskResume(cwd, "ask-1"), method: "task.resume", params: { cwd, id: "ask-1" } },
  { name: "taskResume(to)", run: () => ipc.taskResume(cwd, "ask-1", { step: "dev" }), method: "task.resume", params: { cwd, id: "ask-1", to: { step: "dev" } } },
  { name: "taskBlock", run: () => ipc.taskBlock(cwd, "ask-1", "ask-2"), method: "task.block", params: { cwd, id: "ask-1", blocked_by: "ask-2" } },
  { name: "taskUnblock", run: () => ipc.taskUnblock(cwd, "ask-1", "ask-2"), method: "task.unblock", params: { cwd, id: "ask-1", blocked_by: "ask-2" } },
  // task.subscribe is now front-end-issued and REQUIRES {cwd}.
  { name: "taskSubscribe", run: () => ipc.taskSubscribe(cwd), method: "task.subscribe", params: { cwd } },
  { name: "taskUnsubscribe", run: () => ipc.taskUnsubscribe(cwd), method: "task.unsubscribe", params: { cwd } },
  // task.comment.* (the v1 comment.list/add were renamed under task.comment.*)
  { name: "commentList", run: () => ipc.commentList(cwd, "ask-1"), method: "task.comment.list", params: { cwd, task_id: "ask-1" } },
  { name: "commentAdd", run: () => ipc.commentAdd(cwd, "ask-1", "hi"), method: "task.comment.add", params: { cwd, task_id: "ask-1", text: "hi" } },
  { name: "commentEdit", run: () => ipc.commentEdit(cwd, "ask-1", "c1", "hi"), method: "task.comment.edit", params: { cwd, task_id: "ask-1", comment_id: "c1", text: "hi" } },
  { name: "commentDelete", run: () => ipc.commentDelete(cwd, "ask-1", "c1"), method: "task.comment.delete", params: { cwd, task_id: "ask-1", comment_id: "c1" } },
  // registry.* (workflows + agents are code; read-only)
  { name: "workflowList", run: () => ipc.workflowList(cwd), method: "registry.workflow.list", params: { cwd } },
  { name: "workflowGet", run: () => ipc.workflowGet(cwd, "wf"), method: "registry.workflow.get", params: { cwd, name: "wf" } },
  { name: "agentList", run: () => ipc.agentList(cwd), method: "registry.agent.list", params: { cwd } },
  // session.* run lifecycle (the v1 run methods were renamed under this namespace)
  { name: "sessionList", run: () => ipc.sessionList(cwd), method: "session.list", params: { cwd } },
  { name: "sessionList(task)", run: () => ipc.sessionList(cwd, "ask-1"), method: "session.list", params: { cwd, task_id: "ask-1" } },
  { name: "sessionGet", run: () => ipc.sessionGet(cwd, "s1"), method: "session.get", params: { cwd, id: "s1" } },
  { name: "sessionTranscript", run: () => ipc.sessionTranscript(cwd, "s1", 1, 50), method: "session.transcript", params: { cwd, id: "s1", from_line: 1, limit: 50 } },
  { name: "sessionTranscript(no-args)", run: () => ipc.sessionTranscript(cwd, "s1"), method: "session.transcript", params: { cwd, id: "s1" } },
  { name: "sessionSubscribe", run: () => ipc.sessionSubscribe(cwd, "s1", 1), method: "session.subscribe", params: { cwd, id: "s1", from_line: 1 } },
  { name: "sessionUnsubscribe", run: () => ipc.sessionUnsubscribe(cwd, "s1"), method: "session.unsubscribe", params: { cwd, id: "s1" } },
  { name: "sessionSubscribeProject", run: () => ipc.sessionSubscribeProject(cwd), method: "session.subscribeProject", params: { cwd } },
  { name: "sessionUnsubscribeProject", run: () => ipc.sessionUnsubscribeProject(cwd), method: "session.unsubscribeProject", params: { cwd } },
  { name: "sessionInput(steer)", run: () => ipc.sessionInput(cwd, "s1", "go", "steer"), method: "session.input", params: { cwd, id: "s1", message: "go", kind: "steer" } },
  { name: "sessionInput(followup)", run: () => ipc.sessionInput(cwd, "s1", "go", "followup"), method: "session.input", params: { cwd, id: "s1", message: "go", kind: "followup" } },
  { name: "sessionAbort", run: () => ipc.sessionAbort(cwd, "s1"), method: "session.abort", params: { cwd, id: "s1" } },
  { name: "sessionCreate", run: () => ipc.sessionCreate(cwd, "pi"), method: "session.create", params: { cwd, agent: "pi" } },
  { name: "sessionEnd", run: () => ipc.sessionEnd(cwd, "s1"), method: "session.end", params: { cwd, id: "s1" } },
];

describe("ipc v2 method rewire", () => {
  beforeEach(() => {
    invokeMock.mockReset();
    invokeMock.mockResolvedValue({});
  });

  for (const c of cases) {
    it(`${c.name} → ${c.method} with the {cwd}-only selector`, async () => {
      await c.run();
      // Every daemon shim funnels through the generic `daemon_request` command.
      expect(invokeMock).toHaveBeenCalledTimes(1);
      expect(invokeMock).toHaveBeenCalledWith("daemon_request", { method: c.method, params: c.params });
      // The v2 selector shrink: no v1 `source` discriminator, no `db_path`.
      const [, payload] = invokeMock.mock.calls[0] as [string, { params: Record<string, unknown> }];
      expect(payload.params).not.toHaveProperty("source");
      expect(payload.params).not.toHaveProperty("db_path");
    });
  }
});

// ---- extension management shims -------------------------------------------
// extension.install legitimately carries a `source` (the package spec), so it
// is exempt from the generic loop's no-`source` assertion; extension_search is
// a LOCAL Tauri command (not a daemon_request), so it is pinned separately.
describe("ipc extension shims", () => {
  beforeEach(() => {
    invokeMock.mockReset();
    invokeMock.mockResolvedValue({});
  });

  it("extensionList → extension.list with the {cwd} selector", async () => {
    await ipc.extensionList(cwd);
    expect(invokeMock).toHaveBeenCalledWith("daemon_request", { method: "extension.list", params: { cwd } });
  });

  it("extensionInstall(global) → extension.install {cwd, source, local:false}", async () => {
    await ipc.extensionInstall(cwd, "npm:@autosk/feature-dev", false);
    expect(invokeMock).toHaveBeenCalledWith("daemon_request", {
      method: "extension.install",
      params: { cwd, source: "npm:@autosk/feature-dev", local: false },
    });
  });

  it("extensionInstall(project) → extension.install {cwd, source, local:true}", async () => {
    await ipc.extensionInstall(cwd, "npm:left-pad", true);
    expect(invokeMock).toHaveBeenCalledWith("daemon_request", {
      method: "extension.install",
      params: { cwd, source: "npm:left-pad", local: true },
    });
  });

  it("extensionSearch → the local `extension_search` Tauri command (not daemon_request)", async () => {
    invokeMock.mockResolvedValue([]);
    await ipc.extensionSearch();
    expect(invokeMock).toHaveBeenCalledTimes(1);
    expect(invokeMock).toHaveBeenCalledWith("extension_search");
  });
});
