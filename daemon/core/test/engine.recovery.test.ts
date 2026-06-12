/**
 * Crash recovery (plan §3.7(4)): a session interrupted by a daemon restart is
 * marked `failed: daemon_restart` and its task is parked to `human` on the next
 * engine start. A `queued` session orphaned the same way is reaped too
 * (deviation #2).
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";

import { Engine, ExtensionRegistry, Store, CapturingLogger, type EngineProject } from "../src/index.ts";
import { tempDir, waitFor } from "./helpers.ts";

describe("engine — crash recovery", () => {
  const cleanups: (() => void)[] = [];
  const stores: Store[] = [];
  afterEach(async () => {
    for (const s of stores.splice(0)) await s.close();
    for (const c of cleanups.splice(0)) c();
  });

  function buildRegistry(workflows: WorkflowDefinition[], agents: AgentDefinition[]): ExtensionRegistry {
    const registry = new ExtensionRegistry();
    for (const a of agents) registry.addAgent("test", a);
    for (const w of workflows) registry.addWorkflow("test", w);
    return registry;
  }

  test("a running session left by a dead engine → failed:daemon_restart, task → human", async () => {
    const dir = tempDir();
    cleanups.push(() => dir.cleanup());

    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "stuck" } } };
    let started!: () => void;
    const startedP = new Promise<void>((r) => (started = r));
    const stuck: AgentDefinition = {
      name: "stuck",
      // Runs forever until its AbortSignal fires (the daemon-stop simulation).
      onRun: (ctx) =>
        new Promise<void>((resolve) => {
          started();
          ctx.signal.addEventListener("abort", () => resolve(), { once: true });
        }),
    };

    // -- engine #1: dispatch a session, then "crash" (stop) mid-run -----------
    const store1 = new Store(dir.path, { watch: false });
    stores.push(store1);
    await store1.open();
    const registry1 = buildRegistry([wf], [stuck]);
    const project1: EngineProject = { root: store1.root, store: store1, registry: registry1 };
    const engine1 = new Engine({ logger: new CapturingLogger() });
    await engine1.addProject(project1);

    const task = await store1.createTask({ title: "interrupt me" });
    await engine1.enroll(store1.root, task.id, { workflow: "w" });
    await startedP;
    await waitFor(() => store1.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = store1.sessions.liveSessionsForTask(task.id)[0]!.id;
    await waitFor(async () => (await store1.sessions.getMeta(sessionId))?.status === "running");

    engine1.stop(); // simulate the daemon dying — no terminal state is written

    // The meta on disk is still `running` (engine1 detached without finalising).
    const afterStop = await store1.sessions.getMeta(sessionId);
    expect(afterStop?.status).toBe("running");

    // -- engine #2: fresh process over the same files runs recovery ----------
    const store2 = new Store(dir.path, { watch: false });
    stores.push(store2);
    await store2.open(); // startup scan loads the `running` meta
    const registry2 = buildRegistry([wf], [stuck]);
    const project2: EngineProject = { root: store2.root, store: store2, registry: registry2 };
    const engine2 = new Engine({ logger: new CapturingLogger() });
    await engine2.addProject(project2); // recovery runs here
    engine2.stop();

    const meta = await store2.sessions.getMeta(sessionId);
    expect(meta?.status).toBe("failed");
    expect(meta?.error).toBe("daemon_restart");

    const view = await store2.taskView(task.id);
    expect(view.status).toBe("human");
    expect(view.step).toBe("do"); // position preserved for a resume

    const comments = await store2.listComments(task.id);
    expect(comments.some((c) => c.text === "daemon_restart")).toBe(true);

    const { lines } = await store2.sessions.readTranscript(sessionId);
    const end = lines.find(
      (l) => (l as { customType?: string }).customType === "autosk:session_end",
    ) as { data?: { status?: string; error?: string } } | undefined;
    expect(end?.data?.status).toBe("failed");
    expect(end?.data?.error).toBe("daemon_restart");
  });
});
