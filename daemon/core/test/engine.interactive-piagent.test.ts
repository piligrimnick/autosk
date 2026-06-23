/**
 * Interactive chat end-to-end through the engine against a STUB `pi --mode rpc`
 * process (plan §5). The real `@autosk/pi-agent` `runChat` runs unchanged, with
 * `piBin` pointed at the stub.
 *
 * Covered: an interactive session opens idle (runChat sends NO initial prompt —
 * the transcript is header-only until the user types); the first composer
 * message (a followup) reaches the idle pi as a fresh turn; a graceful end seals
 * the session `done`.
 */

import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { piAgent } from "@autosk/pi-agent";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitFor } from "./helpers.ts";

const STUB = fileURLToPath(new URL("../../extensions/pi-agent/test/fixtures/stub-pi.ts", import.meta.url));

beforeAll(() => {
  chmodSync(STUB, 0o755); // executable so the `#!/usr/bin/env bun` shebang runs it
});

describe("engine — interactive chat over a stub pi", () => {
  const cleanups: (() => void)[] = [];
  const engines: { stop(): void }[] = [];
  afterEach(() => {
    for (const e of engines.splice(0)) e.stop();
    for (const c of cleanups.splice(0)) c();
  });

  async function transcript(p: TestProject, id: string): Promise<string> {
    return Bun.file(p.store.paths.sessionTranscript(id)).text();
  }

  test("idle session: runChat sends no initial prompt; the first followup starts a turn", async () => {
    const p = await makeProject();
    cleanups.push(p.cleanup);
    // The stub just echoes each turn (`ack: <message>`) and never transits.
    writeFileSync(join(p.root, ".stub-pi.json"), JSON.stringify({ scenario: "never_transit" }));
    p.registry.addAgent("test", { name: "pi", description: "chat", agent: piAgent({ piBin: STUB }) });
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const meta = await engine.createInteractiveSession(p.root, "pi");
    await waitFor(async () => (await p.store.sessions.getMeta(meta.id))?.status === "running", 15000);

    // No initial prompt: the transcript holds ONLY the header (no echoed turn)
    // until the user sends the first message.
    const before = (await transcript(p, meta.id)).trim();
    expect(before.split("\n")).toHaveLength(1); // header line only
    expect(before).not.toContain("ack:");

    // The first composer message (a followup) reaches the idle pi as a fresh turn.
    const res = await engine.sessionInput(p.root, meta.id, { kind: "followup", message: "HELLO_CHAT" });
    expect(res.handled).toBe(true);
    await waitFor(async () => (await transcript(p, meta.id)).includes("ack: HELLO_CHAT"), 15000);

    // A second followup starts another turn (the session stays alive between turns).
    await engine.sessionInput(p.root, meta.id, { kind: "followup", message: "SECOND_TURN" });
    await waitFor(async () => (await transcript(p, meta.id)).includes("ack: SECOND_TURN"), 15000);

    // A graceful end seals the session `done` (not aborted; no task to park).
    const end = await engine.sessionEnd(p.root, meta.id);
    expect(end.handled).toBe(true);
    await waitFor(async () => (await p.store.sessions.getMeta(meta.id))?.status === "done", 15000);
    const sealed = await p.store.sessions.getMeta(meta.id);
    expect(sealed?.status).toBe("done");
    expect(sealed?.kind).toBe("interactive");
  }, 25000);
});
