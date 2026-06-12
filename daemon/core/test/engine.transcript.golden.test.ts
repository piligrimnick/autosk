/**
 * A full run's transcript replays as valid pi-format JSONL (plan §3.2): a header
 * line + linear typed entries, including the agent's `message` / `custom` entries
 * and the engine's structural `autosk:transit` + `autosk:session_end`.
 *
 * "Golden" here pins the *structure* (the public transcript contract), not the
 * exact bytes — entry ids + timestamps are nondeterministic by design. The byte
 * layout of each line is already pinned by `records.golden.test.ts`.
 */

import { afterEach, describe, expect, test } from "bun:test";
import type {
  AgentDefinition,
  AssistantMessage,
  SessionHeader,
  TranscriptLine,
  UserMessage,
  WorkflowDefinition,
} from "@autosk/sdk";
import { parseTranscript } from "../src/index.ts";

import { makeEngine, makeProject, type TestProject } from "./engineHarness.ts";
import { waitForComplete } from "./helpers.ts";

const HEX8 = /^[0-9a-f]{8}$/;

function isHeader(line: TranscriptLine): line is SessionHeader {
  return (line as { type?: string }).type === "session";
}

describe("engine — transcript golden", () => {
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

  test("a full run is a valid pi-format transcript (header + entries)", async () => {
    const user: UserMessage = { role: "user", content: "build it", timestamp: 1_700_000_000_000 };
    const assistant: AssistantMessage = {
      role: "assistant",
      content: [{ type: "text", text: "done" }],
      provider: "test",
      model: "test-model",
      usage: {
        input: 10,
        output: 5,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 15,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
      stopReason: "stop",
      timestamp: 1_700_000_000_001,
    };
    const ag: AgentDefinition = {
      name: "scribe",
      async onRun(ctx) {
        ctx.log.message(user);
        ctx.log.message(assistant);
        ctx.log.custom("agent:note", { tool: "grep", hits: 3 });
        await ctx.transit({ status: "done" });
      },
    };
    const wf: WorkflowDefinition = { name: "w", firstStep: "do", steps: { do: { agent: "scribe" } } };
    const p = track(await makeProject({ workflows: [wf], agents: [ag] }));
    const { engine } = makeEngine();
    engines.push(engine);
    await engine.addProject(p.project);

    const task = await p.store.createTask({ title: "scribe run" });
    await engine.enroll(p.root, task.id, { workflow: "w" });
    // The structural `autosk:transit` + `autosk:session_end` entries are flushed
    // at commit step 2 — AFTER the task-status flip (step 1) but before the seal
    // (step 4). Wait for the full settle so the transcript on disk is complete.
    await waitForComplete(p.store, task.id, "done");

    const sessionId = p.store.sessions.sessionsForTask(task.id)[0]!.id;
    const raw = await Bun.file(p.store.paths.sessionTranscript(sessionId)).text();
    const lines = parseTranscript(raw); // re-parses the on-disk bytes (the public read path)

    // -- line 1: a well-formed header ---------------------------------------
    const header = lines[0]!;
    expect(isHeader(header)).toBe(true);
    if (!isHeader(header)) throw new Error("unreachable");
    expect(header).toMatchObject({
      type: "session",
      version: 1,
      id: sessionId,
      task_id: task.id,
      workflow: "w",
      step: "do",
      agent: "scribe",
    });
    expect(typeof header.timestamp).toBe("string");
    expect(header.cwd).toBe(p.root);

    // -- entries: linear, each with a typed shape + 8-hex id + timestamp -----
    const entries = lines.slice(1) as Exclude<TranscriptLine, SessionHeader>[];
    for (const e of entries) {
      expect(typeof e.type).toBe("string");
      expect(e.id).toMatch(HEX8);
      expect(typeof e.timestamp).toBe("string");
      expect("parentId" in e).toBe(false); // autosk transcripts are linear
    }

    // The agent's two messages + one custom, then the engine's transit + end.
    const kinds = entries.map((e) =>
      e.type === "custom" ? `custom:${(e as { customType: string }).customType}` : e.type,
    );
    expect(kinds).toEqual([
      "message",
      "message",
      "custom:agent:note",
      "custom:autosk:transit",
      "custom:autosk:session_end",
    ]);

    // The message entries survived verbatim (pi schema passes through).
    const msgs = entries.filter((e) => e.type === "message") as { message: unknown }[];
    expect(msgs[0]!.message).toEqual(user);
    expect(msgs[1]!.message).toEqual(assistant);

    // The structural entries carry the committed transition + terminal status.
    const transit = entries.find(
      (e) => (e as { customType?: string }).customType === "autosk:transit",
    ) as { data: { to: unknown; from: unknown } };
    expect(transit.data.to).toEqual({ status: "done" });
    expect(transit.data.from).toEqual({ workflow: "w", step: "do" });

    const end = entries.find(
      (e) => (e as { customType?: string }).customType === "autosk:session_end",
    ) as { data: { status: string } };
    expect(end.data.status).toBe("done");
  });
});
