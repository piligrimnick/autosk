/**
 * Extension hot-reload at the engine seam (plan §2-3): `setProjectRegistry`
 * atomically swaps the registry a project schedules over, and `dispatch` parks a
 * task whose workflow/step vanished so an orphaned `work` task self-heals after
 * its session settles. A live session is never disturbed by a swap.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";

import { ExtensionRegistry } from "../src/index.ts";
import { gate, makeEngine, makeProject, oneStep, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

describe("engine — extension hot-reload", () => {
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

  test("setProjectRegistry makes a freshly-added workflow schedulable without re-open", async () => {
    const p = track(await makeProject({ workflows: [] })); // empty registry
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "hot add" });
    // Before the swap the workflow is unknown — enroll is rejected.
    await expect(engine.enroll(p.root, task.id, { workflow: "hot" })).rejects.toThrow("unknown workflow: hot");

    // Build a fresh registry with the new workflow OFF to the side, then swap it
    // in (atomic: it is fully built before the single synchronous assignment). The
    // swap reports success (the root is engine-registered), the boolean the daemon
    // gates its "applied live" reporting on.
    const next = new ExtensionRegistry();
    next.addWorkflow("test", oneStep("hot", transitAgent({ status: "done" })));
    expect(engine.setProjectRegistry(p.root, next)).toBe(true);

    // The very next enroll sees the complete new registry and dispatches.
    await engine.enroll(p.root, task.id, { workflow: "hot" });
    await waitForComplete(p.store, task.id, "done");
    expect((await p.store.taskView(task.id)).status).toBe("done");
  });

  test("setProjectRegistry on an unregistered root is a no-op", () => {
    const { engine } = makeEngine();
    engines.push(engine);
    expect(() => engine.setProjectRegistry("/no/such/root", new ExtensionRegistry())).not.toThrow();
    // An unregistered root reports `false` (no swap landed) so the daemon never
    // claims "applied live" for a project the engine doesn't schedule.
    expect(engine.setProjectRegistry("/no/such/root", new ExtensionRegistry())).toBe(false);
  });

  test("dispatch parks a work task whose workflow is missing (self-heal)", async () => {
    const p = track(await makeProject({ workflows: [] })); // empty registry
    const { engine } = makeEngine();
    engines.push(engine);

    // Seed a `work` task pointing at a workflow no registry defines (the shape an
    // external hand-edit, or a removed extension, leaves behind).
    const task = await p.store.createTask({ title: "orphan" });
    await p.store.setPosition(task.id, { status: "work", workflow: "ghost", step: "go" });
    await engine.addProject(p.project); // kicks a scan → dispatch → park

    await waitFor(async () => (await p.store.taskView(task.id)).status === "human");
    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("human");
    expect(view.workflow).toBe("ghost"); // position preserved for the operator
    expect(view.step).toBe("go");
    const comments = await p.store.listComments(task.id);
    expect(comments.at(-1)).toMatchObject({ author: "autosk", text: "workflow_missing: ghost" });
  });

  test("a running session survives a remove; the task parks only after it settles", async () => {
    const g = gate();
    const dev: AgentDefinition = {
      async onRun(ctx) {
        await g.wait; // hold the session open across the registry swap
        await ctx.transit({ step: "review" }); // advance using the CAPTURED workflow
      },
    };
    const review = transitAgent({ status: "done" });
    const wf: WorkflowDefinition = { name: "W", firstStep: "dev", steps: { dev, review } };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "mid-run" });
    await engine.enroll(p.root, task.id, { workflow: "W" });
    // The dev session is now running (blocked on the gate).
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    expect((await p.store.taskView(task.id)).status).toBe("work");

    // Remove W mid-run by swapping in an empty registry. The live session must
    // NOT be parked out from under it.
    engine.setProjectRegistry(p.root, new ExtensionRegistry());
    await new Promise((r) => setTimeout(r, 50)); // let the kicked scan run
    expect((await p.store.taskView(task.id)).status).toBe("work");
    expect(p.store.sessions.liveSessionsForTask(task.id).length).toBe(1);

    // Let the captured dev agent finish: it transits to `review` (W is captured),
    // the session settles, and the NEXT scan dispatches review → W is gone → park.
    g.open();
    await waitFor(async () => (await p.store.taskView(task.id)).status === "human");
    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("human");
    const comments = await p.store.listComments(task.id);
    expect(comments.at(-1)).toMatchObject({ author: "autosk", text: "workflow_missing: W" });
    // dev ran to completion (done); review never started (parked instead).
    const sessions = p.store.sessions.sessionsForTask(task.id);
    expect(sessions.map((s) => s.step)).toEqual(["dev"]);
    expect(sessions[0]!.status).toBe("done");
  });
});
