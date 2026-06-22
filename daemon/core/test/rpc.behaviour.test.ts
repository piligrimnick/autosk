/**
 * Happy-path behaviour over the live daemon (plan §4): the task lifecycle and
 * the P5-implemented done/cancel/reopen semantics, comments, the registry
 * views, session reads, health, and the project flows.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { ErrorCodes, statusStep } from "@autosk/sdk";
import type {
  AgentDefinition,
  Comment,
  Health,
  ProjectInfo,
  SessionMeta,
  TaskView,
  WorkflowInfo,
} from "@autosk/sdk";

import { waitFor } from "./helpers.ts";
import { startTestDaemon, type RpcClient, type TestDaemon } from "./rpcHarness.ts";

describe("task lifecycle + done/cancel/reopen", () => {
  let td: TestDaemon;
  let cwd: string;
  let client: RpcClient;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("life");
    client = await td.client();
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("create / get / list / update / block / unblock", async () => {
    const a = await client.call<TaskView>("task.create", { cwd, title: "A", description: "first" });
    expect(a.status).toBe("new");
    expect(a.title).toBe("A");

    const b = await client.call<TaskView>("task.create", { cwd, title: "B" });

    const got = await client.call<TaskView>("task.get", { cwd, id: a.id });
    expect(got.id).toBe(a.id);

    const list = await client.call<TaskView[]>("task.list", { cwd });
    expect(list.map((t) => t.id).sort()).toEqual([a.id, b.id].sort());

    const updated = await client.call<TaskView>("task.update", { cwd, id: a.id, title: "A2" });
    expect(updated.title).toBe("A2");

    const blocked = await client.call<TaskView>("task.block", { cwd, id: a.id, blocked_by: b.id });
    expect(blocked.blocked).toBe(true);
    expect(blocked.blocked_by.map((r) => r.id)).toEqual([b.id]);

    const unblocked = await client.call<TaskView>("task.unblock", { cwd, id: a.id, blocked_by: b.id });
    expect(unblocked.blocked).toBe(false);
  });

  test("task.metadata.set / unset: dot-path merge, prune, NOT_FOUND, INVALID_PARAMS", async () => {
    const t = await client.call<TaskView>("task.create", { cwd, title: "meta" });
    expect(t.metadata).toEqual({});

    // set creates nested objects + merges sibling keys.
    const set1 = await client.call<TaskView>("task.metadata.set", {
      cwd,
      id: t.id,
      patch: { "step_visits.dev": 3, note: "keep" },
    });
    expect(set1.metadata).toEqual({ step_visits: { dev: 3 }, note: "keep" });
    expect(set1.updated_at >= t.updated_at).toBe(true);

    const set2 = await client.call<TaskView>("task.metadata.set", {
      cwd,
      id: t.id,
      patch: { "step_visits.review": 1 },
    });
    expect(set2.metadata).toEqual({ step_visits: { dev: 3, review: 1 }, note: "keep" });

    // unset removes a leaf and prunes nothing while a sibling remains.
    const unset1 = await client.call<TaskView>("task.metadata.unset", {
      cwd,
      id: t.id,
      keys: ["step_visits.dev"],
    });
    expect(unset1.metadata).toEqual({ step_visits: { review: 1 }, note: "keep" });

    // unsetting the last child prunes the emptied parent.
    const unset2 = await client.call<TaskView>("task.metadata.unset", {
      cwd,
      id: t.id,
      keys: ["step_visits.review"],
    });
    expect(unset2.metadata).toEqual({ note: "keep" });

    // Unknown task id → NOT_FOUND for both.
    await expect(
      client.call("task.metadata.set", { cwd, id: "ask-nope01", patch: { a: 1 } }),
    ).rejects.toMatchObject({ code: ErrorCodes.NOT_FOUND });
    await expect(
      client.call("task.metadata.unset", { cwd, id: "ask-nope01", keys: ["a"] }),
    ).rejects.toMatchObject({ code: ErrorCodes.NOT_FOUND });

    // Malformed params → INVALID_PARAMS.
    await expect(
      client.call("task.metadata.set", { cwd, id: t.id, patch: [1, 2] }),
    ).rejects.toMatchObject({ code: ErrorCodes.INVALID_PARAMS });
    await expect(
      client.call("task.metadata.unset", { cwd, id: t.id, keys: "dev" }),
    ).rejects.toMatchObject({ code: ErrorCodes.INVALID_PARAMS });
  });

  test("done then reopen of a never-enrolled task → new (unenrolled)", async () => {
    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    const done = await client.call<TaskView>("task.done", { cwd, id: t.id });
    expect(done.status).toBe("done");
    // Idempotent.
    expect((await client.call<TaskView>("task.done", { cwd, id: t.id })).status).toBe("done");

    const reopened = await client.call<TaskView>("task.reopen", { cwd, id: t.id });
    expect(reopened.status).toBe("new");
    expect(reopened.workflow).toBeNull();
  });

  test("cancel then reopen of an enrolled task → human (resumable, keeps workflow/step)", async () => {
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", { name: "wf", firstStep: "review", steps: { review: statusStep("human") } });

    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    await client.call("task.enroll", { cwd, id: t.id, workflow: "wf" });

    const cancelled = await client.call<TaskView>("task.cancel", { cwd, id: t.id });
    expect(cancelled.status).toBe("cancel");
    expect(cancelled.workflow).toBe("wf");

    const reopened = await client.call<TaskView>("task.reopen", { cwd, id: t.id });
    expect(reopened.status).toBe("human");
    expect(reopened.workflow).toBe("wf");
    expect(reopened.step).toBe("review");
  });

  test("reopen of a non-terminal task → CONFLICT", async () => {
    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    const frame = await client.callRaw("task.reopen", { cwd, id: t.id });
    expect(frame.error).toBeDefined();
  });

  test("done is rejected while a session is live, then succeeds after abort", async () => {
    const handle = await td.handle(cwd);
    const agent: AgentDefinition = { onRun: () => new Promise<void>(() => {}) };
    handle.extensions.addWorkflow("test", { name: "hang", firstStep: "do", steps: { do: agent } });

    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    await client.call("task.enroll", { cwd, id: t.id, workflow: "hang" });

    let sessionId = "";
    await waitFor(async () => {
      const sessions = await client.call<SessionMeta[]>("session.list", { cwd });
      if (sessions.length === 0) return false;
      sessionId = sessions[0]!.id;
      return sessions[0]!.status === "running";
    });

    const blocked = await client.callRaw("task.done", { cwd, id: t.id });
    expect(blocked.error).toBeDefined(); // CONFLICT: a session is live

    const aborted = await client.call<{ ok: boolean }>("session.abort", { cwd, id: sessionId });
    expect(aborted.ok).toBe(true);
    // Abort parks the task to human; with no live session, done now succeeds.
    await waitFor(async () => (await client.call<SessionMeta>("session.get", { cwd, id: sessionId })).status === "aborted");
    const done = await client.call<TaskView>("task.done", { cwd, id: t.id });
    expect(done.status).toBe("done");
  });

  test("manual done/cancel is a RAW status flip — no isolation teardown, no dirty-gate", async () => {
    // Isolation is agent-owned and torn down by a cleanup workflow step, so a
    // terminal verb just flips the status (keeping workflow/step). There is no
    // `force` knob and no ENVIRONMENT_DIRTY refusal.
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", {
      name: "parker",
      firstStep: "park",
      steps: { park: statusStep("human") },
    });

    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    await client.call("task.enroll", { cwd, id: t.id, workflow: "parker" });
    await waitFor(async () => (await client.call<TaskView>("task.get", { cwd, id: t.id })).status === "human");

    const done = await client.call<TaskView>("task.done", { cwd, id: t.id });
    expect(done.status).toBe("done");
    // workflow/step are preserved across the flip.
    expect(done.workflow).toBe("parker");
    expect(done.step).toBe("park");
  });
});

describe("session.input / abort wire translation (plan §3.4)", () => {
  let td: TestDaemon;
  let cwd: string;
  let client: RpcClient;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("steer");
    client = await td.client();
  });
  afterEach(async () => {
    await td.cleanup();
  });

  /** Drives a task to a live `running` session under `workflow` and returns the session id. */
  async function liveSession(workflow: string): Promise<string> {
    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    await client.call("task.enroll", { cwd, id: t.id, workflow });
    let sessionId = "";
    await waitFor(async () => {
      const sessions = await client.call<SessionMeta[]>("session.list", { cwd });
      if (sessions.length === 0) return false;
      sessionId = sessions[0]!.id;
      return sessions[0]!.status === "running";
    });
    return sessionId;
  }

  test("session.input steer on a LIVE agent with no onSteer → CONFLICT (unsupported_by_agent)", async () => {
    const handle = await td.handle(cwd);
    // A hanging agent with NO onSteer hook: the run is live, but it cannot accept
    // a steer — the engine returns { handled:false }, which the RPC layer must map
    // to CONFLICT/unsupported_by_agent (NOT a no-op success).
    handle.extensions.addWorkflow("test", {
      name: "plain",
      firstStep: "do",
      steps: { do: { onRun: () => new Promise<void>(() => {}) } },
    });

    const sessionId = await liveSession("plain");
    const frame = await client.callRaw("session.input", { cwd, id: sessionId, message: "go left", kind: "steer" });
    expect(frame.error?.code).toBe(ErrorCodes.CONFLICT);
    expect(frame.error?.message).toMatch(/unsupported_by_agent/);
  });

  test("session.abort on an already-settled (still-attached) session → { ok: false }, not an error", async () => {
    const handle = await td.handle(cwd);
    // A hanging agent ignores the abort signal, so its runtime stays attached to
    // the engine (onRun never resolves) even after the first abort seals the meta
    // `aborted`. A SECOND abort then hits a settled-but-live runtime — the engine
    // returns { handled:false }, which for abort means only "nothing left to
    // abort" and must surface as ok:false, NOT a CONFLICT/unsupported error.
    handle.extensions.addWorkflow("test", {
      name: "hang",
      firstStep: "do",
      steps: { do: { onRun: () => new Promise<void>(() => {}) } },
    });

    const sessionId = await liveSession("hang");
    const first = await client.call<{ ok: boolean }>("session.abort", { cwd, id: sessionId });
    expect(first.ok).toBe(true);

    const second = await client.call<{ ok: boolean }>("session.abort", { cwd, id: sessionId });
    expect(second.ok).toBe(false);
  });
});

describe("comments", () => {
  let td: TestDaemon;
  let cwd: string;
  let client: RpcClient;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("comments");
    client = await td.client();
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("add / list / edit / delete and comment_count", async () => {
    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    const c1 = await client.call<Comment>("task.comment.add", { cwd, task_id: t.id, text: "hello", author: "me" });
    expect(c1.text).toBe("hello");
    expect(c1.author).toBe("me");

    await client.call("task.comment.add", { cwd, task_id: t.id, text: "world" });
    let list = await client.call<Comment[]>("task.comment.list", { cwd, task_id: t.id });
    expect(list.map((c) => c.text)).toEqual(["hello", "world"]);

    expect((await client.call<TaskView>("task.get", { cwd, id: t.id })).comment_count).toBe(2);

    const edited = await client.call<Comment>("task.comment.edit", {
      cwd,
      task_id: t.id,
      comment_id: c1.id,
      text: "edited",
    });
    expect(edited.text).toBe("edited");

    const del = await client.call<{ ok: boolean }>("task.comment.delete", { cwd, task_id: t.id, comment_id: c1.id });
    expect(del.ok).toBe(true);
    list = await client.call<Comment[]>("task.comment.list", { cwd, task_id: t.id });
    expect(list.map((c) => c.text)).toEqual(["world"]);

    // Editing a missing comment → NOT_FOUND.
    const missing = await client.callRaw("task.comment.edit", { cwd, task_id: t.id, comment_id: "cm-zzzzzz", text: "x" });
    expect(missing.error).toBeDefined();
  });
});

describe("registry + session reads + health + projects", () => {
  let td: TestDaemon;
  let cwd: string;
  let client: RpcClient;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("reg");
    client = await td.client();
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("registry.workflow.list/get reflect the registered code (agents inline)", async () => {
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", {
      name: "flow",
      firstStep: "dev",
      steps: { dev: { onRun: async (c) => void (await c.transit({ status: "done" })) }, accept: statusStep("human") },
    });

    const workflows = await client.call<WorkflowInfo[]>("registry.workflow.list", { cwd });
    expect(workflows.map((w) => w.name)).toContain("flow");

    const flow = await client.call<WorkflowInfo>("registry.workflow.get", { cwd, name: "flow" });
    expect(flow.first_step).toBe("dev");
    // The agent step renders status null; the accept step is a human statusStep.
    expect(flow.steps.find((s) => s.name === "dev")?.status).toBeNull();
    expect(flow.steps.find((s) => s.name === "accept")?.status).toBe("human");

    // Unknown workflow → NOT_FOUND.
    expect((await client.callRaw("registry.workflow.get", { cwd, name: "ghost" })).error).toBeDefined();
  });

  test("session.list/get/transcript over a finished session", async () => {
    const handle = await td.handle(cwd);
    handle.extensions.addWorkflow("test", {
      name: "quick-wf",
      firstStep: "quick",
      steps: {
        quick: {
          onRun: async (c) => {
            c.log.custom("note", { ok: true });
            await c.transit({ status: "done" });
          },
        },
      },
    });

    const t = await client.call<TaskView>("task.create", { cwd, title: "T" });
    await client.call("task.enroll", { cwd, id: t.id, workflow: "quick-wf" });
    await waitFor(async () => {
      const list = await client.call<SessionMeta[]>("session.list", { cwd });
      return list.length === 1 && list[0]!.status === "done";
    });

    const sessions = await client.call<SessionMeta[]>("session.list", { cwd, task_id: t.id });
    expect(sessions).toHaveLength(1);
    const sid = sessions[0]!.id;

    const meta = await client.call<SessionMeta>("session.get", { cwd, id: sid });
    expect(meta.status).toBe("done");
    // SessionMeta.agent is the step key (inline step agents).
    expect(meta.agent).toBe("quick");

    const { entries, next_line } = await client.call<{ entries: { type: string }[]; next_line: number }>(
      "session.transcript",
      { cwd, id: sid },
    );
    expect(entries[0]!.type).toBe("session"); // header is line 1
    expect(next_line).toBe(entries.length + 1);
  });

  test("meta.healthz reports workers + opened projects", async () => {
    await client.call("task.list", { cwd }); // open the project
    const health = await client.call<Health>("meta.healthz", null);
    expect(health.ok).toBe(true);
    expect(health.workers).toBeGreaterThan(0);
    expect(health.projects.some((p) => p.root.endsWith("reg"))).toBe(true);
  });

  test("project.init registers, project.add/remove/list/diagnostics", async () => {
    const fresh = `${td.dir}/fresh-init`;
    const inited = await client.call<ProjectInfo>("project.init", { cwd: fresh });
    expect(inited.name).toBe("fresh-init");
    let projects = await client.call<ProjectInfo[]>("project.list", null);
    expect(projects.some((p) => p.root === inited.root)).toBe(true);

    const added = await client.call<ProjectInfo>("project.add", { cwd });
    projects = await client.call<ProjectInfo[]>("project.list", null);
    expect(projects.some((p) => p.root === added.root)).toBe(true);

    const diag = await client.call<{ root: string; extensions: unknown[] }>("project.diagnostics", { cwd });
    expect(Array.isArray(diag.extensions)).toBe(true);

    const removed = await client.call<{ ok: boolean }>("project.remove", { cwd });
    expect(removed.ok).toBe(true);
    projects = await client.call<ProjectInfo[]>("project.list", null);
    expect(projects.some((p) => p.root === added.root)).toBe(false);
  });
});
