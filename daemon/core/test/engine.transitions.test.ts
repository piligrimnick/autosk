/**
 * Enroll/resume + human-step parking (plan §3.3 step 7, §3.4) and the
 * `visits()` guard pattern (plan §3.6) — the client-driven transition paths that
 * complement the in-session `ctx.transit` covered by the behavioural suite.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";

import { gate, makeEngine, makeProject, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitForComplete } from "./helpers.ts";

describe("engine — enroll / resume / human parking", () => {
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

  test("enroll {agent} materialises a single:<agent> workflow and runs it", async () => {
    const solo = transitAgent("solo", { status: "done" });
    const p = track(await makeProject({ agents: [solo] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "single" });
    const enrolled = await engine.enroll(p.root, task.id, { agent: "solo" });
    expect(enrolled.workflow).toBe("single:solo");
    expect(enrolled.step).toBe("do");

    await waitForComplete(p.store, task.id, "done");
    const sessions = p.store.sessions.sessionsForTask(task.id);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]!.workflow).toBe("single:solo");
  });

  test("enroll rejects an unknown agent / workflow / non-new task", async () => {
    const solo = transitAgent("solo", { status: "done" });
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "solo" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [solo] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "x" });
    await expect(engine.enroll(p.root, task.id, { agent: "ghost" })).rejects.toThrow(/unknown agent/);
    await expect(engine.enroll(p.root, task.id, { workflow: "nope" })).rejects.toThrow(/unknown workflow/);

    await engine.enroll(p.root, task.id, { workflow: "w" });
    await waitForComplete(p.store, task.id, "done");
    // Re-enrolling a non-new task is a conflict.
    await expect(engine.enroll(p.root, task.id, { workflow: "w" })).rejects.toThrow(/expected new/);
  });

  test("onTransit can reject the enroll → firstStep edge", async () => {
    const solo = transitAgent("solo", { status: "done" });
    const wf: WorkflowDefinition = {
      name: "w",
      firstStep: "do",
      steps: { do: { agent: "solo" } },
      onTransit(ctx, to) {
        // The enroll edge leaves the empty (pre-enrolment) step.
        if (ctx.step === "" && "step" in to && to.step === "do") {
          throw new Error("enroll blocked");
        }
      },
    };
    const p = track(await makeProject({ workflows: [wf], agents: [solo] }));
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

  test("transit into a human:true step parks the task; resume re-enters it", async () => {
    // dev → review(human). The dev agent hands off to the human-owned review.
    const release = gate();
    const ran: string[] = [];
    const dev: AgentDefinition = {
      name: "dev",
      async onRun(ctx) {
        ran.push("dev");
        await ctx.transit({ step: "review" });
      },
    };
    const finalize: AgentDefinition = {
      name: "finalize",
      async onRun(ctx) {
        ran.push("finalize");
        await release.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = {
      name: "fd",
      firstStep: "dev",
      steps: { dev: { agent: "dev" }, review: { human: true }, finalize: { agent: "finalize" } },
    };
    const p = track(await makeProject({ workflows: [wf], agents: [dev, finalize] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "needs review" });
    await engine.enroll(p.root, task.id, { workflow: "fd" });

    // Parked at the human step (status flips to human, never scheduled). Settle
    // fully so the dev session is sealed (non-live) before `resume` re-enters —
    // otherwise the resumed step can't be dispatched until the seal lands.
    await waitForComplete(p.store, task.id, "human");
    let view = await p.store.taskView(task.id);
    expect(view.step).toBe("review");
    expect(ran).toEqual(["dev"]); // the human step never ran an agent

    // The human resumes by routing to the next agent step.
    await engine.resume(p.root, task.id, { step: "finalize" });
    view = await p.store.taskView(task.id);
    expect(view.status).toBe("work");
    expect(view.step).toBe("finalize");

    release.open();
    await waitForComplete(p.store, task.id, "done");
    expect(ran).toEqual(["dev", "finalize"]);
  });

  test("resume defaults to the same step the task was parked at", async () => {
    // An agent that parks itself once, then completes on the resumed re-run.
    const seen: number[] = [];
    const flaky: AgentDefinition = {
      name: "flaky",
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
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "flaky" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [flaky] }));
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
    const solo = transitAgent("solo", { status: "done" });
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "solo" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [solo] }));
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
      name: "looper",
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
      steps: { do: { agent: "looper" } },
      onTransit(ctx, to) {
        if ("step" in to && to.step === "do" && ctx.visits("do") >= 3) {
          throw new Error("looped too many times");
        }
      },
    };
    const p = track(await makeProject({ workflows: [wf], agents: [looper] }));
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
