/**
 * Engine × shipped worktree provider integration (P6 acceptance #1, engine side):
 * an `acquire` failure from the real `worktreeIsolation()` provider (here: the
 * project root is not a git repo) is wrapped by the engine as
 * `isolation_acquire_failed: …`, the session is sealed `failed`, and the task is
 * parked to `human` — exactly the contract `session.ts` documents.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, WorkflowDefinition } from "@autosk/sdk";
import { worktreeIsolation } from "@autosk/worktree";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { tempDir, waitForComplete } from "./helpers.ts";

describe("engine — worktreeIsolation acquire failure parks the task", () => {
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

  test("non-git project root → isolation_acquire_failed → parked to human, session failed", async () => {
    // git must exist for the provider to even classify the root; if absent the
    // provider throws "git binary not found", which is still an acquire failure —
    // either way the task parks, so the assertion holds without a git guard.
    let ran = false;
    const ag: AgentDefinition = {
      name: "do",
      async onRun(ctx) {
        ran = true;
        await ctx.transit({ status: "done" });
      },
    };
    const home = tempDir();
    cleanups.push(home.cleanup);
    const wf: WorkflowDefinition = {
      name: "iso-wt",
      firstStep: "do",
      steps: { do: { agent: "do" } },
      isolation: worktreeIsolation({ home: home.path }),
    };
    // makeProject's root is a plain temp dir — NOT a git repo — so acquire throws.
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "no git here" });
    await engine.enroll(p.root, task.id, { workflow: "iso-wt" });
    await waitForComplete(p.store, task.id, "human");

    expect(ran).toBe(false); // onRun never started

    const sessions = p.store.sessions.sessionsForTask(task.id);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]!.status).toBe("failed");
    expect(sessions[0]!.error).toMatch(/^isolation_acquire_failed:/);

    const comments = await p.store.listComments(task.id);
    expect(comments.some((c) => c.text.startsWith("isolation_acquire_failed:"))).toBe(true);
  });
});
