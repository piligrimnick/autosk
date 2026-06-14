/**
 * Unit tests for the pi-agent's pure pieces: target parsing, the pi command
 * builder, and prompt rendering. The full stdio drive is covered end-to-end
 * against a stub pi in `core/test/engine.piagent.test.ts`.
 */

import { describe, expect, test } from "bun:test";
import type { ChildHandle, Comment, StepTarget, TaskView } from "@autosk/sdk";

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

  test("warns when autosk_transit is called with no usable target", () => {
    const { f, warnings } = driverWithWarnSink();
    f.emitStdout(JSON.stringify({ type: "tool_execution_start", toolName: "autosk_transit", args: { junk: 1 } }));
    expect(warnings.some((w) => w.includes("had no usable target"))).toBe(true);
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
