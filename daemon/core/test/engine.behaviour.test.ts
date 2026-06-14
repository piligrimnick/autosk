/**
 * Engine behavioural suite (plan §3.3-3.5, §3.7(3-4)) — replaces the Rust
 * executor conformance tests. Covers the happy path, onTransit rejection +
 * retry, double transit, missing transit, the worker-pool cap across two
 * projects, abort, and steer.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, StepTarget, WorkflowDefinition } from "@autosk/sdk";

import type { CustomEntry } from "@autosk/sdk";
import { gate, makeEngine, makeProject, oneStep, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

describe("engine — behavioural", () => {
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

  // -- happy path ----------------------------------------------------------

  test("enroll → run → transit → next step → done", async () => {
    const dev = transitAgent("dev", { step: "review" });
    const review = transitAgent("review", { status: "done" });
    const wf: WorkflowDefinition = {
      name: "fd",
      firstStep: "dev",
      steps: { dev: { agent: "dev" }, review: { agent: "review" } },
    };
    const p = track(await makeProject({ workflows: [wf], agents: [dev, review] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "ship it" });
    const enrolled = await engine.enroll(p.root, task.id, { workflow: "fd" });
    expect(enrolled.status).toBe("work");
    expect(enrolled.step).toBe("dev");

    await waitForComplete(p.store, task.id, "done");

    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("done");
    expect(view.workflow).toBe("fd");
    expect(view.step).toBe("review"); // terminal keeps the last step

    const sessions = p.store.sessions.sessionsForTask(task.id);
    expect(sessions).toHaveLength(2);
    // sessionsForTask is newest-first (descending id): review precedes dev.
    expect(sessions.map((s) => s.step)).toEqual(["review", "dev"]);
    expect(sessions.every((s) => s.status === "done")).toBe(true);
  });

  // -- onTransit rejection + retry ----------------------------------------

  test("onTransit rejection surfaces to the agent; a second target succeeds", async () => {
    const rejection: { v: string | null } = { v: null };
    const wf: WorkflowDefinition = {
      name: "fd",
      firstStep: "dev",
      steps: { dev: { agent: "dev" }, review: { agent: "review" } },
      onTransit(ctx, to) {
        if ("status" in to && to.status === "done" && ctx.step === "dev") {
          throw new Error("no direct done from dev");
        }
      },
    };
    const dev: AgentDefinition = {
      name: "dev",
      async onRun(ctx) {
        try {
          await ctx.transit({ status: "done" }); // rejected by onTransit
        } catch (e) {
          rejection.v = e instanceof Error ? e.message : String(e);
        }
        await ctx.transit({ step: "review" }); // second target succeeds
      },
    };
    const review = transitAgent("review", { status: "done" });
    const p = track(await makeProject({ workflows: [wf], agents: [dev, review] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "retry" });
    await engine.enroll(p.root, task.id, { workflow: "fd" });
    await waitForComplete(p.store, task.id, "done");

    expect(rejection.v).toContain("no direct done from dev");
    const sessions = p.store.sessions.sessionsForTask(task.id);
    // sessionsForTask is newest-first (descending id): review precedes dev.
    expect(sessions.map((s) => s.step)).toEqual(["review", "dev"]);
    expect(sessions.every((s) => s.status === "done")).toBe(true);
  });

  // -- double transit ------------------------------------------------------

  test("a second transit in one session throws; the first stands", async () => {
    const secondError: { v: string | null } = { v: null };
    const dev: AgentDefinition = {
      name: "dev",
      async onRun(ctx) {
        await ctx.transit({ status: "done" });
        try {
          await ctx.transit({ status: "cancel" });
        } catch (e) {
          secondError.v = e instanceof Error ? e.message : String(e);
        }
      },
    };
    const wf: WorkflowDefinition = { name: "single", firstStep: "dev", steps: { dev: { agent: "dev" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [dev] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "double" });
    await engine.enroll(p.root, task.id, { workflow: "single" });
    // The second transit is set INSIDE onRun, a couple of awaits AFTER the first
    // transit's seal completes, so the "fully settled" barrier alone can fire
    // before it is recorded — wait for the actual observable (the second-transit
    // rejection), which implies the first transit already committed.
    await waitFor(() => secondError.v !== null);

    expect(secondError.v).toContain("transit already called");
    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("done"); // first transit stands; the cancel never applied
    expect(p.store.sessions.sessionsForTask(task.id)).toHaveLength(1);
  });

  // -- missing transit -----------------------------------------------------

  test("an agent that returns without a transit parks the task to human", async () => {
    const noop: AgentDefinition = { name: "noop", onRun: async () => {} };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "noop" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [noop] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "forgot" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await waitForComplete(p.store, task.id, "human");

    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("human");
    expect(view.step).toBe("do"); // position preserved for a resume
    const sessions = p.store.sessions.sessionsForTask(task.id);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]!.status).toBe("failed");
    expect(sessions[0]!.error).toBe("agent_did_not_transit");
    const comments = await p.store.listComments(task.id);
    expect(comments.some((c) => c.text === "agent_did_not_transit")).toBe(true);
  });

  // -- worker-pool cap -----------------------------------------------------

  test("the global worker-pool cap is honoured across two projects", async () => {
    const g = gate();
    let active = 0;
    let maxActive = 0;
    const blocker = (name: string): AgentDefinition => ({
      name,
      async onRun(ctx) {
        active += 1;
        maxActive = Math.max(maxActive, active);
        await g.wait;
        active -= 1;
        await ctx.transit({ status: "done" });
      },
    });
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "b" } } };

    const p1 = track(await makeProject({ workflows: [wf], agents: [blocker("b")] }));
    const p2 = track(await makeProject({ workflows: [wf], agents: [blocker("b")] }));
    const { engine } = makeEngine({ workers: 2 });
    engines.push(engine);
    await engine.addProject(p1.project);
    await engine.addProject(p2.project);

    const ids: { project: TestProject; id: string }[] = [];
    for (const project of [p1, p2]) {
      for (let i = 0; i < 3; i++) {
        const t = await project.store.createTask({ title: `t${i}` });
        await engine.enroll(project.root, t.id, { workflow: "w" });
        ids.push({ project, id: t.id });
      }
    }

    // Two saturate the pool; the other four queue. Wait for that exact state,
    // then hold briefly and confirm the cap is never exceeded.
    await waitFor(() => engine.stats().running === 2 && engine.stats().queued === 4);
    await new Promise((r) => setTimeout(r, 40));
    expect(active).toBe(2);
    expect(maxActive).toBe(2);
    expect(engine.stats().running).toBe(2);
    expect(engine.stats().queued).toBe(4);

    g.open();
    await waitFor(async () => {
      for (const { project, id } of ids) {
        if ((await project.store.taskView(id)).status !== "done") return false;
      }
      return true;
    });
    expect(maxActive).toBe(2); // never exceeded the cap
  });

  // -- abort ---------------------------------------------------------------

  test("abort fires the AbortSignal + onAbort and marks the session aborted", async () => {
    const started = gate();
    let abortObserved = false;
    let onAbortCalled = false;
    const ag: AgentDefinition = {
      name: "long",
      onRun: (ctx) =>
        new Promise<void>((resolve) => {
          started.open();
          ctx.signal.addEventListener(
            "abort",
            () => {
              abortObserved = true;
              resolve();
            },
            { once: true },
          );
        }),
      onAbort: async () => {
        onAbortCalled = true;
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "long" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "abort me" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    const res = await engine.sessionAbort(p.root, sessionId);
    expect(res.handled).toBe(true);

    await waitForComplete(p.store, task.id, "human");
    expect(abortObserved).toBe(true);
    expect(onAbortCalled).toBe(true);
    const meta = await p.store.sessions.getMeta(sessionId);
    expect(meta?.status).toBe("aborted");
    const comments = await p.store.listComments(task.id);
    expect(comments.some((c) => c.text === "aborted")).toBe(true);
  });

  test("abort on a running agent WITHOUT onAbort still acts and reports handled", async () => {
    // The common case: most agents won't implement onAbort. Aborting their running
    // session must still fully take effect (signal fires, session sealed aborted,
    // task parked) and report `handled:true` — abort is never `unsupported_by_agent`
    // (round-5 BLOCKER; mirrors the steer-unsupported test below, which DOES report
    // `handled:false` because an absent steer hook cannot deliver the message).
    const started = gate();
    let abortObserved = false;
    const ag: AgentDefinition = {
      name: "plain",
      onRun: (ctx) =>
        new Promise<void>((resolve) => {
          started.open();
          ctx.signal.addEventListener(
            "abort",
            () => {
              abortObserved = true;
              resolve();
            },
            { once: true },
          );
        }),
      // deliberately NO onAbort
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "plain" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "abort plain" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    const res = await engine.sessionAbort(p.root, sessionId);
    expect(res.handled).toBe(true); // acted, despite no onAbort hook

    await waitForComplete(p.store, task.id, "human");
    expect(abortObserved).toBe(true); // the AbortSignal fired
    expect((await p.store.sessions.getMeta(sessionId))?.status).toBe("aborted");
    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("human");
    const comments = await p.store.listComments(task.id);
    expect(comments.some((c) => c.text === "aborted")).toBe(true);
  });

  // -- steer ---------------------------------------------------------------

  test("steer reaches onSteer mid-run and is recorded in the transcript", async () => {
    const started = gate();
    const steered = gate();
    const steerMsg: { v: string | null } = { v: null };
    const ag: AgentDefinition = {
      name: "steerable",
      async onRun(ctx) {
        started.open();
        await steered.wait;
        await ctx.transit({ status: "done" });
      },
      async onSteer(_ctx, message) {
        steerMsg.v = message;
        steered.open();
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "steerable" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "steer me" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    const res = await engine.sessionInput(p.root, sessionId, { kind: "steer", message: "use bun" });
    expect(res.handled).toBe(true);

    await waitForComplete(p.store, task.id, "done");
    expect(steerMsg.v).toBe("use bun");

    const { lines } = await p.store.sessions.readTranscript(sessionId);
    const steer = lines.find(
      (l): l is CustomEntry => (l as CustomEntry).type === "custom" && (l as CustomEntry).customType === "autosk:steer",
    );
    expect(steer?.data).toEqual({ kind: "steer", message: "use bun" });
  });

  // -- followup ------------------------------------------------------------

  test("followup reaches onFollowup mid-run", async () => {
    const started = gate();
    const delivered = gate();
    const got: { v: string | null } = { v: null };
    const ag: AgentDefinition = {
      name: "f",
      async onRun(ctx) {
        started.open();
        await delivered.wait;
        await ctx.transit({ status: "done" });
      },
      async onFollowup(_ctx, message) {
        got.v = message;
        delivered.open();
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "f" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "followup" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    const res = await engine.sessionInput(p.root, sessionId, { kind: "followup", message: "also fix lint" });
    expect(res.handled).toBe(true);
    await waitForComplete(p.store, task.id, "done");
    expect(got.v).toBe("also fix lint");
  });

  // -- abort / steer on a QUEUED session (regression: do not act before onRun)

  test("aborting a still-queued session seals it aborted without ever running the agent", async () => {
    const order: string[] = [];
    const hold = gate();
    const a1: AgentDefinition = {
      name: "a1",
      async onRun(ctx) {
        order.push("run:1");
        await hold.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const a2: AgentDefinition = {
      name: "a2",
      async onRun(ctx) {
        order.push("run:2");
        await ctx.transit({ status: "done" });
      },
      async onAbort() {
        order.push("abort:2");
      },
    };
    const p = track(await makeProject({ workflows: [oneStep("w1", "a1"), oneStep("w2", "a2")], agents: [a1, a2] }));
    const { engine } = makeEngine({ workers: 1 });
    engines.push(engine);
    await engine.addProject(p.project);

    const t1 = await p.store.createTask({ title: "first" });
    await engine.enroll(p.root, t1.id, { workflow: "w1" });
    await waitFor(() => p.store.sessions.liveSessionsForTask(t1.id).some((m) => m.status === "running"));

    const t2 = await p.store.createTask({ title: "second" });
    await engine.enroll(p.root, t2.id, { workflow: "w2" });
    // The pool is full (workers=1), so t2's session is created but stays queued.
    await waitFor(() => p.store.sessions.liveSessionsForTask(t2.id).some((m) => m.status === "queued"));
    const s2 = p.store.sessions.liveSessionsForTask(t2.id)[0]!.id;

    const res = await engine.sessionAbort(p.root, s2);
    expect(res.handled).toBe(true);
    await waitForComplete(p.store, t2.id, "human");
    expect((await p.store.sessions.getMeta(s2))?.status).toBe("aborted");

    // Free the pool; the aborted runtime must NOT be run when it is dequeued.
    hold.open();
    await waitForComplete(p.store, t1.id, "done");
    // Deterministic barrier (not a wall-clock window): the pool fully drains only
    // AFTER pump() has shifted t2's settled runtime out of the queue and dropped
    // it. Once queued+running are both 0 the dequeue has provably happened, so the
    // negative assertion below can no longer false-pass on a slow run.
    await waitFor(() => engine.stats().queued === 0 && engine.stats().running === 0);
    expect(order).toEqual(["run:1"]); // run:2 / abort:2 never happened
    expect((await p.store.sessions.getMeta(s2))?.status).toBe("aborted");
  });

  test("steering a still-queued session is rejected and never calls the hook before onRun", async () => {
    const order: string[] = [];
    const hold = gate();
    const a1: AgentDefinition = {
      name: "a1",
      async onRun(ctx) {
        order.push("run:1");
        await hold.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const a2: AgentDefinition = {
      name: "a2",
      async onRun(ctx) {
        order.push("run:2");
        await ctx.transit({ status: "done" });
      },
      async onSteer(_ctx, message) {
        order.push(`steer:2:${message}`);
      },
    };
    const p = track(await makeProject({ workflows: [oneStep("w1", "a1"), oneStep("w2", "a2")], agents: [a1, a2] }));
    const { engine } = makeEngine({ workers: 1 });
    engines.push(engine);
    await engine.addProject(p.project);

    const t1 = await p.store.createTask({ title: "first" });
    await engine.enroll(p.root, t1.id, { workflow: "w1" });
    await waitFor(() => p.store.sessions.liveSessionsForTask(t1.id).some((m) => m.status === "running"));

    const t2 = await p.store.createTask({ title: "second" });
    await engine.enroll(p.root, t2.id, { workflow: "w2" });
    await waitFor(() => p.store.sessions.liveSessionsForTask(t2.id).some((m) => m.status === "queued"));
    const s2 = p.store.sessions.liveSessionsForTask(t2.id)[0]!.id;

    const res = await engine.sessionInput(p.root, s2, { kind: "steer", message: "early" });
    expect(res.handled).toBe(false); // queued: onRun has not started

    hold.open();
    await waitForComplete(p.store, t2.id, "done");
    expect(order).toEqual(["run:1", "run:2"]); // onSteer never fired (no steer:2 entry)
  });

  test("steer on an agent without onSteer reports unsupported", async () => {
    const started = gate();
    const release = gate();
    const ag: AgentDefinition = {
      name: "plain",
      async onRun(ctx) {
        started.open();
        await release.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "plain" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "no steer" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await started.wait;
    await waitFor(() => p.store.sessions.liveSessionsForTask(task.id).length === 1);
    const sessionId = p.store.sessions.liveSessionsForTask(task.id)[0]!.id;

    const res = await engine.sessionInput(p.root, sessionId, { kind: "steer", message: "x" });
    expect(res.handled).toBe(false);
    release.open();
    await waitForComplete(p.store, task.id, "done");
  });
});
