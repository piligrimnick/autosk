/**
 * Unit tests for the claude-agent factory + the pure command / mcp-config / env
 * builders (`src/index.ts`).
 */

import { describe, expect, test } from "bun:test";
import type { AgentRunContext } from "@autosk/sdk";

import { autoskEnv, buildClaudeCommand, buildMcpConfig, claudeAgent, resolveAutoskdBin } from "../src/index.ts";

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
  test("points the autosk stdio server at `autoskd mcp` with the autosk env baked in", () => {
    const prevSock = process.env.AUTOSK_SOCK;
    const prevBin = process.env.AUTOSKD_BIN;
    process.env.AUTOSK_SOCK = "/tmp/autosk.sock";
    process.env.AUTOSKD_BIN = "/bin/autoskd";
    try {
      const cfg = JSON.parse(buildMcpConfig(fakeCtx(), { transit: true }));
      const server = cfg.mcpServers.autosk;
      expect(server.type).toBe("stdio");
      expect(server.command).toBe("/bin/autoskd");
      expect(server.args).toEqual(["mcp"]);
      expect(server.env).toEqual({
        AUTOSK_CWD: "/repo/project",
        AUTOSK_AGENT: "dev",
        AUTOSK_SOCK: "/tmp/autosk.sock",
        AUTOSK_MCP_TRANSIT: "1",
      });
    } finally {
      if (prevSock === undefined) delete process.env.AUTOSK_SOCK;
      else process.env.AUTOSK_SOCK = prevSock;
      if (prevBin === undefined) delete process.env.AUTOSKD_BIN;
      else process.env.AUTOSKD_BIN = prevBin;
    }
  });

  test("transit:false omits AUTOSK_MCP_TRANSIT (interactive mode)", () => {
    const cfg = JSON.parse(buildMcpConfig(fakeCtx(), { transit: false }));
    expect(cfg.mcpServers.autosk.env.AUTOSK_MCP_TRANSIT).toBeUndefined();
  });
});

describe("autoskEnv", () => {
  test("maps projectRoot → AUTOSK_CWD, step → AUTOSK_AGENT, and includes AUTOSK_SOCK", () => {
    const prev = process.env.AUTOSK_SOCK;
    process.env.AUTOSK_SOCK = "/tmp/d.sock";
    try {
      expect(autoskEnv(fakeCtx())).toEqual({
        AUTOSK_CWD: "/repo/project",
        AUTOSK_AGENT: "dev",
        AUTOSK_SOCK: "/tmp/d.sock",
      });
    } finally {
      if (prev === undefined) delete process.env.AUTOSK_SOCK;
      else process.env.AUTOSK_SOCK = prev;
    }
  });
});

describe("resolveAutoskdBin", () => {
  test("prefers $AUTOSKD_BIN, else process.execPath", () => {
    const prev = process.env.AUTOSKD_BIN;
    process.env.AUTOSKD_BIN = "/x/autoskd";
    try {
      expect(resolveAutoskdBin()).toBe("/x/autoskd");
      delete process.env.AUTOSKD_BIN;
      expect(resolveAutoskdBin()).toBe(process.execPath);
    } finally {
      if (prev === undefined) delete process.env.AUTOSKD_BIN;
      else process.env.AUTOSKD_BIN = prev;
    }
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
