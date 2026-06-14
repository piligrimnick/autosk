/**
 * Enroll/resume + statusStep parking/closing (plan §3.3 step 7, §3.4) and the
 * `visits()` guard pattern (plan §3.6) — the client-driven transition paths that
 * complement the in-session `ctx.transit` covered by the behavioural suite.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { statusStep, type AgentDefinition, type WorkflowDefinition } from "@autosk/sdk";

import { gate, makeEngine, makeProject, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitForComplete } from "./helpers.ts";

describe("engine — enroll / resume / statusStep parking", () => {
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

  test("enroll rejects an unknown workflow / non-new task", async () => {
    const solo = transitAgent({ status: "done" });
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: solo } };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "x" });
    await expect(engine.enroll(p.root, task.id, { workflow: "nope" })).rejects.toThrow(/unknown workflow/);

    await engine.enroll(p.root, task.id, { workflow: "w" });
    await waitForComplete(p.store, task.id, "done");
    // Re-enrolling a non-new task is a conflict.
    await expect(engine.enroll(p.root, task.id, { workflow: "w" })).rejects.toThrow(/expected new/);
  });

  test("onTransit can reject the enroll → firstStep edge", async () => {
    const solo = transitAgent({ status: "done" });
    const wf: WorkflowDefinition = {
      name: "w",
      firstStep: "do",
      steps: { do: solo },
      onTransit(ctx, to) {
        // The enroll edge leaves the empty (pre-enrolment) step.
        if (ctx.step === "" && "step" in to && to.step === "do") {
          throw new Error("enroll blocked");
        }
      },
    };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "x" });
    await expect(engine.enroll(p.root, task.id, { workflow: "w" })).rejects.toThrow(/enroll blocked/);
    // The enroll never committed: the task stays `new` and nothing is scheduled.
    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("new");
    expect(view.workflow).toBeNull();
    expect(p.store.sessions.sessionsForTask(task.id)).toHaveLength(0);
  });

  test("transit into a statusStep('human') parks the task; resume re-enters it", async () => {
    // dev → review (statusStep human). The dev agent hands off to the park step.
    const release = gate();
    const ran: string[] = [];
    const dev: AgentDefinition = {
      async onRun(ctx) {
        ran.push("dev");
        await ctx.transit({ step: "review" });
      },
    };
    const finalize: AgentDefinition = {
      async onRun(ctx) {
        ran.push("finalize");
        await release.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = {
      name: "fd",
      firstStep: "dev",
      steps: { dev, review: statusStep("human"), finalize },
    };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "needs review" });
    await engine.enroll(p.root, task.id, { workflow: "fd" });

    // Parked at the statusStep (status flips to human, never scheduled). Settle
    // fully so the dev session is sealed (non-live) before `resume` re-enters —
    // otherwise the resumed step can't be dispatched until the seal lands.
    await waitForComplete(p.store, task.id, "human");
    let view = await p.store.taskView(task.id);
    expect(view.step).toBe("review");
    expect(ran).toEqual(["dev"]); // the statusStep never ran an agent

    // The human resumes by routing to the next agent step.
    await engine.resume(p.root, task.id, { step: "finalize" });
    view = await p.store.taskView(task.id);
    expect(view.status).toBe("work");
    expect(view.step).toBe("finalize");

    release.open();
    await waitForComplete(p.store, task.id, "done");
    expect(ran).toEqual(["dev", "finalize"]);
  });

  test("transit into a statusStep('done') closes the task; ('cancel') cancels it", async () => {
    const edges: string[] = [];
    const finisher = (target: string): AgentDefinition => ({
      async onRun(ctx) {
        await ctx.transit({ step: target });
      },
    });
    const wf: WorkflowDefinition = {
      name: "term",
      firstStep: "shipper",
      steps: {
        shipper: finisher("done"),
        scrapper: finisher("cancelled"),
        done: statusStep("done"),
        cancelled: statusStep("cancel"),
      },
      onTransit(ctx, to) {
        // onTransit fires for the edge into a statusStep too.
        if ("step" in to) edges.push(`${ctx.step}->${to.step}`);
      },
    };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const t1 = await p.store.createTask({ title: "to close" });
    await engine.enroll(p.root, t1.id, { workflow: "term" });
    await waitForComplete(p.store, t1.id, "done");
    const v1 = await p.store.taskView(t1.id);
    expect(v1.status).toBe("done");
    expect(v1.step).toBe("done"); // closed showing the step it ended on
    expect(edges).toContain("shipper->done");

    // Same workflow, but route the firstStep agent to the cancel statusStep.
    const wf2: WorkflowDefinition = {
      ...wf,
      name: "term2",
      firstStep: "scrapper",
    };
    const p2 = track(await makeProject({ workflows: [wf2] }));
    await engine.addProject(p2.project);
    const t2 = await p2.store.createTask({ title: "to cancel" });
    await engine.enroll(p2.root, t2.id, { workflow: "term2" });
    await waitForComplete(p2.store, t2.id, "cancel");
    const v2 = await p2.store.taskView(t2.id);
    expect(v2.status).toBe("cancel");
    expect(v2.step).toBe("cancelled");
  });

  test("resume defaults to the same step the task was parked at", async () => {
    // An agent that parks itself once, then completes on the resumed re-run.
    const seen: number[] = [];
    const flaky: AgentDefinition = {
      async onRun(ctx) {
        const n = seen.length;
        seen.push(n);
        if (n === 0) {
          await ctx.transit({ status: "human" }); // park on the first attempt
        } else {
          await ctx.transit({ status: "done" }); // succeed on the resume
        }
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: flaky } };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "retry on resume" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    await waitForComplete(p.store, task.id, "human");
    expect((await p.store.taskView(task.id)).step).toBe("do");

    await engine.resume(p.root, task.id); // no target → same step "do"
    await waitForComplete(p.store, task.id, "done");
    expect(seen).toEqual([0, 1]);
    expect(p.store.sessions.sessionsForTask(task.id)).toHaveLength(2);
  });

  test("resume rejects a task that is not parked at human", async () => {
    const solo = transitAgent({ status: "done" });
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: solo } };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "fresh" });
    await expect(engine.resume(p.root, task.id)).rejects.toThrow(/expected human/);
  });

  test("a workflow can cap re-runs via ctx.visits()", async () => {
    // The agent always self-loops; onTransit rejects the 3rd entry into "do".
    const parkReason: { v: string | null } = { v: null };
    const looper: AgentDefinition = {
      async onRun(ctx) {
        try {
          await ctx.transit({ step: "do" }); // self-loop / retry
        } catch (e) {
          parkReason.v = e instanceof Error ? e.message : String(e);
          await ctx.transit({ status: "human" }); // give up → park
        }
      },
    };
    const wf: WorkflowDefinition = {
      name: "capped",
      firstStep: "do",
      steps: { do: looper },
      onTransit(ctx, to) {
        if ("step" in to && to.step === "do" && ctx.visits("do") >= 3) {
          throw new Error("looped too many times");
        }
      },
    };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "loopy" });
    await engine.enroll(p.root, task.id, { workflow: "capped" });
    await waitForComplete(p.store, task.id, "human");

    expect(parkReason.v).toContain("looped too many times");
    // 3 sessions ran at "do" (the 3rd self-loop was rejected → parked).
    expect(p.store.sessions.sessionsForTask(task.id)).toHaveLength(3);
  });
});
