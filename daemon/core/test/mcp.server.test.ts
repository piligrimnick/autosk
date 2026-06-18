/**
 * `autoskd mcp` stdio MCP server unit tests (claude-agent acceptance):
 *  - `tools/list` advertises the right tools (transit gated by AUTOSK_MCP_TRANSIT);
 *  - `transit` acks only (the driver, not the server, drives the transition);
 *  - `task` / `comment` shell out to a FAKE `autosk` binary (keyed by $AUTOSK_BIN)
 *    emitting canned `--json` and return the mapped result; `comment add`
 *    defaults the author to $AUTOSK_AGENT;
 *  - the missing-binary path returns a clear actionable error.
 *
 * The server code lives in `daemon/core/src/mcp/`; the `task`/`comment` execution
 * path is exercised end-to-end against the stub binary (no real daemon).
 */

import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmodSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { bunRunProcess, callTool, handleMessage, listTools } from "../src/mcp/index.ts";
import type { AutoskDetails } from "../src/mcp/index.ts";

const STUB = fileURLToPath(new URL("./fixtures/stub-autosk.ts", import.meta.url));

beforeAll(() => {
  chmodSync(STUB, 0o755); // executable so the `#!/usr/bin/env bun` shebang runs it
});

/** Runs the tool against the real shell-out path with $AUTOSK_BIN = the stub. */
function withStubBin<T>(fn: () => Promise<T>, agent?: string): Promise<T> {
  const prevBin = process.env.AUTOSK_BIN;
  const prevAgent = process.env.AUTOSK_AGENT;
  process.env.AUTOSK_BIN = STUB;
  if (agent !== undefined) process.env.AUTOSK_AGENT = agent;
  return fn().finally(() => {
    if (prevBin === undefined) delete process.env.AUTOSK_BIN;
    else process.env.AUTOSK_BIN = prevBin;
    if (prevAgent === undefined) delete process.env.AUTOSK_AGENT;
    else process.env.AUTOSK_AGENT = prevAgent;
  });
}

const realOpts = (transitEnabled: boolean) => ({ transitEnabled, run: bunRunProcess });

describe("tools/list", () => {
  test("advertises task + comment, and transit only when enabled", () => {
    const withTransit = listTools(true).map((t) => t.name);
    expect(withTransit).toEqual(["transit", "task", "comment"]);
    const withoutTransit = listTools(false).map((t) => t.name);
    expect(withoutTransit).toEqual(["task", "comment"]);
  });

  test("each tool carries a JSON-schema with the {action,args} (or {to}) shape", () => {
    const tools = Object.fromEntries(listTools(true).map((t) => [t.name, t]));
    expect((tools.transit!.inputSchema as { required: string[] }).required).toEqual(["to"]);
    expect((tools.task!.inputSchema as { required: string[] }).required).toEqual(["action"]);
    expect((tools.comment!.inputSchema as { required: string[] }).required).toEqual(["action"]);
  });
});

describe("initialize / tools/list over the JSON-RPC envelope", () => {
  test("initialize echoes protocolVersion and advertises the tools capability", async () => {
    const resp = await handleMessage(
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18" } },
      realOpts(true),
    );
    expect(resp).toMatchObject({
      jsonrpc: "2.0",
      id: 1,
      result: { protocolVersion: "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "autosk" } },
    });
  });

  test("a notification (no id) gets no response", async () => {
    const resp = await handleMessage({ jsonrpc: "2.0", method: "notifications/initialized" }, realOpts(true));
    expect(resp).toBeNull();
  });

  test("tools/list over the envelope reflects the transit gate", async () => {
    const resp = await handleMessage({ jsonrpc: "2.0", id: 2, method: "tools/list" }, realOpts(false));
    const tools = (resp!.result as { tools: { name: string }[] }).tools.map((t) => t.name);
    expect(tools).toEqual(["task", "comment"]);
  });

  test("an unknown method is a -32601", async () => {
    const resp = await handleMessage({ jsonrpc: "2.0", id: 3, method: "nope" }, realOpts(true));
    expect(resp!.error?.code).toBe(-32601);
  });
});

describe("transit — ack only", () => {
  test("acks the submitted target without performing it", async () => {
    const res = await callTool("transit", { to: "review" }, realOpts(true));
    expect(res.isError).toBe(false);
    expect(res.content[0]!.text).toContain('transition to "review" submitted');
  });

  test("is not callable when the transit gate is off", async () => {
    const res = await callTool("transit", { to: "done" }, realOpts(false));
    expect(res.isError).toBe(true);
    expect(res.content[0]!.text).toContain("unknown tool");
  });
});

describe("task — shells out to autosk --json", () => {
  test("create returns the created task", async () => {
    const res = await withStubBin(() => callTool("task", { action: "create", args: { title: "Build" } }, realOpts(true)));
    expect(res.isError).toBe(false);
    const details = res.structuredContent as Extract<AutoskDetails, { kind: "task" }>;
    expect(details.kind).toBe("task");
    expect(details.task.id).toBe("ask-created");
    expect(details.task.title).toBe("Build");
  });

  test("list returns the task array", async () => {
    const res = await withStubBin(() => callTool("task", { action: "list" }, realOpts(true)));
    const details = res.structuredContent as Extract<AutoskDetails, { kind: "tasks" }>;
    expect(details.kind).toBe("tasks");
    expect(details.tasks.map((t) => t.id)).toEqual(["ask-1", "ask-2"]);
  });

  test("show requires an id (invalid_args before any shell-out)", async () => {
    const res = await withStubBin(() => callTool("task", { action: "show", args: {} }, realOpts(true)));
    expect(res.isError).toBe(true);
    const details = res.structuredContent as Extract<AutoskDetails, { kind: "error" }>;
    expect(details.reason).toBe("invalid_args");
  });
});

describe("comment — shells out to autosk --json", () => {
  test("add defaults the author to $AUTOSK_AGENT", async () => {
    const res = await withStubBin(
      () => callTool("comment", { action: "add", args: { task_id: "ask-1", text: "note" } }, realOpts(true)),
      "review",
    );
    expect(res.isError).toBe(false);
    const details = res.structuredContent as Extract<AutoskDetails, { kind: "comment" }>;
    expect(details.comment.author).toBe("review");
    expect(details.comment.text).toBe("note");
  });

  test("add honors an explicit author override", async () => {
    const res = await withStubBin(
      () => callTool("comment", { action: "add", args: { task_id: "ask-1", text: "n", author: "alice" } }, realOpts(true)),
      "review",
    );
    const details = res.structuredContent as Extract<AutoskDetails, { kind: "comment" }>;
    expect(details.comment.author).toBe("alice");
  });

  test("list returns the comment array", async () => {
    const res = await withStubBin(() => callTool("comment", { action: "list", args: { task_id: "ask-1" } }, realOpts(true)));
    const details = res.structuredContent as Extract<AutoskDetails, { kind: "comments" }>;
    expect(details.kind).toBe("comments");
    expect(details.comments.map((c) => c.id)).toEqual(["cm-1", "cm-2"]);
  });
});

describe("missing-binary path", () => {
  test("a non-existent autosk binary returns a clear missing_binary error", async () => {
    const prev = process.env.AUTOSK_BIN;
    process.env.AUTOSK_BIN = "/nonexistent/autosk-does-not-exist";
    try {
      const res = await callTool("task", { action: "list" }, realOpts(true));
      expect(res.isError).toBe(true);
      const details = res.structuredContent as Extract<AutoskDetails, { kind: "error" }>;
      expect(details.reason).toBe("missing_binary");
      expect(details.message).toContain("not found on PATH");
    } finally {
      if (prev === undefined) delete process.env.AUTOSK_BIN;
      else process.env.AUTOSK_BIN = prev;
    }
  });
});
