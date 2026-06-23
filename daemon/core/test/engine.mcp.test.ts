/**
 * `ctx.newMCPServer()` wiring + the `ctx.cwd === projectRoot` contract (plan §7).
 *
 * An agent step mints a per-session MCP server, records its url/port/token, and
 * asserts `ctx.cwd` is the project root (isolation no longer rewrites it). The
 * engine backstop must close the server on EVERY settle path — a committed
 * transit, an agent that throws without transiting (failed → parked), and an
 * abort — even though the agent never called `close()` (a POST to the port is
 * then refused). It also covers the one genuinely-new in-daemon MCP path: a
 * `task` create with `workflow` enrolls the new task via the engine.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition } from "@autosk/sdk";

import { gate, makeEngine, makeProject, oneStep, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

/** Asserts a POST to `url` (with `token`) is refused — the server has been closed. */
async function expectRefused(url: string, token: string): Promise<void> {
  let refused = false;
  try {
    await fetch(url, {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
      body: "{}",
      signal: AbortSignal.timeout(500),
    });
  } catch {
    refused = true;
  }
  expect(refused).toBe(true);
}

describe("ctx.newMCPServer + ctx.cwd", () => {
  const cleanups: (() => void)[] = [];
  const engines: { stop(): void }[] = [];
  afterEach(() => {
    for (const e of engines.splice(0)) e.stop();
    for (const c of cleanups.splice(0)) c();
  });
  function track(p: TestProject): TestProject {
    cleanups.push(p.cleanup);
    return p;
  }

  test("mints a server bound to a loopback port; ctx.cwd is the project root; the engine closes it on settle", async () => {
    let captured: { url: string; port: number; token: string } | null = null;
    let cwd = "";
    let projectRoot = "";
    const agent: AgentDefinition = {
      onRun: async (ctx) => {
        cwd = ctx.cwd;
        projectRoot = ctx.projectRoot;
        const mcp = await ctx.newMCPServer();
        captured = { url: mcp.url, port: mcp.port, token: mcp.token };
        // Do NOT call mcp.close() — the engine backstop must close it on settle.
        await ctx.transit({ status: "done" });
      },
    };

    const p = track(await makeProject({ workflows: [oneStep("mcp-wf", agent)] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "mcp" });
    await engine.enroll(p.root, task.id, { workflow: "mcp-wf" });
    await waitForComplete(p.store, task.id, "done", 10000);

    expect(captured).not.toBeNull();
    expect(captured!.url).toBe(`http://127.0.0.1:${captured!.port}`);
    expect(captured!.token.length).toBeGreaterThan(0);
    // ctx.cwd is always the project root now (isolation is agent-owned).
    expect(cwd).toBe(p.root);
    expect(projectRoot).toBe(p.root);

    // The engine backstop closed the server: a POST to its port is refused.
    await expectRefused(captured!.url, captured!.token);
  }, 15000);

  test("the engine backstop closes the server when the agent throws without transiting (failed → parked)", async () => {
    let captured: { url: string; token: string } | null = null;
    const agent: AgentDefinition = {
      onRun: async (ctx) => {
        const mcp = await ctx.newMCPServer();
        captured = { url: mcp.url, token: mcp.token };
        // Throw WITHOUT calling mcp.close() and WITHOUT transiting: the session
        // finalises `failed` and the task parks `human`. The backstop must still
        // close the server (no leaked port across the failure).
        throw new Error("boom — never transited");
      },
    };

    const p = track(await makeProject({ workflows: [oneStep("throw-wf", agent)] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "throw" });
    await engine.enroll(p.root, task.id, { workflow: "throw-wf" });
    await waitForComplete(p.store, task.id, "human", 10000);

    expect(captured).not.toBeNull();
    await expectRefused(captured!.url, captured!.token);
  }, 15000);

  test("the engine backstop closes the server on abort", async () => {
    let captured: { url: string; token: string } | null = null;
    const started = gate();
    const agent: AgentDefinition = {
      // Mint the server, publish it, then block until the abort signal fires; the
      // agent never calls close() — the engine's finalizeAborted must close it.
      onRun: (ctx) =>
        new Promise<void>((resolve) => {
          void (async () => {
            const mcp = await ctx.newMCPServer();
            captured = { url: mcp.url, token: mcp.token };
            started.open();
            ctx.signal.addEventListener("abort", () => resolve(), { once: true });
          })();
        }),
    };

    const p = track(await makeProject({ workflows: [oneStep("abort-wf", agent)] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "abort" });
    await engine.enroll(p.root, task.id, { workflow: "abort-wf" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    const res = await engine.sessionAbort(p.root, sessionId);
    expect(res.handled).toBe(true);
    await waitForComplete(p.store, task.id, "human", 10000);

    expect(captured).not.toBeNull();
    await expectRefused(captured!.url, captured!.token);
  }, 15000);

  test("an MCP `task` create with `workflow` enrolls the new task via the engine (returns the enrolled view)", async () => {
    let enrolled: { id: string; workflow: string; step: string; status: string } | null = null;
    const agent: AgentDefinition = {
      onRun: async (ctx) => {
        const mcp = await ctx.newMCPServer();
        // The one genuinely-new in-daemon path: directStoreBackend.enroll →
        // SessionHost.enrollTask → engine.enroll (validates + runs onTransit for
        // the entry edge + sets the position), returning the enrolled view.
        const res = await fetch(mcp.url, {
          method: "POST",
          headers: { authorization: `Bearer ${mcp.token}`, "content-type": "application/json" },
          body: JSON.stringify({
            jsonrpc: "2.0",
            id: 1,
            method: "tools/call",
            params: { name: "task", arguments: { action: "create", args: { title: "child", workflow: "child-wf" } } },
          }),
        });
        const body = (await res.json()) as {
          result: { structuredContent: { task: { id: string; workflow: string; step: string; status: string } } };
        };
        enrolled = body.result.structuredContent.task;
        await ctx.transit({ status: "done" });
      },
    };

    const p = track(
      await makeProject({
        workflows: [oneStep("driver-wf", agent), oneStep("child-wf", transitAgent({ status: "done" }))],
      }),
    );
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "driver" });
    await engine.enroll(p.root, task.id, { workflow: "driver-wf" });
    await waitForComplete(p.store, task.id, "done", 10000);

    // The returned view is the ENROLLED task: into child-wf at its first step,
    // status `work` (a bare store write would leave workflow="" / status="new").
    expect(enrolled).not.toBeNull();
    expect(enrolled!.workflow).toBe("child-wf");
    expect(enrolled!.step).toBe("do");
    expect(enrolled!.status).toBe("work");
  }, 15000);
});
