/**
 * `ctx.exec` (one-shot) and `ctx.spawn` (long-lived, line-buffered) smoke tests
 * (plan §3.4). These back the pi-agent extension (P6); core just provides the
 * generic process primitives. Driven through a real running session so the cwd
 * default + the session's AbortSignal are exercised end-to-end.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type { AgentDefinition, ChildHandle, ExecResult, WorkflowDefinition } from "@autosk/sdk";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitFor } from "./helpers.ts";

const BUN = process.execPath;

describe("engine — ctx.exec / ctx.spawn", () => {
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

  /** Runs `body(ctx)` inside one session, then transits done. */
  async function inSession(body: AgentDefinition["onRun"]): Promise<TestProject> {
    const ag: AgentDefinition = {
      async onRun(ctx) {
        await body(ctx);
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: ag } };
    const p = track(await makeProject({ workflows: [wf] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);
    const task = await p.store.createTask({ title: "child" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    // Gates on the real `bun -e` subprocess(es) inside `body` finishing. A fresh
    // bun cold-start + the session's disk reads are load-sensitive, so the
    // default 2000ms is too tight under full-suite contention (the work is
    // trivial console.log / stdin echo — under load it merely starves, never
    // wedges). Allow well past it; the per-test deadline below is larger still.
    await waitFor(async () => (await p.store.taskView(task.id)).status === "done", 20000);
    return p;
  }

  test("exec captures stdout, stderr, and the exit code; cwd defaults to ctx.cwd", async () => {
    let result: ExecResult | undefined;
    let cwdResult: ExecResult | undefined;
    const p = await inSession(async (ctx) => {
      result = await ctx.exec([
        BUN,
        "-e",
        "process.stdout.write('out'); process.stderr.write('err'); process.exit(3)",
      ]);
      cwdResult = await ctx.exec([BUN, "-e", "process.stdout.write(process.cwd())"]);
    });

    expect(result?.stdout).toBe("out");
    expect(result?.stderr).toBe("err");
    expect(result?.code).toBe(3);
    // ctx.cwd is the project root (no isolation), and exec inherits it.
    expect(cwdResult?.stdout).toBe(await realpath(p.root));
    // Bun's per-test deadline defaults to 5s; a real `bun -e` cold-start under
    // full-suite load can exceed it (a starved-not-wedged timeout, never an
    // assertion miss). Give real-subprocess tests room past their own waitFor.
  }, 20000);

  test("exec feeds stdin and honours a timeout (kill)", async () => {
    let echoed: ExecResult | undefined;
    let timedOut: ExecResult | undefined;
    await inSession(async (ctx) => {
      echoed = await ctx.exec([BUN, "-e", "process.stdin.pipe(process.stdout)"], { input: "ping" });
      timedOut = await ctx.exec([BUN, "-e", "setTimeout(() => {}, 10000)"], { timeoutMs: 50 });
    });
    expect(echoed?.stdout).toBe("ping");
    expect(timedOut?.code).not.toBe(0); // killed by the timeout
  }, 20000);

  test("spawn streams line-buffered stdout and resolves exited", async () => {
    const lines: string[] = [];
    let exit: { code: number | null } | undefined;
    await inSession(async (ctx) => {
      const child: ChildHandle = ctx.spawn([
        BUN,
        "-e",
        "console.log('alpha'); console.log('beta')",
      ]);
      child.onStdout((l) => lines.push(l));
      exit = await child.exited;
    });
    expect(lines).toEqual(["alpha", "beta"]);
    expect(exit?.code).toBe(0);
  }, 20000);

  test("spawn writes to child stdin via the writer", async () => {
    const lines: string[] = [];
    await inSession(async (ctx) => {
      const child = ctx.spawn([
        BUN,
        "-e",
        "const t = await Bun.stdin.text(); process.stdout.write('echo:' + t.trim() + '\\n')",
      ]);
      child.onStdout((l) => lines.push(l));
      await child.stdin.write(new TextEncoder().encode("hello\n"));
      await child.stdin.close();
      await child.exited;
    });
    expect(lines).toEqual(["echo:hello"]);
  }, 20000);
});

async function realpath(p: string): Promise<string> {
  const { realpath: rp } = await import("node:fs/promises");
  return rp(p);
}
