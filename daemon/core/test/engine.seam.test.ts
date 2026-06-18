/**
 * Execution-seam routing (Docker-Isolation plan §5): the engine routes
 * `ctx.exec` / `ctx.spawn` through the acquired isolation HANDLE's optional
 * `exec` / `spawn` when present (passing the resolved cwd + abort signal), and
 * falls back to the host child helpers when the handle omits them (today's
 * worktree / no-isolation behaviour).
 *
 * Driven through a real running session with a recording fake handle so the
 * cwd-default (= the handle's path) and the session's AbortSignal are exercised
 * end-to-end — without needing docker.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type {
  AgentDefinition,
  ChildHandle,
  ExecResult,
  IsolationExecOptions,
  IsolationHandle,
  IsolationProvider,
  IsolationSpawnOptions,
  WorkflowDefinition,
} from "@autosk/sdk";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitForComplete } from "./helpers.ts";

const BUN = process.execPath;

interface SeamCall {
  op: "exec" | "spawn";
  cmd: string[];
  cwd: string;
  hasSignal: boolean;
  env?: Record<string, string>;
}

/** A no-op child handle the fake `spawn` seam returns. */
function fakeChild(): ChildHandle {
  return {
    stdin: new WritableStream<Uint8Array>().getWriter(),
    onStdout: () => {},
    onStderr: () => {},
    kill: () => {},
    exited: Promise.resolve({ code: 0 }),
  };
}

/**
 * A provider whose `acquire` returns a handle WITH the exec/spawn seam, recording
 * every routed call. `cwd: "/iso/<task>"` so we can assert the engine resolved it.
 */
function seamProvider(calls: SeamCall[]): IsolationProvider {
  return {
    tag: "seam-fake",
    async acquire({ taskId }): Promise<IsolationHandle> {
      const cwd = `/iso/${taskId}`;
      return {
        cwd,
        meta: {},
        async exec(cmd: string[], o: IsolationExecOptions): Promise<ExecResult> {
          calls.push({ op: "exec", cmd, cwd: o.cwd, hasSignal: o.signal instanceof AbortSignal, env: o.env });
          return { code: 0, stdout: `seam-exec:${o.cwd}`, stderr: "" };
        },
        spawn(cmd: string[], o: IsolationSpawnOptions): ChildHandle {
          calls.push({ op: "spawn", cmd, cwd: o.cwd, hasSignal: o.signal instanceof AbortSignal, env: o.env });
          return fakeChild();
        },
      };
    },
  };
}

/** A provider whose handle has NO seam (host fallback path), like the worktree provider. */
function noSeamProvider(): IsolationProvider {
  return {
    tag: "noseam-fake",
    async acquire({ taskId }): Promise<IsolationHandle> {
      return { cwd: `/iso/${taskId}`, meta: {} };
    },
  };
}

describe("engine — ctx.exec / ctx.spawn seam routing", () => {
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

  async function runOnce(wf: WorkflowDefinition, body: AgentDefinition["onRun"]): Promise<TestProject> {
    const ag: AgentDefinition = {
      async onRun(ctx) {
        await body(ctx);
        await ctx.transit({ status: "done" });
      },
    };
    wf.steps = { do: ag };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);
    const task = await p.store.createTask({ title: "seam" });
    await engine.enroll(p.root, task.id, { workflow: wf.name });
    await waitForComplete(p.store, task.id, "done");
    return p;
  }

  test("with a seam handle: ctx.exec / ctx.spawn route through it with the resolved cwd + signal", async () => {
    const calls: SeamCall[] = [];
    let execResult: ExecResult | undefined;
    const wf: WorkflowDefinition = {
      name: "seam",
      firstStep: "do",
      steps: {},
      isolation: seamProvider(calls),
    };
    let taskCwd = "";
    await runOnce(wf, async (ctx) => {
      taskCwd = ctx.cwd;
      execResult = await ctx.exec(["echo", "hi"]);
      ctx.spawn(["pi", "--mode", "rpc"]);
    });

    // ctx.cwd became the handle path; the seam saw the SAME resolved cwd.
    expect(taskCwd).toMatch(/^\/iso\/ask-/);
    // exec routed through the seam (its canned result, not a real subprocess).
    expect(execResult?.stdout).toBe(`seam-exec:${taskCwd}`);
    expect(calls).toEqual([
      { op: "exec", cmd: ["echo", "hi"], cwd: taskCwd, hasSignal: true, env: undefined },
      { op: "spawn", cmd: ["pi", "--mode", "rpc"], cwd: taskCwd, hasSignal: true, env: undefined },
    ]);
  });

  test("spawn env (e.g. pi-agent's AUTOSK_CWD/AUTOSK_AGENT) is forwarded through the seam", async () => {
    // Mirrors how `@autosk/pi-agent` spawns pi: `ctx.spawn(cmd, { env: autoskEnv })`.
    // Under docker isolation the seam turns this env into `docker exec -e …`, so
    // the in-container `autosk` targets the right project and attributes comments.
    const calls: SeamCall[] = [];
    const wf: WorkflowDefinition = {
      name: "seam",
      firstStep: "do",
      steps: {},
      isolation: seamProvider(calls),
    };
    await runOnce(wf, async (ctx) => {
      ctx.spawn(["pi", "--mode", "rpc"], {
        env: { AUTOSK_CWD: ctx.projectRoot, AUTOSK_AGENT: "do" },
      });
    });
    expect(calls).toHaveLength(1);
    expect(calls[0]!.op).toBe("spawn");
    expect(calls[0]!.env).toEqual({ AUTOSK_CWD: expect.any(String), AUTOSK_AGENT: "do" });
    expect(calls[0]!.env?.AUTOSK_CWD).not.toBe(""); // the real project root
  });

  test("an opts.cwd override is forwarded to the seam (signal still resolved)", async () => {
    const calls: SeamCall[] = [];
    const wf: WorkflowDefinition = {
      name: "seam",
      firstStep: "do",
      steps: {},
      isolation: seamProvider(calls),
    };
    await runOnce(wf, async (ctx) => {
      await ctx.exec(["ls"], { cwd: "/custom/dir" });
    });
    expect(calls).toEqual([{ op: "exec", cmd: ["ls"], cwd: "/custom/dir", hasSignal: true }]);
  });

  test("with a seamless handle: ctx.exec falls back to the HOST child helpers", async () => {
    const { realpathSync } = await import("node:fs");
    const { tmpdir } = await import("node:os");
    const validCwd = realpathSync(tmpdir());
    let result: ExecResult | undefined;
    const wf: WorkflowDefinition = {
      name: "seam",
      firstStep: "do",
      steps: {},
      isolation: noSeamProvider(),
    };
    await runOnce(wf, async (ctx) => {
      // A real host subprocess runs (no seam to intercept it). The handle's cwd
      // (`/iso/<task>`) is a fake path with no real dir, so point this one-shot at
      // a valid host dir — the assertion is only that the HOST helper ran it.
      result = await ctx.exec([BUN, "-e", "process.stdout.write('host-ran')"], { cwd: validCwd });
    });
    expect(result?.stdout).toBe("host-ran");
  });
});
