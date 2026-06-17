/**
 * Interactive (taskless) chat sessions (plan §4.2/§4.3). A fake registered agent
 * whose interactive `onRun` waits on `ctx.signal` drives the lifecycle through
 * the engine: `createInteractiveSession` → running, `session.input` delivers a
 * followup, `sessionEnd` → `done` (no park, no task touched), `abort` →
 * `aborted`, crash recovery → `failed` without parking a task, and `ctx.transit`
 * rejects in interactive mode.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition } from "@autosk/sdk";

import { CapturingLogger, Engine, ExtensionRegistry, Store, type EngineProject } from "../src/index.ts";
import { gate, makeEngine, makeProject, oneStep, type TestProject } from "./engineHarness.ts";
import { tempDir, waitFor, waitForComplete } from "./helpers.ts";

describe("engine — interactive (taskless) sessions", () => {
  const cleanups: (() => void)[] = [];
  const engines: { stop(): void }[] = [];
  const stores: Store[] = [];
  afterEach(async () => {
    for (const e of engines.splice(0)) e.stop();
    for (const s of stores.splice(0)) await s.close();
    for (const c of cleanups.splice(0)) c();
  });

  /** Builds a project with one registered agent and an engine over it. */
  async function setup(
    agent: AgentDefinition,
    agentName = "chat",
  ): Promise<{ p: TestProject; engine: Engine }> {
    const p = await makeProject();
    cleanups.push(p.cleanup);
    p.registry.addAgent("test", { name: agentName, description: "Chat agent", agent });
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);
    return { p, engine };
  }

  /** An interactive agent that records each followup and blocks until the signal fires. */
  function chatAgent(opts: { started?: () => void; received?: string[]; mode?: { value?: string } } = {}): AgentDefinition {
    return {
      async onRun(ctx) {
        if (opts.mode) opts.mode.value = ctx.mode;
        opts.started?.();
        await new Promise<void>((resolve) => {
          if (ctx.signal.aborted) return resolve();
          ctx.signal.addEventListener("abort", () => resolve(), { once: true });
        });
      },
      async onFollowup(_ctx, message) {
        opts.received?.push(message);
      },
    };
  }

  async function waitForStatus(p: TestProject, id: string, status: string): Promise<void> {
    await waitFor(async () => (await p.store.sessions.getMeta(id))?.status === status, 5000);
  }

  test("createInteractiveSession dispatches a running, taskless session in interactive mode", async () => {
    const mode: { value?: string } = {};
    const { p, engine } = await setup(chatAgent({ mode }));
    const meta = await engine.createInteractiveSession(p.root, "chat");
    expect(meta.kind).toBe("interactive");
    expect(meta.task_id).toBe("");
    expect(meta.workflow).toBe("");
    expect(meta.step).toBe("");
    expect(meta.agent).toBe("chat");
    expect(meta.status).toBe("queued");

    await waitForStatus(p, meta.id, "running");
    expect(mode.value).toBe("interactive");
    // No task was created to host the session.
    expect(await p.store.listTaskViews()).toEqual([]);
  });

  test("unknown agent → invalid params", async () => {
    const { p, engine } = await setup(chatAgent());
    await expect(engine.createInteractiveSession(p.root, "no-such-agent")).rejects.toThrow(/unknown agent/);
  });

  test("an idle interactive session does NOT occupy a task-worker slot (no starvation)", async () => {
    // Regression (review BLOCKING): interactive sessions run OFF the bounded
    // task-worker pool. With a SINGLE worker, an idle chat — whose onRun blocks on
    // the abort signal for the whole session lifetime — must NOT prevent a
    // workflow task from being dispatched and run. Before the fix the chat held
    // the only `this.active` slot and the task stayed `queued` forever.
    const chatStarted = gate();
    const taskRan = gate();
    const taskAgent: AgentDefinition = {
      onRun: async (ctx) => {
        taskRan.open();
        await ctx.transit({ status: "done" });
      },
    };
    const p = await makeProject({ workflows: [oneStep("w", taskAgent)] });
    cleanups.push(p.cleanup);
    p.registry.addAgent("test", { name: "chat", agent: chatAgent({ started: () => chatStarted.open() }) });
    const { engine } = makeEngine({ workers: 1 });
    engines.push(engine);
    await engine.addProject(p.project);

    // Open a chat and wait until it is actually running, so it WOULD hold the
    // only worker slot if it were dispatched onto the pool.
    const chatMeta = await engine.createInteractiveSession(p.root, "chat");
    await chatStarted.wait;
    await waitForStatus(p, chatMeta.id, "running");

    // A task enrolled now must still run to completion despite the live chat.
    const task = await p.store.createTask({ title: "still scheduled" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await taskRan.wait; // would hang forever under the starvation bug
    await waitForComplete(p.store, task.id, "done");

    // The chat is unaffected; end it cleanly.
    await engine.sessionEnd(p.root, chatMeta.id);
    await waitForStatus(p, chatMeta.id, "done");
  });

  test("session.input delivers a followup; sessionEnd seals done without touching a task", async () => {
    const received: string[] = [];
    const { p, engine } = await setup(chatAgent({ received }));
    const meta = await engine.createInteractiveSession(p.root, "chat");
    await waitForStatus(p, meta.id, "running");

    const res = await engine.sessionInput(p.root, meta.id, { kind: "followup", message: "hello model" });
    expect(res.handled).toBe(true);
    expect(received).toEqual(["hello model"]);

    const end = await engine.sessionEnd(p.root, meta.id);
    expect(end.handled).toBe(true);
    await waitForStatus(p, meta.id, "done");
    const sealed = await p.store.sessions.getMeta(meta.id);
    expect(sealed?.status).toBe("done");
    expect(sealed?.error).toBeUndefined();
    // No task exists (nothing parked, nothing created).
    expect(await p.store.listTaskViews()).toEqual([]);
  });

  test("abort seals an interactive session as aborted (no park, no task)", async () => {
    const { p, engine } = await setup(chatAgent());
    const meta = await engine.createInteractiveSession(p.root, "chat");
    await waitForStatus(p, meta.id, "running");

    const res = await engine.sessionAbort(p.root, meta.id);
    expect(res.handled).toBe(true);
    await waitForStatus(p, meta.id, "aborted");
    expect(await p.store.listTaskViews()).toEqual([]);
  });

  test("sessionEnd rejects an unknown session id (not found)", async () => {
    const { p, engine } = await setup(chatAgent());
    // An unknown session id is `not found`.
    await expect(engine.sessionEnd(p.root, "00000000-0000-7000-8000-000000000000")).rejects.toThrow(
      /session not running/,
    );
  });

  test("sessionEnd refuses a live TASK session (wrong kind → conflict)", async () => {
    // A workflow (task) session cannot be gracefully ended via session.end — it is
    // only abortable. Exercise the `rt.kind !== 'interactive'` conflict branch of
    // engine.sessionEnd against a real, live task session.
    const started = gate();
    const blocking: AgentDefinition = {
      onRun: (ctx) =>
        new Promise<void>((resolve) => {
          started.open();
          ctx.signal.addEventListener("abort", () => resolve(), { once: true });
        }),
    };
    const p = await makeProject({ workflows: [oneStep("w", blocking)] });
    cleanups.push(p.cleanup);
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "task session" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1, 5000);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    await expect(engine.sessionEnd(p.root, sessionId)).rejects.toThrow(/not an interactive session/);

    // Release the gated agent so the suite tears down cleanly.
    await engine.sessionAbort(p.root, sessionId);
  });

  test("ctx.transit rejects in interactive mode (and a natural return seals done)", async () => {
    let captured: unknown;
    const agent: AgentDefinition = {
      async onRun(ctx) {
        try {
          await ctx.transit({ status: "done" });
        } catch (e) {
          captured = e;
        }
        // Returning normally from an interactive onRun seals the session `done`.
      },
    };
    const { p, engine } = await setup(agent);
    const meta = await engine.createInteractiveSession(p.root, "chat");
    await waitForStatus(p, meta.id, "done");
    expect(captured).toBeInstanceOf(Error);
    expect((captured as Error).message).toContain("transit is not available in an interactive session");
  });

  test("end() seals done even when the interactive onRun throws while winding down", async () => {
    // The `ending` flag makes a graceful End deterministic: if onRun throws when
    // the signal fires (a cleanup error), `onRunSettled` must STILL seal `done`,
    // not race it to `failed`. The onAbort that yields a microtask lets that
    // onRunSettled reaction run before end()'s own finalizeDone, so this case
    // exercises the `if (this.ending) finalizeDone()` branch in onRunSettled.
    const agent: AgentDefinition = {
      onRun: (ctx) =>
        new Promise<void>((_resolve, reject) => {
          const fail = () => reject(new Error("cleanup blew up"));
          if (ctx.signal.aborted) fail();
          else ctx.signal.addEventListener("abort", fail, { once: true });
        }),
      async onAbort() {
        await Promise.resolve(); // yield so onRunSettled wins the settle race
      },
    };
    const { p, engine } = await setup(agent);
    const meta = await engine.createInteractiveSession(p.root, "chat");
    await waitForStatus(p, meta.id, "running");

    await engine.sessionEnd(p.root, meta.id);
    await waitForStatus(p, meta.id, "done");
    const sealed = await p.store.sessions.getMeta(meta.id);
    expect(sealed?.status).toBe("done");
    expect(sealed?.error).toBeUndefined();
  });

  test("recovery: an interrupted interactive session → failed:daemon_restart, no task parked", async () => {
    const dir = tempDir();
    cleanups.push(() => dir.cleanup());
    const started = gate();
    const agent: AgentDefinition = {
      onRun: (ctx) =>
        new Promise<void>((resolve) => {
          started.open();
          ctx.signal.addEventListener("abort", () => resolve(), { once: true });
        }),
    };

    // -- engine #1: dispatch an interactive session, then "crash" (stop) -------
    const store1 = new Store(dir.path, { watch: false });
    stores.push(store1);
    await store1.open();
    const registry1 = new ExtensionRegistry();
    registry1.addAgent("test", { name: "chat", agent });
    const project1: EngineProject = { root: store1.root, store: store1, registry: registry1 };
    const engine1 = new Engine({ logger: new CapturingLogger() });
    await engine1.addProject(project1);
    const meta = await engine1.createInteractiveSession(store1.root, "chat");
    await started.wait;
    await waitFor(async () => (await store1.sessions.getMeta(meta.id))?.status === "running", 5000);
    engine1.stop(); // simulate the daemon dying — no terminal state is written

    // -- engine #2: fresh process over the same files runs recovery ----------
    const store2 = new Store(dir.path, { watch: false });
    stores.push(store2);
    await store2.open();
    const registry2 = new ExtensionRegistry();
    registry2.addAgent("test", { name: "chat", agent });
    const engine2 = new Engine({ logger: new CapturingLogger() });
    await engine2.addProject({ root: store2.root, store: store2, registry: registry2 });
    engine2.stop();

    const recovered = await store2.sessions.getMeta(meta.id);
    expect(recovered?.status).toBe("failed");
    expect(recovered?.error).toBe("daemon_restart");
    // There is no task behind an interactive session, so nothing was parked.
    expect(await store2.listTaskViews()).toEqual([]);
  });
});
