/**
 * Regression for issue #15: a large RPC request can arrive split across several
 * socket `data` chunks, and a chunk boundary may fall INSIDE a multibyte UTF-8
 * rune. The read path must decode with a streaming `TextDecoder` (holding the
 * trailing partial sequence until the next chunk) rather than decoding each
 * chunk independently, which would emit a U+FFFD for each dangling half and
 * silently corrupt the payload (Cyrillic `с` → `��`).
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { startTestDaemon, type TestDaemon } from "./rpcHarness.ts";

describe("RPC UTF-8 chunk-boundary integrity (issue #15)", () => {
  let td: TestDaemon;
  let cwd: string;

  beforeEach(async () => {
    td = await startTestDaemon();
    cwd = await td.makeProject("utf8");
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("a multibyte rune straddling a transport chunk boundary round-trips intact", async () => {
    const client = await td.client();
    // A distinctive Cyrillic marker whose first rune is 2 bytes (з = 0xD0 0xB7);
    // the harness splits the frame one byte into it, so 0xD0 ends chunk one and
    // 0xB7 starts chunk two.
    const marker = "зависимость";
    const description = `${"x".repeat(4096)} ${marker} ${"y".repeat(4096)}`;

    const frame = await client.callRawRuneSplit("task.create", { cwd, title: "t", description }, marker);
    expect(frame.error).toBeUndefined();
    const created = frame.result as { id: string; description: string };
    expect(created.description).toBe(description);
    expect(created.description).not.toContain("\uFFFD");

    // And it persisted intact — a fresh read serves the exact bytes.
    const got = await client.call<{ description: string }>("task.get", { cwd, id: created.id });
    expect(got.description).toBe(description);
    expect(got.description).not.toContain("\uFFFD");
  });
});
