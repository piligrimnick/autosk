/**
 * Single-instance bind (plan §4 acceptance): two concurrent starts → exactly
 * one serves and the loser reports `alreadyRunning` (the binary then exits 0);
 * a stale lock from a dead pid is reclaimed; and the real binary exits 0 when a
 * live daemon already owns the socket.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  CapturingLogger,
  Engine,
  ProjectManager,
  ProjectRegistry,
  startDaemon,
  type DaemonRuntime,
  type StartDaemonResult,
} from "../src/index.ts";
import { RpcClient } from "./rpcHarness.ts";

const INDEX_TS = join(dirname(fileURLToPath(import.meta.url)), "..", "src", "index.ts");

function isRuntime(r: StartDaemonResult): r is DaemonRuntime {
  return !("alreadyRunning" in r);
}

function makeStartOpts(dir: string, socketPath: string, n: number) {
  const home = join(dir, `home${n}`);
  return {
    socketPath,
    token: null as string | null,
    idleWindowMs: null,
    projectManager: new ProjectManager({
      registry: new ProjectRegistry(join(home, ".autosk", "projects.json")),
      store: { watch: false as const },
      extensions: { home },
      logger: new CapturingLogger(),
    }),
    engine: new Engine({ rescanMs: 0, logger: new CapturingLogger() }),
    logger: new CapturingLogger(),
    exit: () => {},
  };
}

/** Polls until `socketPath` accepts a connection (or times out). */
async function waitForSocket(socketPath: string, timeoutMs = 5000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const alive = await new Promise<boolean>((resolve) => {
      const c = net.connect(socketPath);
      c.once("connect", () => {
        c.destroy();
        resolve(true);
      });
      c.once("error", () => resolve(false));
    });
    if (alive) return;
    if (Date.now() > deadline) throw new Error("waitForSocket: timed out");
    await new Promise((r) => setTimeout(r, 25));
  }
}

describe("single-instance bind", () => {
  let dir: string;
  let socketPath: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "autosk-single-"));
    socketPath = join(dir, "daemon.sock");
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  test("two concurrent starts: one serves, the loser reports alreadyRunning", async () => {
    const [a, b] = await Promise.all([
      startDaemon(makeStartOpts(dir, socketPath, 1)),
      startDaemon(makeStartOpts(dir, socketPath, 2)),
    ]);
    const runtimes = [a, b].filter(isRuntime);
    const losers = [a, b].filter((r) => !isRuntime(r));
    expect(runtimes).toHaveLength(1);
    expect(losers).toHaveLength(1);

    // The winner actually serves.
    const client = await RpcClient.connect(socketPath);
    const version = await client.call<{ version: string }>("meta.version", null);
    expect(typeof version.version).toBe("string");
    client.close();

    await runtimes[0]!.shutdown();
  });

  test("a stale lock from a dead pid is reclaimed", async () => {
    // A leftover lock referencing a pid that cannot exist (well above any live
    // pid) must not wedge a fresh start.
    writeFileSync(socketPath + ".lock", "2147483646\n");
    const result = await startDaemon(makeStartOpts(dir, socketPath, 1));
    expect(isRuntime(result)).toBe(true);
    const client = await RpcClient.connect(socketPath);
    expect(await client.call("meta.version", null)).toBeDefined();
    client.close();
    await (result as DaemonRuntime).shutdown();
  });

  test("concurrent cross-process spawns never double-bind the socket", async () => {
    // Regression for the empty-lock-file race: several SEPARATE daemon
    // processes (distinct pids) racing to claim the same socket at once used to
    // be able to misread a live holder's still-empty lock as stale and bind a
    // second listener at the same path, stranding clients that connected to the
    // orphaned listener (their requests hung forever). The in-process
    // concurrent test above can't catch this (both racers share process.pid).
    const n = 4;
    const procs = Array.from({ length: n }, (_, i) =>
      Bun.spawn({
        cmd: ["bun", INDEX_TS],
        env: {
          ...process.env,
          HOME: join(dir, `cc-home${i}`),
          AUTOSK_SOCK: socketPath,
          AUTOSK_IDLE_SECS: "0",
        },
        stdout: "ignore",
        stderr: "ignore",
      }),
    );
    try {
      await waitForSocket(socketPath);
      // Fire many concurrent clients; with the double-bind bug at least one
      // would connect to the stranded listener and never get a reply.
      const results = await Promise.all(
        Array.from({ length: 12 }, async () => {
          const client = await RpcClient.connect(socketPath);
          try {
            const v = await Promise.race([
              client.call<{ version: string }>("meta.version", null),
              new Promise<never>((_, rej) => setTimeout(() => rej(new Error("meta.version timed out")), 5000)),
            ]);
            return typeof v.version === "string";
          } finally {
            client.close();
          }
        }),
      );
      expect(results.every(Boolean)).toBe(true);
      // Exactly the losers (n-1) exit 0 on their own; the winner keeps serving.
      let exited = 0;
      for (const p of procs) {
        const code = await Promise.race([
          p.exited,
          new Promise<number | null>((r) => setTimeout(() => r(null), 1500)),
        ]);
        if (code === 0) exited++;
      }
      expect(exited).toBe(n - 1);
    } finally {
      for (const p of procs) p.kill();
      await Promise.all(procs.map((p) => p.exited));
    }
  }, 30_000);

  test("the binary exits 0 when a live daemon already owns the socket", async () => {
    const home1 = join(dir, "bin-home1");
    const home2 = join(dir, "bin-home2");
    const env1 = { ...process.env, HOME: home1, AUTOSK_SOCK: socketPath, AUTOSK_IDLE_SECS: "0" };
    const env2 = { ...process.env, HOME: home2, AUTOSK_SOCK: socketPath, AUTOSK_IDLE_SECS: "0" };

    const winner = Bun.spawn({ cmd: ["bun", INDEX_TS], env: env1, stdout: "ignore", stderr: "ignore" });
    try {
      await waitForSocket(socketPath);
      const loser = Bun.spawn({ cmd: ["bun", INDEX_TS], env: env2, stdout: "ignore", stderr: "ignore" });
      const code = await loser.exited;
      expect(code).toBe(0);
    } finally {
      winner.kill();
      await winner.exited;
    }
  }, 20_000);
});
