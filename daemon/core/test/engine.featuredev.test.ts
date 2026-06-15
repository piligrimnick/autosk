/**
 * `@autosk/feature-dev` workflow — the scripted multi-step walk and the
 * `onTransit` visit-cap (P6 acceptance #4).
 *
 * The walk drives the SHIPPED `feature-dev` step graph (dev → review → docs →
 * validator → accept, with the review→dev bounce) using scripted stub agents and
 * a fake isolation double (so the step graph + `onTransit` are exercised without
 * git). The cap is asserted directly against the shipped `onTransit`.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type {
  AgentDefinition,
  IsolationProvider,
  StepTarget,
  TransitContext,
  WorkflowDefinition,
} from "@autosk/sdk";
import { DEV_VISIT_CAP, featureDevWorkflow } from "@autosk/feature-dev";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitForComplete } from "./helpers.ts";

/**
 * An isolation double that hands back the project root (no git, no worktrees).
 * Mirrors the real worktree provider's `{ tag, acquire }` shape — no `release`
 * (a worktree has nothing to quiesce) and no `reap` needed for this stub.
 */
const fakeIsolation: IsolationProvider = {
  tag: "worktree",
  async acquire({ projectRoot }) {
    return { cwd: projectRoot, meta: {} };
  },
};

function scripted(decide: () => StepTarget): AgentDefinition {
  return { onRun: async (ctx) => void (await ctx.transit(decide())) };
}

/**
 * The shipped `feature-dev` workflow with its four pi-agent steps replaced by
 * scripted stubs (so the real step graph + `onTransit` + the `accept`
 * statusStep are exercised without spawning pi). The step KEYS are unchanged,
 * so the agents stay inline — we just swap the `dev/review/docs/validator`
 * values for controllable doubles and keep `accept: statusStep("human")`.
 */
function scriptedFeatureDev(steps: {
  dev: () => StepTarget;
  review: () => StepTarget;
  docs: () => StepTarget;
  validator: () => StepTarget;
}): WorkflowDefinition {
  const real = featureDevWorkflow({ isolation: fakeIsolation });
  return {
    ...real,
    steps: {
      ...real.steps,
      dev: scripted(steps.dev),
      review: scripted(steps.review),
      docs: scripted(steps.docs),
      validator: scripted(steps.validator),
    },
  };
}

describe("feature-dev — scripted walk + visit cap", () => {
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

  test("walks dev → review → dev (bounce) → review → docs → validator → accept", async () => {
    let reviewCount = 0;
    const wf = scriptedFeatureDev({
      dev: () => ({ step: "review" }),
      // First review bounces back to dev; the second review forwards to docs.
      review: () => (++reviewCount === 1 ? { step: "dev" } : { step: "docs" }),
      docs: () => ({ step: "validator" }),
      validator: () => ({ step: "accept" }),
    });
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "feature" });
    await engine.enroll(p.root, task.id, { workflow: "feature-dev" });
    // `accept` is a human step → the task parks there.
    await waitForComplete(p.store, task.id, "human", 10000);

    const view = await p.store.taskView(task.id);
    expect(view.status).toBe("human");
    expect(view.step).toBe("accept");

    // The six sessions trace the exact bounce-and-forward path. sessionsForTask
    // is newest-first, so reverse to read it chronologically.
    const steps = p.store.sessions.sessionsForTask(task.id).map((m) => m.step);
    expect(steps.reverse()).toEqual(["dev", "review", "dev", "review", "docs", "validator"]);
  }, 15000);

  test("the dev visit-cap fires through the engine's REAL session-counting visits()", async () => {
    // The unit test below stubs `visits`; this drives enough real review→dev
    // bounces that the engine's own `visits('dev')` (transition.ts, counts prior
    // dev sessions) crosses the cap — protecting the prior-vs-inclusive semantics
    // against a future refactor of how sessions are counted.
    const wf = scriptedFeatureDev({
      dev: () => ({ step: "review" }),
      // `review` always bounces back to `dev` — the loop is bounded ONLY by the cap.
      review: () => ({ step: "dev" }),
      docs: () => ({ step: "validator" }),
      validator: () => ({ step: "accept" }),
    });
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "bouncer" });
    await engine.enroll(p.root, task.id, { workflow: "feature-dev" });
    // The cap parks the task to `human` once `review` attempts the (cap+1)th dev entry.
    await waitForComplete(p.store, task.id, "human", 15000);

    const metas = p.store.sessions.sessionsForTask(task.id);
    // Exactly DEV_VISIT_CAP dev sessions completed; the next dev entry was rejected.
    const devSessions = metas.filter((m) => m.step === "dev");
    expect(devSessions).toHaveLength(DEV_VISIT_CAP);
    expect(devSessions.every((m) => m.status === "done")).toBe(true);
    // The rejecting review session failed carrying the cap error; the task parked.
    const failed = metas.find((m) => m.status === "failed");
    expect(failed?.step).toBe("review");
    expect(failed?.error).toMatch(/too many times/);
    expect((await p.store.taskView(task.id)).status).toBe("human");
  }, 20000);

  test("onTransit rejects the 6th dev entry (visits('dev') >= cap) and allows the 5th", async () => {
    const wf = featureDevWorkflow({ isolation: fakeIsolation });
    expect(wf.onTransit).toBeDefined();

    const ctxWith = (devVisits: number): TransitContext =>
      ({
        taskId: "ask-cap",
        workflow: "feature-dev",
        step: "review",
        visits: (s: string) => (s === "dev" ? devVisits : 0),
        tasks: {} as TransitContext["tasks"],
        comment: async () => {},
      }) as TransitContext;

    // 5 prior dev runs → the 6th entry is rejected.
    expect(() => wf.onTransit!(ctxWith(DEV_VISIT_CAP), { step: "dev" })).toThrow(/too many times/);
    // 4 prior dev runs → the 5th entry is allowed.
    expect(() => wf.onTransit!(ctxWith(DEV_VISIT_CAP - 1), { step: "dev" })).not.toThrow();
    // The cap only guards the `dev` target, never other steps / statuses.
    expect(() => wf.onTransit!(ctxWith(99), { step: "review" })).not.toThrow();
    expect(() => wf.onTransit!(ctxWith(99), { status: "done" })).not.toThrow();
  });
});
