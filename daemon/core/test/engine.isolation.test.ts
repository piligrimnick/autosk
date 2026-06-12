/**
 * Isolation lifecycle (plan §3.5): a fake provider asserts acquire/release
 * ordering + the terminal flag for done, cancel, human-park, and a sibling
 * transition, plus park-on-acquire-failure.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type {
  AgentDefinition,
  IsolationHandle,
  IsolationProvider,
  StepTarget,
  WorkflowDefinition,
} from "@autosk/sdk";

import { gate, makeEngine, makeProject, transitAgent, type TestProject } from "./engineHarness.ts";
import { waitFor, waitForComplete } from "./helpers.ts";

interface IsoEvent {
  op: "acquire" | "release";
  taskId?: string;
  terminal?: boolean;
  cwd?: string;
}

/** A provider that records every acquire/release with its terminal flag. */
function recordingProvider(events: IsoEvent[], opts: { failAcquire?: boolean } = {}): IsolationProvider {
  return {
    tag: "fake",
    async acquire({ taskId }) {
      events.push({ op: "acquire", taskId });
      if (opts.failAcquire) throw new Error("no worktree for you");
      return { cwd: `/iso/${taskId}`, meta: {} } satisfies IsolationHandle;
    },
    async release(handle, { terminal }) {
      events.push({ op: "release", terminal, cwd: handle.cwd });
    },
  };
}

describe("engine — isolation lifecycle", () => {
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

  async function runOneStep(to: StepTarget): Promise<{ events: IsoEvent[]; p: TestProject; taskId: string; cwdSeen: string }> {
    const events: IsoEvent[] = [];
    let cwdSeen = "";
    const ag: AgentDefinition = {
      name: "do",
      async onRun(ctx) {
        cwdSeen = ctx.cwd;
        await ctx.transit(to);
      },
    };
    const wf: WorkflowDefinition = {
      name: "iso",
      firstStep: "do",
      steps: { do: { agent: "do" } },
      isolation: recordingProvider(events),
    };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);
    const task = await p.store.createTask({ title: "iso" });
    await engine.enroll(p.root, task.id, { workflow: "iso" });
    const finalStatus = "status" in to ? to.status : "work";
    // Settle on the session seal + isolation release (step 4), not the task-status
    // flip (step 1) — the `release` event is recorded at step 3, AFTER the status
    // flip, so a bare status wait would assert on `events` before the final
    // release lands.
    await waitForComplete(p.store, task.id, finalStatus);
    return { events, p, taskId: task.id, cwdSeen };
  }

  test("done releases with terminal:true; ctx.cwd is the handle path", async () => {
    const { events, taskId, cwdSeen } = await runOneStep({ status: "done" });
    expect(cwdSeen).toBe(`/iso/${taskId}`);
    expect(events).toEqual([
      { op: "acquire", taskId },
      { op: "release", terminal: true, cwd: `/iso/${taskId}` },
    ]);
  });

  test("cancel releases with terminal:true", async () => {
    const { events, taskId } = await runOneStep({ status: "cancel" });
    expect(events).toEqual([
      { op: "acquire", taskId },
      { op: "release", terminal: true, cwd: `/iso/${taskId}` },
    ]);
  });

  test("human-park releases with terminal:false", async () => {
    const { events, taskId } = await runOneStep({ status: "human" });
    expect(events).toEqual([
      { op: "acquire", taskId },
      { op: "release", terminal: false, cwd: `/iso/${taskId}` },
    ]);
  });

  test("a sibling step transition releases with terminal:false and re-acquires", async () => {
    const events: IsoEvent[] = [];
    const dev = transitAgent("dev", { step: "review" });
    const review = transitAgent("review", { status: "done" });
    const wf: WorkflowDefinition = {
      name: "iso2",
      firstStep: "dev",
      steps: { dev: { agent: "dev" }, review: { agent: "review" } },
      isolation: recordingProvider(events),
    };
    const p = track(await makeProject({ workflows: [wf], agents: [dev, review] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "two steps" });
    await engine.enroll(p.root, task.id, { workflow: "iso2" });
    await waitForComplete(p.store, task.id, "done");

    expect(events).toEqual([
      { op: "acquire", taskId: task.id },
      { op: "release", terminal: false, cwd: `/iso/${task.id}` }, // sibling: non-terminal
      { op: "acquire", taskId: task.id },
      { op: "release", terminal: true, cwd: `/iso/${task.id}` }, // review → done: terminal
    ]);
  });

  test("an acquire failure parks the task to human and seals the session failed; never releases", async () => {
    const events: IsoEvent[] = [];
    let ran = false;
    const ag: AgentDefinition = {
      name: "do",
      async onRun(ctx) {
        ran = true;
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = {
      name: "iso",
      firstStep: "do",
      steps: { do: { agent: "do" } },
      isolation: recordingProvider(events, { failAcquire: true }),
    };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "no iso" });
    await engine.enroll(p.root, task.id, { workflow: "iso" });
    await waitForComplete(p.store, task.id, "human");

    expect(events).toEqual([{ op: "acquire", taskId: task.id }]); // acquire threw; never released
    expect(ran).toBe(false); // onRun never started
    // Acquire now runs in the worker (bounded by the pool), so the session was
    // created as the claim BEFORE the acquire failed: the failure is recorded as
    // a `failed` session rather than vanishing.
    const sessions = p.store.sessions.sessionsForTask(task.id);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]!.status).toBe("failed");
    expect(sessions[0]!.error).toMatch(/^isolation_acquire_failed:/);
    const comments = await p.store.listComments(task.id);
    expect(comments.some((c) => c.text.startsWith("isolation_acquire_failed:"))).toBe(true);
  });

  test("the worker-pool cap bounds concurrent isolation acquisitions", async () => {
    // With workers=2 and 5 isolated tasks, only 2 acquisitions may be in flight
    // at once — the queued tasks must hold no worktree (ISSUE #5).
    const acquireGate = gate();
    let acquiring = 0;
    let maxAcquiring = 0;
    let held = 0;
    let maxHeld = 0;
    const provider: IsolationProvider = {
      tag: "slow",
      async acquire({ taskId }) {
        acquiring += 1;
        maxAcquiring = Math.max(maxAcquiring, acquiring);
        await acquireGate.wait;
        acquiring -= 1;
        held += 1;
        maxHeld = Math.max(maxHeld, held);
        return { cwd: `/iso/${taskId}`, meta: {} } satisfies IsolationHandle;
      },
      async release() {
        held -= 1;
      },
    };
    const release = gate();
    const ag: AgentDefinition = {
      name: "do",
      async onRun(ctx) {
        await release.wait;
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = {
      name: "iso",
      firstStep: "do",
      steps: { do: { agent: "do" } },
      isolation: provider,
    };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine({ workers: 2 });
    engines.push(engine);
    await engine.addProject(p.project);

    const ids: string[] = [];
    for (let i = 0; i < 5; i++) {
      const t = await p.store.createTask({ title: `t${i}` });
      await engine.enroll(p.root, t.id, { workflow: "iso" });
      ids.push(t.id);
    }

    // Exactly two acquisitions block on the gate; the other three sit queued with
    // no acquire even attempted. Wait for the FULL dispatched state (two acquiring
    // AND three queued) before the stability sleep, so the 30ms below only confirms
    // the cap is not exceeded and never races the post-enroll dispatch settling.
    await waitFor(() => acquiring === 2 && engine.stats().queued === 3);
    await new Promise((r) => setTimeout(r, 30));
    expect(acquiring).toBe(2);
    expect(maxAcquiring).toBe(2); // never more than `workers` acquisitions at once
    expect(engine.stats().running).toBe(2);
    expect(engine.stats().queued).toBe(3);

    acquireGate.open();
    release.open();
    await waitFor(async () => {
      for (const id of ids) {
        // Fully settled: task done AND its session sealed (so its `release` ran).
        if ((await p.store.taskView(id)).status !== "done") return false;
        if (p.store.sessions.liveSessionsForTask(id).length !== 0) return false;
      }
      return true;
    });
    expect(maxAcquiring).toBe(2); // cap held across the whole run
    expect(maxHeld).toBeLessThanOrEqual(2); // queued tasks never held a worktree
  });
});
