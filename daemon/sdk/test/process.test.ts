/**
 * `runChild` (one-shot) and `spawnChild` (long-lived, line-buffered) — the
 * generic Bun process primitives moved out of the daemon's `engine/child.ts`
 * into `@autosk/sdk` so the engine and out-of-tree isolation providers share ONE
 * implementation (Docker-Isolation plan §4.2). These port the child-level
 * expectations: stdout/stderr/exit-code capture, cwd/env handling, stdin feed,
 * timeout kill, abort kills the child, and line buffering / replay.
 */

import { describe, expect, test } from "bun:test";

import { runChild, spawnChild } from "../src/index.ts";

const BUN = process.execPath;

/** A never-aborting signal for the happy-path cases. */
function liveSignal(): AbortSignal {
  return new AbortController().signal;
}

describe("runChild (one-shot)", () => {
  test("captures stdout, stderr, and the exit code", async () => {
    const r = await runChild(
      [BUN, "-e", "process.stdout.write('out'); process.stderr.write('err'); process.exit(3)"],
      { signal: liveSignal() },
    );
    expect(r.stdout).toBe("out");
    expect(r.stderr).toBe("err");
    expect(r.code).toBe(3);
  });

  test("runs in the given cwd", async () => {
    const { realpathSync } = await import("node:fs");
    const { tmpdir } = await import("node:os");
    const cwd = realpathSync(tmpdir());
    const r = await runChild([BUN, "-e", "process.stdout.write(process.cwd())"], {
      cwd,
      signal: liveSignal(),
    });
    expect(r.stdout).toBe(cwd);
  });

  test("merges env OVER process.env (PATH survives, custom var visible)", async () => {
    const r = await runChild(
      [BUN, "-e", "process.stdout.write(JSON.stringify({ k: process.env.MY_K, hasPath: !!process.env.PATH }))"],
      { env: { MY_K: "v1" }, signal: liveSignal() },
    );
    expect(JSON.parse(r.stdout)).toEqual({ k: "v1", hasPath: true });
  });

  test("feeds stdin then closes it", async () => {
    const r = await runChild([BUN, "-e", "process.stdin.pipe(process.stdout)"], {
      input: "ping",
      signal: liveSignal(),
    });
    expect(r.stdout).toBe("ping");
  });

  test("honours a timeout (kill)", async () => {
    const r = await runChild([BUN, "-e", "setTimeout(() => {}, 10000)"], {
      timeoutMs: 50,
      signal: liveSignal(),
    });
    expect(r.code).not.toBe(0); // killed by the timeout
  });

  test("an already-aborted signal kills the child immediately", async () => {
    const ctrl = new AbortController();
    ctrl.abort();
    const r = await runChild([BUN, "-e", "setTimeout(() => {}, 10000)"], { signal: ctrl.signal });
    expect(r.code).not.toBe(0); // never ran to completion
  });

  test("aborting mid-run kills the child", async () => {
    const ctrl = new AbortController();
    const p = runChild([BUN, "-e", "setTimeout(() => {}, 10000)"], { signal: ctrl.signal });
    setTimeout(() => ctrl.abort(), 50);
    const r = await p;
    expect(r.code).not.toBe(0);
  });
});

describe("spawnChild (long-lived)", () => {
  test("streams line-buffered stdout and resolves exited", async () => {
    const lines: string[] = [];
    const child = spawnChild([BUN, "-e", "console.log('alpha'); console.log('beta')"], {
      signal: liveSignal(),
    });
    child.onStdout((l) => lines.push(l));
    const exit = await child.exited;
    expect(lines).toEqual(["alpha", "beta"]);
    expect(exit.code).toBe(0);
  });

  test("buffers lines emitted before the first subscriber (replay)", async () => {
    const child = spawnChild([BUN, "-e", "console.log('one'); console.log('two')"], {
      signal: liveSignal(),
    });
    await child.exited; // let output land before subscribing
    const lines: string[] = [];
    child.onStdout((l) => lines.push(l));
    // Replay drains the buffered lines to the first subscriber.
    await Promise.resolve();
    expect(lines).toEqual(["one", "two"]);
  });

  test("writes to child stdin via the writer", async () => {
    const lines: string[] = [];
    const child = spawnChild(
      [BUN, "-e", "const t = await Bun.stdin.text(); process.stdout.write('echo:' + t.trim() + '\\n')"],
      { signal: liveSignal() },
    );
    child.onStdout((l) => lines.push(l));
    await child.stdin.write(new TextEncoder().encode("hello\n"));
    await child.stdin.close();
    await child.exited;
    expect(lines).toEqual(["echo:hello"]);
  });

  test("abort kills the spawned child", async () => {
    const ctrl = new AbortController();
    const child = spawnChild([BUN, "-e", "setTimeout(() => {}, 10000)"], { signal: ctrl.signal });
    setTimeout(() => ctrl.abort(), 50);
    const exit = await child.exited;
    expect(exit.code).not.toBe(0);
  });
});
