/**
 * `autoskd mcp` — a minimal stdio MCP (Model Context Protocol) server.
 *
 * Speaks JSON-RPC 2.0 over stdio (one JSON object per line): `initialize` →
 * `notifications/initialized` → `tools/list` → `tools/call`. It is hand-rolled
 * (no `@modelcontextprotocol/sdk` dependency) so it bundles cleanly into the
 * compiled `autoskd` binary under `bun build --compile` with no extra runtime.
 *
 * It advertises up to three tools — `transit` (ack-only, gated by
 * `AUTOSK_MCP_TRANSIT=1`), `task`, and `comment` — which Claude Code sees as
 * `mcp__autosk__transit`, `mcp__autosk__task`, and `mcp__autosk__comment`.
 * `task` / `comment` EXECUTE for real by shelling out to `autosk … --json`;
 * `transit` only acks (the `@autosk/claude-agent` driver observes the call and
 * drives the real transition). See `tools.ts`.
 *
 * The whole reason `task` / `comment` shell out to the `autosk` CLI (instead of
 * an embedded RPC client) is that the CLI already centralizes the project
 * (`AUTOSK_CWD`) + socket (`AUTOSK_SOCK`) + author (`AUTOSK_AGENT`) resolution —
 * `@autosk/claude-agent` bakes those env vars into the `--mcp-config`, and this
 * server's spawned `autosk` inherits them.
 */

import { VERSION } from "../version.ts";
import { bunRunProcess, type RunProcess } from "./cli.ts";
import {
  callComment,
  callTask,
  callTransit,
  COMMENT_TOOL,
  TASK_TOOL,
  TRANSIT_TOOL,
  type McpTool,
  type McpToolResult,
} from "./tools.ts";

/** The MCP protocol version this server defaults to when the client omits one. */
const DEFAULT_PROTOCOL_VERSION = "2025-06-18";

/** JSON-RPC 2.0 error codes used by the server. */
const RPC_PARSE_ERROR = -32700;
const RPC_INVALID_REQUEST = -32600;
const RPC_METHOD_NOT_FOUND = -32601;
const RPC_INTERNAL_ERROR = -32603;

interface JsonRpcRequest {
  jsonrpc: "2.0";
  id?: string | number | null;
  method?: string;
  params?: unknown;
}

interface JsonRpcResponse {
  jsonrpc: "2.0";
  id: string | number | null;
  result?: unknown;
  error?: { code: number; message: string };
}

export interface McpServerOptions {
  /** Advertise + dispatch the `transit` tool (task mode). Defaults to `$AUTOSK_MCP_TRANSIT === "1"`. */
  transitEnabled?: boolean;
  /** One-shot child runner for `task` / `comment` (tests inject a fake). Defaults to `bunRunProcess`. */
  run?: RunProcess;
}

/** Whether the `transit` tool is enabled, from `$AUTOSK_MCP_TRANSIT`. */
export function transitEnabledFromEnv(): boolean {
  return process.env.AUTOSK_MCP_TRANSIT === "1";
}

/** The advertised tool list for `tools/list` (transit is conditional). */
export function listTools(transitEnabled: boolean): McpTool[] {
  const tools: McpTool[] = [];
  if (transitEnabled) tools.push(TRANSIT_TOOL);
  tools.push(TASK_TOOL, COMMENT_TOOL);
  return tools;
}

/** Dispatches a `tools/call` to its tool body. Unknown tool → error result. */
export async function callTool(
  name: string,
  args: Record<string, unknown>,
  opts: { transitEnabled: boolean; run: RunProcess },
): Promise<McpToolResult> {
  switch (name) {
    case "transit":
      if (!opts.transitEnabled) break;
      return callTransit(args);
    case "task":
      return callTask(args as { action?: unknown; args?: unknown }, opts.run);
    case "comment":
      return callComment(args as { action?: unknown; args?: unknown }, opts.run);
  }
  return {
    content: [{ type: "text", text: `unknown tool: ${name}` }],
    isError: true,
  };
}

/**
 * Handles one decoded JSON-RPC message. Returns the response to write, or `null`
 * for a notification (no `id`) that needs no reply.
 */
export async function handleMessage(
  msg: JsonRpcRequest,
  opts: { transitEnabled: boolean; run: RunProcess },
): Promise<JsonRpcResponse | null> {
  const id = msg.id ?? null;
  const method = typeof msg.method === "string" ? msg.method : "";
  const isNotification = msg.id === undefined || msg.id === null;

  // Notifications (no id) never get a response.
  if (isNotification) {
    return null;
  }

  switch (method) {
    case "initialize": {
      const params = (msg.params ?? {}) as { protocolVersion?: unknown };
      const protocolVersion =
        typeof params.protocolVersion === "string" ? params.protocolVersion : DEFAULT_PROTOCOL_VERSION;
      return {
        jsonrpc: "2.0",
        id,
        result: {
          protocolVersion,
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: "autosk", version: VERSION },
        },
      };
    }
    case "ping":
      return { jsonrpc: "2.0", id, result: {} };
    case "tools/list":
      return { jsonrpc: "2.0", id, result: { tools: listTools(opts.transitEnabled) } };
    case "tools/call": {
      const params = (msg.params ?? {}) as { name?: unknown; arguments?: unknown };
      const name = typeof params.name === "string" ? params.name : "";
      const args = (params.arguments ?? {}) as Record<string, unknown>;
      if (name === "") {
        return { jsonrpc: "2.0", id, error: { code: RPC_INVALID_REQUEST, message: "tools/call requires a tool name" } };
      }
      try {
        const result = await callTool(name, args, opts);
        return { jsonrpc: "2.0", id, result };
      } catch (e) {
        const message = e instanceof Error ? e.message : String(e);
        return { jsonrpc: "2.0", id, error: { code: RPC_INTERNAL_ERROR, message } };
      }
    }
    default:
      return { jsonrpc: "2.0", id, error: { code: RPC_METHOD_NOT_FOUND, message: `unknown method: ${method}` } };
  }
}

/**
 * Runs the stdio MCP server: read NDJSON requests from stdin, write NDJSON
 * responses to stdout, until stdin closes. Resolves when stdin is exhausted.
 */
export async function runMcpServer(options: McpServerOptions = {}): Promise<void> {
  const opts = {
    transitEnabled: options.transitEnabled ?? transitEnabledFromEnv(),
    run: options.run ?? bunRunProcess,
  };

  const write = (resp: JsonRpcResponse): void => {
    process.stdout.write(JSON.stringify(resp) + "\n");
  };

  const decoder = new TextDecoder();
  let buf = "";
  for await (const chunk of Bun.stdin.stream()) {
    buf += decoder.decode(chunk, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      const trimmed = line.trim();
      if (trimmed === "") continue;
      let msg: JsonRpcRequest;
      try {
        msg = JSON.parse(trimmed) as JsonRpcRequest;
      } catch {
        write({ jsonrpc: "2.0", id: null, error: { code: RPC_PARSE_ERROR, message: "parse error" } });
        continue;
      }
      const resp = await handleMessage(msg, opts);
      if (resp) write(resp);
    }
  }
}
