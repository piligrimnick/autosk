/**
 * Scheduler concurrency regressions (the bugs the steady-state happy-path suite
 * cannot reach because it uses fast agents and an idle pool):
 *
 *  - BLOCKER #1: a sibling transition completing during another task's dispatch
 *    must not cause a stale-step re-dispatch (a step runs at most once per
 *    occupancy). Reproduced deterministically by gating `sessions.create` so the
 *    scan parks mid-pass while the not-yet-dispatched task advances underneath it.
 *  - ISSUE #6: a session id is routable ONLY under its own project's root.
 *  - ISSUE #4: the periodic safety rescan picks up a task made schedulable with
 *    no engine event.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";

import type { CreateSessionInput } from "../src/index.ts";
import { gate, makeEngine, makeProject, oneStep, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

describe("engine — scheduler concurrency", () => {
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

  test("a sibling transition during another task's dispatch never re-runs the stale step", async () => {
    let devRuns = 0;
    const devGate = gate();
    const dev: AgentDefinition = {
      name: "dev",
      async onRun(ctx) {
        devRuns += 1;
        await devGate.wait;
        await ctx.transit({ step: "review" });
      },
    };
    const review = transitAgent("review", { status: "done" });
    const multi: WorkflowDefinition = {
      name: "multi",
      firstStep: "dev",
      steps: { dev: { agent: "dev" }, review: { agent: "review" } },
    };
    const bAgent = transitAgent("b", { status: "done" });

    const p = track(await makeProject({ workflows: [multi, oneStep("one", "b")], agents: [dev, review, bAgent] }));
    const { engine } = makeEngine({ workers: 4 });
    engines.push(engine);
    await engine.addProject(p.project);

    // Assign roles by id sort: the scheduler enumerates ascending, so `bId`
    // (sorts first) is dispatched first and `aId` (sorts second) is the task that
    // advances while the scan is parked in B's create.
    const x1 = await p.store.createTask({ title: "x1" });
    const x2 = await p.store.createTask({ title: "x2" });
    const [bId, aId] = [x1.id, x2.id].sort() as [string, string];

    // A is running its dev step (gated), so it is `work@dev` with a live session.
    await engine.enroll(p.root, aId, { workflow: "multi" });
    await waitFor(() => p.store.sessions.liveSessionsForTask(aId).some((m) => m.status === "running"));

    // Gate B's session creation: the scan will park inside `dispatch(B)` →
    // `sessions.create(B)`, AFTER it already snapshotted A as `work@dev`.
    const createGate = gate();
    let gatedEntered = false;
    const realCreate = p.store.sessions.create.bind(p.store.sessions);
    p.store.sessions.create = async (input: CreateSessionInput) => {
      if (input.task_id === bId && !gatedEntered) {
        gatedEntered = true;
        await createGate.wait;
      }
      return realCreate(input);
    };

    await engine.enroll(p.root, bId, { workflow: "one" });
    await waitFor(() => gatedEntered); // scan is now parked in B's create

    // A's dev step completes → A advances to `review` and seals its dev session,
    // all while the scan is still parked mid-pass (it has not yet reached A).
    devGate.open();
    await waitFor(async () => {
      const v = await p.store.taskView(aId);
      return v.status === "work" && v.step === "review";
    });

    // Release the scan. When it reaches A it must re-read FRESH and dispatch the
    // `review` step — NOT a second `dev` session at the stale snapshot step.
    createGate.open();
    // Settle fully (both sessions sealed) before asserting on session status — a
    // bare status wait would fire while the `review` session is still `running`,
    // making the very regression guard flaky (a false failure here could be
    // "fixed" by weakening it, silently dropping the BLOCKER #1 coverage).
    await waitForComplete(p.store, aId, "done");

    expect(devRuns).toBe(1); // dev ran exactly once
    const sessions = p.store.sessions.sessionsForTask(aId);
    expect(sessions.map((s) => s.step)).toEqual(["dev", "review"]);
    expect(sessions.every((s) => s.status === "done")).toBe(true);
  });

  test("a session id is not routable under another project's root (ISSUE #6)", async () => {
    const release = gate();
    const ag: AgentDefinition = {
      name: "a",
      async onRun(ctx) {
        await release.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const wf = oneStep("w", "a");
    const pa = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const pb = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(pa.project);
    await engine.addProject(pb.project);

    const t = await pa.store.createTask({ title: "x" });
    await engine.enroll(pa.root, t.id, { workflow: "w" });
    await waitFor(() => pa.store.sessions.liveSessionsForTask(t.id).some((m) => m.status === "running"));
    const sid = pa.store.sessions.liveSessionsForTask(t.id)[0]!.id;

    // pb is a registered project, but the session belongs to pa — routing it under
    // pb's root must be rejected, not silently delivered to pa's session.
    await expect(engine.sessionInput(pb.root, sid, { kind: "steer", message: "x" })).rejects.toThrow(
      /session not running/,
    );
    await expect(engine.sessionAbort(pb.root, sid)).rejects.toThrow(/session not running/);

    release.open();
    await waitForComplete(pa.store, t.id, "done");
  });

  test("the periodic safety rescan dispatches a task made schedulable with no engine event (ISSUE #4)", async () => {
    const solo = transitAgent("solo", { status: "done" });
    const p = track(await makeProject({ workflows: [oneStep("w", "solo")], agents: [solo] }));
    const { engine } = makeEngine({ rescanMs: 20 });
    engines.push(engine);
    await engine.addProject(p.project); // initial scan: nothing schedulable

    const task = await p.store.createTask({ title: "orphan" });
    // Enrol by writing the position straight to the store — this fires NO engine
    // event (unlike engine.enroll, which kicks a scan). Only the periodic rescan
    // can pick it up; without it the task would stall forever.
    await p.store.setPosition(task.id, { status: "work", workflow: "w", step: "do" });

    await waitFor(async () => (await p.store.taskView(task.id)).status === "done", 1500);
  });
});
