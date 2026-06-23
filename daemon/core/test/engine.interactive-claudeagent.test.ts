/**
 * Interactive chat end-to-end through the engine against a STUB `claude -p`
 * stream-json process (claude-agent acceptance: interactive mode). The structural
 * twin of `engine.interactive-piagent.test.ts` — the real `@autosk/claude-agent`
 * `runChat` runs unchanged, with `claudeBin` pointed at the stub.
 *
 * Covered: an interactive session opens idle (runChat sends NO initial prompt —
 * the transcript is header-only until the user types, and NO transit tool is
 * registered); the first composer message (a followup) reaches the idle claude as
 * a fresh turn; a second followup proves the process stays alive between turns; a
 * graceful end seals the session `done` / `interactive`.
 */

import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { claudeAgent } from "@autosk/claude-agent";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitFor } from "./helpers.ts";

const STUB = fileURLToPath(new URL("../../extensions/claude-agent/test/fixtures/stub-claude.ts", import.meta.url));

beforeAll(() => {
  chmodSync(STUB, 0o755); // executable so the `#!/usr/bin/env bun` shebang runs it
});

describe("engine — interactive chat over a stub claude", () => {
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
    // The stub just echoes each turn (`ack: <message>`) and never transits — which
    // is exactly the interactive contract (a chat has nothing to transit).
    writeFileSync(join(p.root, ".stub-claude.json"), JSON.stringify({ scenario: "never_transit" }));
    p.registry.addAgent("test", { name: "claude", description: "chat", agent: claudeAgent({ claudeBin: STUB }) });
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const meta = await engine.createInteractiveSession(p.root, "claude");
    await waitFor(async () => (await p.store.sessions.getMeta(meta.id))?.status === "running", 15000);

    // No initial prompt: the transcript holds ONLY the header (no echoed turn)
    // until the user sends the first message.
    const before = (await transcript(p, meta.id)).trim();
    expect(before.split("\n")).toHaveLength(1); // header line only
    expect(before).not.toContain("ack:");

    // The first composer message (a followup) reaches the idle claude as a fresh turn.
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
