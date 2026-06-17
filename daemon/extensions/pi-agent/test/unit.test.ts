/**
 * Unit tests for the pi-agent's pure pieces: target parsing, the pi command
 * builder, and prompt rendering. The full stdio drive is covered end-to-end
 * against a stub pi in `core/test/engine.piagent.test.ts`.
 */

import { describe, expect, test } from "bun:test";
import type { ChildHandle, Comment, StepTarget, TaskView, TranscriptMessage } from "@autosk/sdk";

import type { AgentRunContext } from "@autosk/sdk";

import {
  autoskEnv,
  buildInputCommand,
  buildPiCommand,
  isStateMismatch,
  kickbackMessage,
  parseTarget,
  PiDriver,
  piAgent,
  renderInitialPrompt,
  rejectionMessage,
  targetLabels,
} from "../src/index.ts";
// `Coalescer` is an internal driver helper (not part of the extension's public
// index surface); the test reaches into it directly to exercise the timer path.
import { Coalescer } from "../src/driver.ts";

describe("parseTarget", () => {
  test("maps `to` to a step or terminal status", () => {
    expect(parseTarget({ to: "review" })).toEqual({ step: "review" });
    expect(parseTarget({ to: "done" })).toEqual({ status: "done" });
    expect(parseTarget({ to: "cancel" })).toEqual({ status: "cancel" });
    expect(parseTarget({ to: "human" })).toEqual({ status: "human" });
  });
  test("accepts explicit {step} / {status} shapes", () => {
    expect(parseTarget({ step: "dev" })).toEqual({ step: "dev" });
    expect(parseTarget({ status: "done" })).toEqual({ status: "done" });
  });
  test("rejects junk", () => {
    expect(parseTarget(null)).toBeNull();
    expect(parseTarget({})).toBeNull();
    expect(parseTarget({ to: "  " })).toBeNull();
    expect(parseTarget("review")).toBeNull();
  });
});

describe("buildPiCommand", () => {
  test("assembles `pi --mode rpc` with model/thinking + the injected transit tool", () => {
    const cmd = buildPiCommand({
      piBin: "/bin/pi",
      model: "sonnet:high",
      thinking: "xhigh",
      piExtensions: ["/ext/a.ts"],
      piSkills: ["brave-search"],
      extraArgs: ["--no-session"],
    });
    expect(cmd[0]).toBe("/bin/pi");
    expect(cmd.slice(1, 3)).toEqual(["--mode", "rpc"]);
    expect(cmd).toContain("--model");
    expect(cmd).toContain("sonnet:high");
    expect(cmd).toContain("--thinking");
    expect(cmd).toContain("xhigh");
    // The autosk_transit tool is injected first (its path ends with the file).
    const eIdx = cmd.indexOf("-e");
    expect(eIdx).toBeGreaterThan(0);
    expect(cmd[eIdx + 1]).toMatch(/pi-transit-extension\.ts$/);
    expect(cmd).toContain("/ext/a.ts");
    expect(cmd).toContain("--skill");
    expect(cmd).toContain("brave-search");
    expect(cmd).toContain("--no-session");
  });

  test("defaults the binary to $AUTOSK_PI_BIN or `pi`", () => {
    const prev = process.env.AUTOSK_PI_BIN;
    delete process.env.AUTOSK_PI_BIN;
    try {
      expect(buildPiCommand({})[0]).toBe("pi");
      process.env.AUTOSK_PI_BIN = "/custom/pi";
      expect(buildPiCommand({})[0]).toBe("/custom/pi");
    } finally {
      if (prev === undefined) delete process.env.AUTOSK_PI_BIN;
      else process.env.AUTOSK_PI_BIN = prev;
    }
  });
});

describe("autoskEnv — project selector + comment authorship for the spawned pi", () => {
  test("maps the canonical project root to AUTOSK_CWD and the step to AUTOSK_AGENT", () => {
    const ctx = {
      projectRoot: "/repo/project",
      cwd: "/home/.autosk/worktrees/slug/ask-1", // worktree under isolation
      workflows: { current: { workflow: "feature-dev", step: "review", targets: [] } },
    } as unknown as AgentRunContext;
    expect(autoskEnv(ctx)).toEqual({ AUTOSK_CWD: "/repo/project", AUTOSK_AGENT: "review" });
  });
});

describe("piAgent factory", () => {
  test("exposes the four hooks and carries no name (the step key is the agent name)", () => {
    const a = piAgent();
    expect("name" in a).toBe(false);
    expect(typeof a.onRun).toBe("function");
    expect(typeof a.onSteer).toBe("function");
    expect(typeof a.onFollowup).toBe("function");
    expect(typeof a.onAbort).toBe("function");
  });
});

describe("buildInputCommand — pi steer/followup wire shape (v1 build_input_command)", () => {
  test("idle pi always takes a fresh `prompt` (steer and followup alike)", () => {
    expect(buildInputCommand("steer", "m", false)).toEqual({ cmd: { type: "prompt", message: "m" }, label: "prompt" });
    expect(buildInputCommand("followup", "m", false)).toEqual({
      cmd: { type: "prompt", message: "m" },
      label: "prompt",
    });
  });
  test("streaming pi takes `steer` / `follow_up` (snake_case) command TYPES, not a prompt", () => {
    expect(buildInputCommand("steer", "m", true)).toEqual({ cmd: { type: "steer", message: "m" }, label: "steer" });
    expect(buildInputCommand("followup", "m", true)).toEqual({
      cmd: { type: "follow_up", message: "m" },
      label: "follow_up",
    });
  });
});

describe("isStateMismatch (v1 is_state_mismatch)", () => {
  test("matches pi state-mismatch phrasings, ignores unrelated errors / empty", () => {
    expect(isStateMismatch("agent already streaming")).toBe(true);
    expect(isStateMismatch("not streaming (no active run)")).toBe(true);
    expect(isStateMismatch("STATE_MISMATCH: idle")).toBe(true);
    expect(isStateMismatch("")).toBe(false);
    expect(isStateMismatch(undefined)).toBe(false);
    expect(isStateMismatch("some other failure")).toBe(false);
  });
});

describe("PiDriver.input — idle/streaming dispatch + state-mismatch retry", () => {
  /** A fake child that RECORDS the JSON commands written to stdin and lets the
   *  test feed stdout lines (so request/response round-trips can be driven). */
  function fakeChildIO(): {
    child: ChildHandle;
    writes: Record<string, unknown>[];
    emitStdout(line: string): void;
  } {
    let stdoutCb: ((l: string) => void) | null = null;
    const writes: Record<string, unknown>[] = [];
    const stdin = {
      write: async (bytes: Uint8Array) => {
        const text = new TextDecoder().decode(bytes);
        for (const line of text.split("\n")) {
          if (line.trim() === "") continue;
          writes.push(JSON.parse(line) as Record<string, unknown>);
        }
      },
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: () => {},
      kill: () => {},
      exited: new Promise(() => {}),
    };
    return { child, writes, emitStdout: (l) => stdoutCb?.(l) };
  }

  const tick = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

  function makeDriver(): { f: ReturnType<typeof fakeChildIO>; driver: PiDriver; warnings: string[] } {
    const f = fakeChildIO();
    const warnings: string[] = [];
    const driver = new PiDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      warn: (m) => warnings.push(m),
    });
    return { f, driver, warnings };
  }

  /** Drains the last recorded command and acks it with the given response. */
  function ack(f: ReturnType<typeof fakeChildIO>, success: boolean, error?: string): void {
    const last = f.writes.at(-1)!;
    f.emitStdout(JSON.stringify({ type: "response", id: last.id, command: last.type, success, error }));
  }

  test("idle steer is sent as a `prompt`", async () => {
    const { f, driver } = makeDriver();
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "FOCUS" });
    ack(f, true);
    await p;
  });

  test("a steer issued while streaming is sent as a `steer` command", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // pi is now streaming
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer", message: "FOCUS" });
    ack(f, true);
    await p;
  });

  test("a followup issued while streaming is sent as a `follow_up` command", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" }));
    const p = driver.input("followup", "ALSO_DO_X");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "follow_up", message: "ALSO_DO_X" });
    ack(f, true);
    await p;
  });

  test("a state-mismatch rejection retries once with the opposite dispatch shape", async () => {
    const { f, driver } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // believed streaming
    const p = driver.input("steer", "FOCUS");
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "steer" }); // first attempt
    ack(f, false, "not streaming"); // pi raced us back to idle
    await tick();
    expect(f.writes.at(-1)).toMatchObject({ type: "prompt", message: "FOCUS" }); // opposite shape
    ack(f, true);
    await p;
  });

  test("a non-state-mismatch rejection does NOT retry and is surfaced via warn", async () => {
    const { f, driver, warnings } = makeDriver();
    f.emitStdout(JSON.stringify({ type: "agent_start" }));
    const p = driver.input("steer", "FOCUS");
    await tick();
    const writesBefore = f.writes.length;
    ack(f, false, "boom");
    await p;
    expect(f.writes.length).toBe(writesBefore); // no retry command written
    expect(warnings.some((w) => w.includes("pi rejected steer"))).toBe(true);
  });
});

describe("PiDriver — diagnostics (R1)", () => {
  /** A fake {@link ChildHandle} whose stdout/stderr/exit the test drives by hand. */
  function fakeChild(): {
    child: ChildHandle;
    emitStdout(line: string): void;
    emitStderr(line: string): void;
  } {
    let stdoutCb: ((l: string) => void) | null = null;
    let stderrCb: ((l: string) => void) | null = null;
    const stdin = {
      write: async () => {},
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: (cb) => void (stderrCb = cb),
      kill: () => {},
      exited: new Promise(() => {}), // never exits during these synchronous tests
    };
    return {
      child,
      emitStdout: (l) => stdoutCb?.(l),
      emitStderr: (l) => stderrCb?.(l),
    };
  }

  function driverWithWarnSink(): { f: ReturnType<typeof fakeChild>; warnings: string[] } {
    const f = fakeChild();
    const warnings: string[] = [];
    new PiDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      warn: (m) => warnings.push(m),
    });
    return { f, warnings };
  }

  test("forwards non-empty pi stderr lines through the warn hook, tagged `pi:stderr`", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStderr("Error: bad -e extension at mod.ts:10");
    f.emitStderr("   "); // blank/whitespace → drained but not surfaced
    expect(warnings).toEqual(["pi:stderr: Error: bad -e extension at mod.ts:10"]);
  });

  test("drops pi's terminal-teardown escape burst instead of surfacing it", () => {
    const { f, warnings } = driverWithWarnSink();
    // The exact teardown line pi writes to stderr on exit (mouse off, leave
    // alt-screen, end synchronized output, …) — all control bytes, no text.
    f.emitStderr(
      "\u001b[?2026h\u001b[r\u001b[?1006l\u001b[?1002l\u001b[?1000l\u001b[?1007h\u001b[?1049l\u001b[<999u\u001b[>4;0m\u001b[?2026l",
    );
    expect(warnings).toEqual([]);
  });

  test("keeps real stderr text while stripping embedded escape codes", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStderr("\u001b[31mError: boom\u001b[0m at mod.ts:10");
    expect(warnings).toEqual(["pi:stderr: Error: boom at mod.ts:10"]);
  });

  test("warns when autosk_transit is called with no usable target", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStdout(JSON.stringify({ type: "tool_execution_start", toolName: "autosk_transit", args: { junk: 1 } }));
    expect(warnings.some((w) => w.includes("had no usable target"))).toBe(true);
  });

  test("reports activity busy on agent_start and idle on agent_end (chat turn boundaries)", () => {
    const f = fakeChild();
    const activity: boolean[] = [];
    new PiDriver(f.child, {
      onMessage: () => {},
      onCustom: () => {},
      signal: new AbortController().signal,
      onActivity: (busy) => activity.push(busy),
    });
    f.emitStdout(JSON.stringify({ type: "agent_start" })); // turn begins → busy
    f.emitStdout(JSON.stringify({ type: "agent_end" })); // turn ends → idle
    expect(activity).toEqual([true, false]);
  });
});

describe("PiDriver — partial streaming (message_update coalescing)", () => {
  function fakeChild(): { child: ChildHandle; emitStdout(line: string): void } {
    let stdoutCb: ((l: string) => void) | null = null;
    const stdin = {
      write: async () => {},
      close: async () => {},
    } as unknown as WritableStreamDefaultWriter<Uint8Array>;
    const child: ChildHandle = {
      stdin,
      onStdout: (cb) => void (stdoutCb = cb),
      onStderr: () => {},
      kill: () => {},
      exited: new Promise(() => {}),
    };
    return { child, emitStdout: (l) => stdoutCb?.(l) };
  }

  /** A minimal cumulative assistant snapshot carrying one text block. */
  function assistantSnap(text: string): unknown {
    return { role: "assistant", content: [{ type: "text", text }], provider: "stub", model: "m" };
  }
  function snapText(m: TranscriptMessage): string {
    const blocks = (m as { content: { type: string; text?: string }[] }).content;
    return blocks.map((b) => (b.type === "text" ? (b.text ?? "") : "")).join("");
  }

  /** A driver that records the ordered partial/commit event log. */
  function recordingDriver(): { f: ReturnType<typeof fakeChild>; events: string[] } {
    const f = fakeChild();
    const events: string[] = [];
    new PiDriver(f.child, {
      onMessage: (m) => events.push(`commit:${snapText(m)}`),
      onCustom: () => {},
      onPartial: (m) => events.push(`partial:${snapText(m)}`),
      signal: new AbortController().signal,
    });
    return { f, events };
  }

  const mu = (text: string) => JSON.stringify({ type: "message_update", message: assistantSnap(text) });
  const me = (text: string) => JSON.stringify({ type: "message_end", message: assistantSnap(text) });

  test("leading-edge emits the first snapshot; intermediates coalesce to the latest on message_end flush", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(mu("a")); // leading edge → emitted immediately
    f.emitStdout(mu("ab")); // buffered within the window
    f.emitStdout(mu("abc")); // overwrites the buffered snapshot
    f.emitStdout(me("abc")); // flush the buffered "abc" then commit the durable line
    expect(events).toEqual(["partial:a", "partial:abc", "commit:abc"]);
  });

  test("message_end stops partial emission: no partial frame follows the committed message", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(mu("x"));
    f.emitStdout(me("x")); // commit
    // A new turn's message starts a fresh leading-edge partial (proves the
    // coalescer reset), but nothing partial fires AFTER a commit within a cycle.
    f.emitStdout(mu("y"));
    f.emitStdout(me("y"));
    // The exact sequence proves that within each turn the committed line is the
    // LAST frame (no partial trails it); a new turn's leading-edge partial is a
    // fresh cycle, not a late partial for the prior committed message.
    expect(events).toEqual(["partial:x", "commit:x", "partial:y", "commit:y"]);
  });

  test("a message_end with no preceding updates commits without emitting any partial", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(me("only"));
    expect(events).toEqual(["commit:only"]);
  });

  test("non-assistant message_update snapshots are ignored (no in-progress form)", () => {
    const { f, events } = recordingDriver();
    f.emitStdout(JSON.stringify({ type: "message_update", message: { role: "user", content: "hi" } }));
    f.emitStdout(JSON.stringify({ type: "message_update", message: null }));
    expect(events).toEqual([]);
  });

  test("with no onPartial hook, message_update is inert (mirrors only the commit)", () => {
    const f = fakeChild();
    const events: string[] = [];
    new PiDriver(f.child, {
      onMessage: (m) => events.push(`commit:${snapText(m)}`),
      onCustom: () => {},
      signal: new AbortController().signal,
    });
    f.emitStdout(mu("a"));
    f.emitStdout(me("a"));
    expect(events).toEqual(["commit:a"]);
  });
});

// The five tests above drive `message_end` (or no updates), so they only exercise
// the synchronous leading-edge + `flush()` paths. These cover the timer-based
// trailing edge in `Coalescer.arm()`: the re-arm loop that bounds the frame rate
// in production. Bun has no built-in timer mocking (its `useFakeTimers` only fakes
// `Date`, not `setTimeout`), so we use real timers with generous windows; the
// assertions are chosen to be robust to scheduling slack (each lands far from a
// timer boundary, or where a boundary firing is a no-op).
describe("Coalescer — trailing-edge timer (re-arm loop)", () => {
  const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
  const WINDOW = 50;

  test("re-arms across windows, each trailing flush emits the LATEST snapshot, far fewer than the pushes", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(WINDOW, (v) => emitted.push(v));

    // Drive a steady stream several times faster than the window for a handful of
    // windows: the leading edge fires once, then the timer must re-arm and flush
    // the trailing LATEST value once per window while input keeps arriving.
    let pushed = 0;
    const deadline = Date.now() + WINDOW * 5;
    while (Date.now() < deadline) {
      c.push(++pushed);
      await sleep(8);
    }

    // BEFORE any flush(): a leading edge PLUS at least one timer-driven trailing
    // emit (the re-arm loop ran), yet far fewer emissions than pushes (coalescing).
    expect(emitted.length).toBeGreaterThan(1);
    expect(emitted.length).toBeLessThan(pushed);
    expect(emitted[0]).toBe(1); // the leading edge is the first push

    c.flush(); // settle the final buffered snapshot deterministically
    expect(emitted.at(-1)).toBe(pushed); // the latest cumulative value, never an intermediate
    // Every emission is the latest-at-flush-time, so they strictly increase.
    for (let i = 1; i < emitted.length; i++) expect(emitted[i]!).toBeGreaterThan(emitted[i - 1]!);
  });

  test("goes idle after an empty window so the next push is a fresh leading edge", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(WINDOW, (v) => emitted.push(v));

    c.push(1); // leading edge → emitted immediately; timer armed
    expect(emitted).toEqual([1]);
    await sleep(WINDOW * 3); // a window passes EMPTY → timer fires with no buffer → idle
    expect(emitted).toEqual([1]); // nothing emitted while idle (no trailing flush)
    c.push(2); // timer is null again → a FRESH leading edge, emitted immediately
    expect(emitted).toEqual([1, 2]);
  });

  test("stop() cancels an armed trailing timer and drops the buffer WITHOUT emitting", async () => {
    const emitted: number[] = [];
    const c = new Coalescer<number>(WINDOW, (v) => emitted.push(v));
    c.push(1); // leading edge → emit 1; timer armed
    c.push(2); // buffered for the trailing edge
    c.stop(); // teardown: cancel the timer and drop the buffered 2 WITHOUT emitting
    expect(emitted).toEqual([1]);
    await sleep(WINDOW * 2); // the cancelled timer must NOT fire the dropped 2
    expect(emitted).toEqual([1]);
  });
});

describe("prompt rendering", () => {
  const task: TaskView = {
    id: "ask-1",
    title: "Build auth",
    description: "Add login",
    status: "work",
    workflow: "feature-dev",
    step: "dev",
    blocked: false,
    blocked_by: [],
    blocks: [],
    comment_count: 1,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
  const targets: StepTarget[] = [{ step: "dev" }, { step: "review" }, { status: "done" }, { status: "cancel" }, { status: "human" }];
  const comments: Comment[] = [
    { id: "c1", author: "alice", text: "use bcrypt", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z" },
  ];

  test("initial prompt names the agent/step, lists targets, and instructs the tool", () => {
    const p = renderInitialPrompt({
      firstMessage: "You are the DEVELOPER.\n\n",
      agentName: "@x/dev",
      workflow: "feature-dev",
      step: "dev",
      task,
      targets,
      comments,
    });
    expect(p).toContain("You are the DEVELOPER.");
    expect(p).toContain('You are agent "@x/dev" on step "dev" of workflow "feature-dev".');
    expect(p).toContain("Task: ask-1");
    expect(p).toContain("Title: Build auth");
    expect(p).toContain("Add login");
    expect(p).toContain("  - review");
    expect(p).toContain("  - done");
    expect(p).toContain("call the `autosk_transit` tool exactly once");
    expect(p).toContain("[alice] use bcrypt");
  });

  test("targetLabels dedups and preserves order", () => {
    expect(targetLabels(targets)).toEqual(["dev", "review", "done", "cancel", "human"]);
  });

  test("kickback and rejection messages reference the corrective attempt", () => {
    expect(kickbackMessage("ask-1", targets, 1, 3)).toContain("correction attempt 1 of 3");
    expect(kickbackMessage("ask-1", targets, 1, 3)).toContain("autosk_transit");
    const r = rejectionMessage({ step: "docs" }, "park it", targets, 2, 3);
    expect(r).toContain('to "docs" was rejected: park it');
    expect(r).toContain("correction attempt 2 of 3");
  });
});
