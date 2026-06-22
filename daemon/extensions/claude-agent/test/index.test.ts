/**
 * Unit tests for the claude-agent factory + the pure command / mcp-config / env
 * builders (`src/index.ts`).
 */

import { describe, expect, test } from "bun:test";
import type { AgentRunContext } from "@autosk/sdk";

import { autoskEnv, buildClaudeCommand, buildMcpConfig, claudeAgent } from "../src/index.ts";

function fakeCtx(overrides: Partial<AgentRunContext> = {}): AgentRunContext {
  return {
    projectRoot: "/repo/project",
    cwd: "/home/.autosk/worktrees/slug/ask-1",
    workflows: { current: { workflow: "feature-dev", step: "dev", targets: [] } },
    ...overrides,
  } as unknown as AgentRunContext;
}

describe("buildClaudeCommand", () => {
  test("assembles the stream-json flags + an acceptEdits permission mode by default", () => {
    const cmd = buildClaudeCommand({ claudeBin: "/bin/claude", model: "sonnet" });
    expect(cmd[0]).toBe("/bin/claude");
    expect(cmd).toContain("-p");
    expect(cmd).toContain("--output-format");
    expect(cmd).toContain("stream-json");
    expect(cmd).toContain("--input-format");
    expect(cmd).toContain("--verbose");
    expect(cmd).toContain("--include-partial-messages");
    expect(cmd).toContain("--replay-user-messages");
    expect(cmd).toContain("--model");
    expect(cmd).toContain("sonnet");
    expect(cmd).toContain("--permission-mode");
    expect(cmd[cmd.indexOf("--permission-mode") + 1]).toBe("acceptEdits");
    expect(cmd).not.toContain("--effort");
  });

  test("forwards the effort level when set", () => {
    const cmd = buildClaudeCommand({ claudeBin: "claude", effort: "high" });
    expect(cmd[cmd.indexOf("--effort") + 1]).toBe("high");
  });

  test("dangerouslySkipPermissions wins over permissionMode", () => {
    const cmd = buildClaudeCommand({ dangerouslySkipPermissions: true, permissionMode: "default" });
    expect(cmd).toContain("--dangerously-skip-permissions");
    expect(cmd).not.toContain("--permission-mode");
  });

  test("with an mcpConfig, bakes --mcp-config and auto-allows the autosk MCP tools", () => {
    const cmd = buildClaudeCommand({ claudeBin: "claude" }, { mcpConfig: '{"mcpServers":{}}' });
    const i = cmd.indexOf("--mcp-config");
    expect(i).toBeGreaterThan(0);
    expect(cmd[i + 1]).toBe('{"mcpServers":{}}');
    const allowed = cmd[cmd.indexOf("--allowedTools") + 1]!;
    expect(allowed).toContain("mcp__autosk__transit");
    expect(allowed).toContain("mcp__autosk__task");
    expect(allowed).toContain("mcp__autosk__comment");
  });

  test("interactive mode does not auto-allow the transit tool", () => {
    const cmd = buildClaudeCommand({ claudeBin: "claude" }, { mcpConfig: "{}", interactive: true });
    const allowed = cmd[cmd.indexOf("--allowedTools") + 1]!;
    expect(allowed).not.toContain("mcp__autosk__transit");
    expect(allowed).toContain("mcp__autosk__task");
  });

  test("omits --mcp-config when no config is provided (no autosk tools)", () => {
    const cmd = buildClaudeCommand({ claudeBin: "claude" });
    expect(cmd).not.toContain("--mcp-config");
    expect(cmd).not.toContain("--allowedTools");
  });

  test("forwards bare / disallowedTools / appendSystemPrompt / extraArgs", () => {
    const cmd = buildClaudeCommand({
      claudeBin: "claude",
      bare: true,
      disallowedTools: ["WebSearch"],
      appendSystemPrompt: "be terse",
      extraArgs: ["--foo"],
    });
    expect(cmd).toContain("--bare");
    expect(cmd[cmd.indexOf("--disallowedTools") + 1]).toBe("WebSearch");
    expect(cmd[cmd.indexOf("--append-system-prompt") + 1]).toBe("be terse");
    expect(cmd).toContain("--foo");
  });

  test("defaults the binary to $AUTOSK_CLAUDE_BIN or `claude`", () => {
    const prev = process.env.AUTOSK_CLAUDE_BIN;
    delete process.env.AUTOSK_CLAUDE_BIN;
    try {
      expect(buildClaudeCommand({})[0]).toBe("claude");
      process.env.AUTOSK_CLAUDE_BIN = "/custom/claude";
      expect(buildClaudeCommand({})[0]).toBe("/custom/claude");
    } finally {
      if (prev === undefined) delete process.env.AUTOSK_CLAUDE_BIN;
      else process.env.AUTOSK_CLAUDE_BIN = prev;
    }
  });
});

describe("buildMcpConfig", () => {
  test("emits a type:http server pointing at the per-session URL with the bearer", () => {
    const cfg = JSON.parse(buildMcpConfig("http://host.docker.internal:45678", "tok-abc"));
    const server = cfg.mcpServers.autosk;
    expect(server.type).toBe("http");
    expect(server.url).toBe("http://host.docker.internal:45678");
    expect(server.headers).toEqual({ Authorization: "Bearer tok-abc" });
    // No stdio command / args / env baked in any more.
    expect(server.command).toBeUndefined();
    expect(server.env).toBeUndefined();
  });

  test("a loopback URL (no sandbox) is carried verbatim", () => {
    const cfg = JSON.parse(buildMcpConfig("http://127.0.0.1:5000", "t"));
    expect(cfg.mcpServers.autosk.url).toBe("http://127.0.0.1:5000");
  });
});

describe("autoskEnv", () => {
  test("maps projectRoot → AUTOSK_CWD and step → AUTOSK_AGENT (no socket — the MCP tools are server-bound)", () => {
    expect(autoskEnv(fakeCtx())).toEqual({
      AUTOSK_CWD: "/repo/project",
      AUTOSK_AGENT: "dev",
    });
  });
});

describe("claudeAgent factory", () => {
  test("exposes the four hooks and carries no name (the step key is the agent name)", () => {
    const a = claudeAgent();
    expect("name" in a).toBe(false);
    expect(typeof a.onRun).toBe("function");
    expect(typeof a.onSteer).toBe("function");
    expect(typeof a.onFollowup).toBe("function");
    expect(typeof a.onAbort).toBe("function");
  });
});
